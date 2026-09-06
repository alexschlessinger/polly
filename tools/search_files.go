package tools

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

// ErrSearchFilesUnavailable identifies an absent optional zg dependency.
// Session restoration can omit this tool without treating the session as broken.
var ErrSearchFilesUnavailable = errors.New("search_files requires zvec-grep (zg) on PATH")

// SearchFilesAvailable reports whether the native search tool's dependency is
// installed. Loading resolves it again so a PATH change cannot expose a tool
// whose dependency is no longer available.
func SearchFilesAvailable() bool {
	_, err := exec.LookPath("zg")
	return err == nil
}

func loadSearchFilesTool(registry *ToolRegistry) (Tool, error) {
	binary, err := exec.LookPath("zg")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSearchFilesUnavailable, err)
	}
	if err := registry.requireProcessSandbox("search_files"); err != nil {
		return nil, err
	}
	return &searchFilesTool{registry: registry, zvecPath: binary}, nil
}

const (
	searchDefaultLimit = 100
	searchMaxLimit     = 500
	// searchLineHold bounds how much of one physical line is held for regex
	// matching and display; literal matching stays exact past the hold via the
	// pager's carry search.
	searchLineHold   = 4 << 10
	searchDisplayMax = 200
	// searchMaxFiles bounds a walk over a pathologically large tree.
	searchMaxFiles = 20000
	// searchMaxBytes leaves footer room under PageMaxBytes so a result page
	// is never externalized to an artifact.
	searchMaxBytes = PageMaxBytes - 512
)

// searchFilesTool locates matching lines across files under a directory, the
// cross-file counterpart of read_file's per-file query. Matching is literal by
// default like read_file's query; results are bounded so one stray minified
// file cannot flood model context. Traversal honors the registry's base
// sandbox read policy: denied subtrees and files are skipped, so results
// cannot reveal what a sandboxed command could not read.
type searchFilesTool struct {
	NativeTool
	registry *ToolRegistry
	zvecPath string
}

// NewSearchFilesTool creates the search_files tool bound to registry's
// sandbox policy.
func NewSearchFilesTool(registry *ToolRegistry) Tool {
	t := &searchFilesTool{registry: registry}
	if registry.requireProcessSandbox("indexed search") == nil {
		t.zvecPath, _ = exec.LookPath("zg")
	}
	return t
}

func (t *searchFilesTool) GetName() string { return "search_files" }

func (t *searchFilesTool) GetSchema() *schema.ToolSchema {
	s := schema.Tool(
		"search_files",
		"Search files under a directory for matching lines, reported as path:line: text with paths relative to the search root. Matching is a case-sensitive literal by default; .git, .zvec-grep, symlinks, and binary files are skipped. Follow up with read_file; never include the path:line: prefix in edit_file's old_string.",
		schema.Params{
			"pattern": schema.S("Text to search for (single-line, case-sensitive literal unless regex is set)"),
			"path":    schema.S("Directory (or single file) to search; defaults to the current directory"),
			"include": schema.S("Optional glob filter on the file name or root-relative path, e.g. \"*.go\" or \"cmd/*.go\""),
			"regex":   schema.Bool("Treat pattern as a Go RE2 regular expression (default false)"),
			"limit":   schema.Int("Maximum matching lines (default 100, maximum 500)"),
		},
		"pattern",
	)
	if t.zvecPath != "" {
		s.Raw["description"] = "Preferred tool for finding code and documents. Use query for natural-language, conceptual, or cross-file discovery with zvec-grep; Polly automatically creates a local workspace index and refreshes it. Start here before shell searches, directory exploration, or broad file reads. Use pattern only for exact literal/RE2 occurrences and focused verification. Supply exactly one of query or pattern. Indexed results are ranked samples, not exhaustive matches; follow up with read_file only when the snippets are insufficient."
		props := s.Raw["properties"].(map[string]any)
		props["query"] = schema.S("Natural-language discovery question, optionally including known symbols; uses the local zvec-grep index")
		props["limit"] = schema.Int("Maximum results: query defaults to 7 (max 50); pattern defaults to 100 (max 500)")
		props["path"] = schema.S("Directory to search (default current directory); pattern also accepts a single file. Query reuses the nearest workspace index while keeping results inside this directory")
		delete(s.Raw, "required")
	}
	return s
}

type searchState struct {
	root          string
	pattern       string
	re            *regexp.Regexp
	include       string
	limit         int
	sandboxCfg    sandbox.Config
	sandboxActive bool

	out          strings.Builder
	matches      int
	filesScanned int
	capped       bool
	fileCapped   bool
}

func (t *searchFilesTool) Execute(ctx context.Context, raw map[string]any) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	args := Args(raw)
	if query := strings.TrimSpace(args.String("query")); query != "" {
		if args.String("pattern") != "" || args.Bool("regex") {
			return "", fmt.Errorf("supply query for indexed discovery or pattern (with optional regex) for exact matching, not both")
		}
		return t.searchIndexed(ctx, args, query)
	}
	pattern := args.String("pattern")
	if pattern == "" {
		return "", fmt.Errorf("pattern is required")
	}
	if strings.ContainsAny(pattern, "\n\r") && !args.Bool("regex") {
		return "", fmt.Errorf("pattern is matched per line and cannot contain a line break")
	}
	limit := args.Int("limit", searchDefaultLimit)
	if limit < 1 || limit > searchMaxLimit {
		return "", fmt.Errorf("limit must be between 1 and %d", searchMaxLimit)
	}
	state := &searchState{pattern: pattern, include: args.String("include"), limit: limit}
	if args.Bool("regex") {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return "", fmt.Errorf("invalid pattern: %v. Patterns use Go RE2 syntax (no backreferences or lookarounds); omit regex for literal matching", err)
		}
		state.re = re
	}
	if state.include != "" {
		if _, err := filepath.Match(state.include, "probe"); err != nil {
			return "", fmt.Errorf("invalid include glob %q: %w", state.include, err)
		}
	}
	path := strings.TrimSpace(args.String("path"))
	if path == "" {
		path = "."
	}
	abs, err := resolveLocalPath(path)
	if err != nil {
		return "", err
	}
	state.root = abs
	state.sandboxCfg, state.sandboxActive, err = t.registry.SandboxReadPolicy()
	if err != nil {
		return "", fmt.Errorf("resolve sandbox policy: %w", err)
	}
	if state.sandboxActive {
		if err := sandbox.ReadAllowed(state.sandboxCfg, abs); err != nil {
			return "", err
		}
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("search %s: %w", abs, err)
	}
	if !info.IsDir() {
		if err := state.scanFile(ctx, abs, abs); err != nil {
			return "", err
		}
		return state.render(), ctx.Err()
	}

	err = filepath.WalkDir(abs, func(path string, d fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			if path == abs {
				return walkErr
			}
			// Unreadable entries elsewhere are skipped, not fatal: a search
			// should report what it can reach.
			return nil
		}
		if d.IsDir() {
			if path == abs {
				return nil
			}
			if d.Name() == ".git" || d.Name() == ".zvec-grep" {
				return fs.SkipDir
			}
			if state.sandboxActive && sandbox.ReadAllowed(state.sandboxCfg, path) != nil {
				return fs.SkipDir
			}
			return nil
		}
		// Symlinks are never followed (loop safety, and a link could route
		// outside the tree being searched); only regular files are scanned.
		if !d.Type().IsRegular() {
			return nil
		}
		if !state.includeMatch(path) {
			return nil
		}
		if state.sandboxActive && sandbox.ReadAllowed(state.sandboxCfg, path) != nil {
			return nil
		}
		state.filesScanned++
		if state.filesScanned > searchMaxFiles {
			state.fileCapped = true
			return fs.SkipAll
		}
		rel, relErr := filepath.Rel(abs, path)
		if relErr != nil {
			rel = path
		}
		if err := state.scanFile(ctx, path, rel); err != nil {
			return err
		}
		if state.capped {
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return state.render(), nil
}

func (s *searchState) includeMatch(path string) bool {
	if s.include == "" {
		return true
	}
	if ok, _ := filepath.Match(s.include, filepath.Base(path)); ok {
		return true
	}
	rel, err := filepath.Rel(s.root, path)
	if err != nil {
		return false
	}
	ok, _ := filepath.Match(s.include, rel)
	return ok
}

// scanFile streams one file line by line, appending matches until the match,
// byte, or per-line limits bound the result. Binary files (NUL byte in the
// leading probe) are skipped.
func (s *searchState) scanFile(ctx context.Context, abs, display string) error {
	f, err := os.Open(abs)
	if err != nil {
		// A file that vanished or turned unreadable mid-walk is skipped.
		return nil
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 64<<10)
	probe, _ := br.Peek(binarySniffBytes)
	if bytes.IndexByte(probe, 0) >= 0 {
		return nil
	}
	query := s.pattern
	if s.re != nil {
		query = ""
	}
	lineNumber := 0
	for {
		line, err := readPhysicalLine(ctx, br, searchLineHold, query)
		if err != nil {
			return fmt.Errorf("scan %s: %w", abs, err)
		}
		if !line.readAny {
			return nil
		}
		lineNumber++
		matched := line.matched
		if s.re != nil {
			matched = s.re.Match(bytes.TrimSuffix(line.held, []byte("\r")))
		}
		if matched {
			entry := fmt.Sprintf("%s:%d: %s\n", display, lineNumber, searchDisplayLine(line))
			if s.out.Len()+len(entry) > searchMaxBytes {
				s.capped = true
				return nil
			}
			s.out.WriteString(entry)
			s.matches++
			if s.matches >= s.limit {
				s.capped = true
				return nil
			}
		}
		if !line.sawNewline {
			return nil
		}
	}
}

// searchDisplayLine bounds one matched line for display, reporting how many
// bytes were elided past the cut.
func searchDisplayLine(line physicalLine) string {
	display := line.held
	if len(display) > 0 && display[len(display)-1] == '\r' {
		display = display[:len(display)-1]
		line.rawLen--
	}
	if len(display) <= searchDisplayMax && int64(len(display)) == line.rawLen {
		return string(display)
	}
	cut := pageUTF8Boundary(string(display), min(len(display), searchDisplayMax))
	return fmt.Sprintf("%s [+%d bytes]", display[:cut], line.rawLen-int64(cut))
}

func (s *searchState) render() string {
	if s.matches == 0 {
		text := fmt.Sprintf("No matches for %q under %s.", s.pattern, s.root)
		if s.fileCapped {
			text += fmt.Sprintf("\n[stopped after scanning %d files; narrow the search path]", searchMaxFiles)
		}
		return CapPageText(text)
	}
	text := strings.TrimRight(s.out.String(), "\n")
	if s.capped {
		text += fmt.Sprintf("\n[results capped at %d matching lines; narrow the pattern, add include, or search a subdirectory]", s.matches)
	}
	if s.fileCapped {
		text += fmt.Sprintf("\n[stopped after scanning %d files; narrow the search path]", searchMaxFiles)
	}
	return CapPageText(text)
}
