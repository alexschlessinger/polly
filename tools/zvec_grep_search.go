package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/alexschlessinger/pollytool/internal/safefile"
	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

// ErrZvecGrepSearchUnavailable identifies an absent optional zg dependency.
// Session restoration can omit this tool without treating the session as
// broken.
var ErrZvecGrepSearchUnavailable = errors.New("zvec_grep_search requires zvec-grep (zg) on PATH")

// ZvecGrepSearchAvailable reports whether the indexed search tool's
// dependency is installed. Loading resolves it again so a PATH change cannot
// expose a tool whose dependency is no longer available.
func ZvecGrepSearchAvailable() bool {
	_, err := exec.LookPath("zg")
	return err == nil
}

func loadZvecGrepSearchTool(registry *ToolRegistry) (Tool, error) {
	binary, err := exec.LookPath("zg")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrZvecGrepSearchUnavailable, err)
	}
	if err := registry.requireProcessSandbox("zvec_grep_search"); err != nil {
		return nil, err
	}
	return &zvecGrepSearchTool{registry: registry, zvecPath: binary}, nil
}

const defaultSearchEmbedding = "local/potion-code-16m-v2"

// searchMaxBytes leaves footer room under PageMaxBytes so a result page is
// never externalized to an artifact.
const searchMaxBytes = PageMaxBytes - 512

// Request bounds, as zg's own MCP schema sets them.
const (
	zvecMaxQueryGroups = 32
	zvecMaxQueryChars  = 4000
	zvecMaxPathFilters = 128
	zvecMaxPathChars   = 1024
	zvecMaxTimeChars   = 128
	zvecMaxLimit       = 50
	zvecDefaultLimit   = 7
)

var zvecSymbolTypes = []string{"module", "class", "interface", "function", "value", "alias"}

// Serialize index creation and refresh across registries and child agents.
// zg owns cross-process locking; Polly never removes its locks or stops a
// daemon that already owns an index.
var indexedSearchGate = make(chan struct{}, 1)

// The installed CLI's agent output frames ranked hits under query-group
// lines, one independently scoped block per hit. Only validated blocks are
// returned: cached text must obey the current read policy even if another
// application built the index.
var (
	indexedHitHeader = regexp.MustCompile(`^#[0-9]+(?: \[[^\r\n]*\])? matchedBy=\S+ (.+):[^:\r\n]+$`)
	indexedGroupLine = regexp.MustCompile(`^(?:Q[0-9]+ \[|hits: )`)
)

// zvecGrepSearchTool offers zg's agent search surface as a native tool: the
// request fields of its MCP zvec_grep_search, run as sandboxed zg
// subprocesses against a workspace index Polly creates and refreshes. path
// stands in for root: the sandbox and the nearest index decide the
// workspace, and the model only narrows within it. Index file-selection and
// embedding-runtime fields are not offered; Polly keeps an existing index's
// settings and pins the local CPU model.
type zvecGrepSearchTool struct {
	NativeTool
	registry *ToolRegistry
	zvecPath string
}

// NewZvecGrepSearchTool creates the tool bound to registry's sandbox policy,
// resolving zg from PATH. Without zg or a process sandbox every search
// reports the missing dependency; LoadToolAuto refuses to load it instead.
func NewZvecGrepSearchTool(registry *ToolRegistry) Tool {
	t := &zvecGrepSearchTool{registry: registry}
	if registry.requireProcessSandbox("zvec_grep_search") == nil {
		t.zvecPath, _ = exec.LookPath("zg")
	}
	return t
}

func (t *zvecGrepSearchTool) GetName() string { return "zvec_grep_search" }

func (t *zvecGrepSearchTool) GetSchema() *schema.ToolSchema {
	symbolType := map[string]any{"type": "string", "enum": []any{"module", "class", "interface", "function", "value", "alias"}}
	return schema.Tool(
		"zvec_grep_search",
		"Search the workspace index for semantic, relational, cross-file, or multi-hop evidence such as architecture, call chains, dependencies, lifecycle, data or control flow, design rationale, and comparisons. Use it first when wording or location is unknown; use grep or rg in bash instead when exact lookup alone is sufficient. Polly creates the local index on first use and refreshes it before answering. Results are ranked samples, not exhaustive matches, with bounded source snippets and query-group metadata; treat sufficient snippets as already-read evidence and read_file only what they do not answer. Supply at least one of query, queries, fts, or vector.",
		schema.Params{
			"query":             schema.S("One primary hybrid-search group using natural-language or exact terms."),
			"queries":           schema.Strings("One or more primary hybrid-search groups. By default, each group is searched separately and retains group metadata."),
			"fts":               schema.Strings("Supplemental lexical-route groups for exact anchors such as symbols, flags, or error messages; these are retrieval routes, not hard result constraints."),
			"vector":            schema.Strings("Supplemental semantic/vector-route groups; these are retrieval routes, not hard result constraints."),
			"fuse":              schema.Bool("Collapse all primary and supplemental groups into one ranked search plan; otherwise search groups separately and retain group metadata."),
			"limit":             schema.Int("Maximum returned items per query group, or for the single fused plan (default 7, maximum 50)."),
			"path":              schema.S("Directory to search, default the current directory. Results stay inside it; the index is the nearest workspace index at or above it."),
			"globs":             schema.Strings("Ordered case-sensitive rg-style glob rules relative to path; prefix with ! to exclude. Later rules override earlier rules."),
			"insensitiveGlobs":  schema.Strings("Ordered case-insensitive rg-style glob rules relative to path; prefix with ! to exclude. Later rules override earlier rules."),
			"fileTypes":         schema.Strings("Ripgrep file type names from rg --type-list to include, such as ts, py, h, or cpp."),
			"excludedFileTypes": schema.Strings("Ripgrep file type names from rg --type-list to exclude."),
			"preferSymbol":      schema.Bool("Prefer exact indexed symbols when query names a symbol."),
			"symbolTypes":       schema.Array("Restrict indexed results to symbol types.", symbolType),
			"modifiedAfter":     schema.S("Only query files modified after this time: a date, or epoch milliseconds."),
			"modifiedBefore":    schema.S("Only query files modified before this time: a date, or epoch milliseconds."),
		},
	)
}

type zvecSearchRequest struct {
	hybrid, fts, vector                                                []string
	fuse, preferSymbol                                                 bool
	limit                                                              int
	globs, insensitiveGlobs, fileTypes, excludedFileTypes, symbolTypes []string
	modifiedAfter, modifiedBefore                                      string
}

func parseZvecSearchRequest(args Args) (zvecSearchRequest, error) {
	var r zvecSearchRequest
	var err error
	if r.hybrid, err = zvecStringList(args, "query", zvecMaxQueryGroups, zvecMaxQueryChars); err != nil {
		return r, err
	}
	more, err := zvecStringList(args, "queries", zvecMaxQueryGroups, zvecMaxQueryChars)
	if err != nil {
		return r, err
	}
	if r.hybrid = append(r.hybrid, more...); len(r.hybrid) > zvecMaxQueryGroups {
		return r, fmt.Errorf("query and queries accept at most %d groups together", zvecMaxQueryGroups)
	}
	if r.fts, err = zvecStringList(args, "fts", zvecMaxQueryGroups, zvecMaxQueryChars); err != nil {
		return r, err
	}
	if r.vector, err = zvecStringList(args, "vector", zvecMaxQueryGroups, zvecMaxQueryChars); err != nil {
		return r, err
	}
	if len(r.hybrid)+len(r.fts)+len(r.vector) == 0 {
		return r, errors.New("zvec_grep_search requires query, queries, fts, or vector")
	}
	r.fuse, r.preferSymbol = args.Bool("fuse"), args.Bool("preferSymbol")
	if r.limit = args.Int("limit", zvecDefaultLimit); r.limit < 1 || r.limit > zvecMaxLimit {
		return r, fmt.Errorf("limit must be between 1 and %d", zvecMaxLimit)
	}
	if r.globs, err = zvecStringList(args, "globs", zvecMaxPathFilters, zvecMaxPathChars); err != nil {
		return r, err
	}
	if r.insensitiveGlobs, err = zvecStringList(args, "insensitiveGlobs", zvecMaxPathFilters, zvecMaxPathChars); err != nil {
		return r, err
	}
	for _, key := range []string{"fileTypes", "excludedFileTypes"} {
		types, err := zvecStringList(args, key, zvecMaxPathFilters, 64)
		if err != nil {
			return r, err
		}
		for _, name := range types {
			if strings.HasPrefix(name, "-") || strings.ContainsAny(name, " \t") {
				return r, fmt.Errorf("%s entry %q is not a ripgrep file type name", key, name)
			}
		}
		if key == "fileTypes" {
			r.fileTypes = types
		} else {
			r.excludedFileTypes = types
		}
	}
	if r.symbolTypes, err = zvecStringList(args, "symbolTypes", len(zvecSymbolTypes), 32); err != nil {
		return r, err
	}
	for _, name := range r.symbolTypes {
		if !slicesContains(zvecSymbolTypes, name) {
			return r, fmt.Errorf("symbolTypes accepts %s", strings.Join(zvecSymbolTypes, ", "))
		}
	}
	if r.modifiedAfter, err = zvecTime(args, "modifiedAfter"); err != nil {
		return r, err
	}
	if r.modifiedBefore, err = zvecTime(args, "modifiedBefore"); err != nil {
		return r, err
	}
	return r, nil
}

func slicesContains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// zvecStringList reads a string or a list of strings, as zg's schema accepts
// either, trimming entries and dropping empty ones.
func zvecStringList(args Args, key string, maxItems, maxChars int) ([]string, error) {
	var items []string
	switch v := args[key].(type) {
	case nil:
		return nil, nil
	case string:
		items = []string{v}
	case []any, []string:
		items = args.StringSlice(key)
	default:
		return nil, fmt.Errorf("%s must be a string or a list of strings", key)
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if len(item) > maxChars {
			return nil, fmt.Errorf("%s entries are limited to %d characters", key, maxChars)
		}
		out = append(out, item)
	}
	if len(out) > maxItems {
		return nil, fmt.Errorf("%s accepts at most %d entries", key, maxItems)
	}
	return out, nil
}

// zvecTime reads a modified-time bound as zg accepts it: a date string or
// epoch milliseconds, which JSON delivers as a number.
func zvecTime(args Args, key string) (string, error) {
	switch v := args[key].(type) {
	case nil:
		return "", nil
	case string:
		v = strings.TrimSpace(v)
		if v == "" {
			return "", nil
		}
		if len(v) > zvecMaxTimeChars || strings.HasPrefix(v, "-") {
			return "", fmt.Errorf("%s must be a date or epoch milliseconds", key)
		}
		return v, nil
	default:
		f := args.Float(key, math.NaN())
		if math.IsNaN(f) || f < 0 || f != math.Trunc(f) {
			return "", fmt.Errorf("%s must be a date or non-negative epoch milliseconds", key)
		}
		return strconv.FormatInt(int64(f), 10), nil
	}
}

// commandArgs renders the request as zg query flags. globs and
// insensitiveGlobs arrive already scoped to the search directory.
func (r zvecSearchRequest) commandArgs(refresh string, globs, insensitiveGlobs []string) []string {
	args := []string{"query"}
	for _, q := range r.hybrid {
		args = append(args, "--hybrid", q)
	}
	for _, q := range r.fts {
		args = append(args, "--fts", q)
	}
	for _, q := range r.vector {
		args = append(args, "--vector", q)
	}
	if r.fuse {
		args = append(args, "--fuse")
	}
	args = append(args, "--refresh", refresh, "--preview", "short", "--limit", strconv.Itoa(r.limit))
	if r.preferSymbol {
		args = append(args, "--prefer-symbol")
	}
	for _, name := range r.symbolTypes {
		args = append(args, "--symbol-type", name)
	}
	if r.modifiedAfter != "" {
		args = append(args, "--modified-after", r.modifiedAfter)
	}
	if r.modifiedBefore != "" {
		args = append(args, "--modified-before", r.modifiedBefore)
	}
	for _, name := range r.fileTypes {
		args = append(args, "--type", name)
	}
	for _, name := range r.excludedFileTypes {
		args = append(args, "--type-not", name)
	}
	for _, g := range globs {
		args = append(args, "--glob", g)
	}
	for _, g := range insensitiveGlobs {
		args = append(args, "--iglob", g)
	}
	return args
}

func (t *zvecGrepSearchTool) Execute(ctx context.Context, raw map[string]any) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	args := Args(raw)
	req, err := parseZvecSearchRequest(args)
	if err != nil {
		return "", err
	}
	if t.zvecPath == "" {
		return "", fmt.Errorf("zvec_grep_search requires zg on PATH and a process-enabled registry; use grep or rg in bash for exact matching")
	}
	path := strings.TrimSpace(args.String("path"))
	if path == "" {
		path = "."
	}
	abs, err := resolveLocalPath(path)
	if err != nil {
		return "", err
	}
	cfg, active, err := t.registry.SandboxReadPolicy()
	if err != nil {
		return "", err
	}
	if active {
		if err := sandbox.ReadAllowed(cfg, abs); err != nil {
			return "", err
		}
	}
	var info os.FileInfo
	if abs, err = filepath.EvalSymlinks(abs); err == nil {
		info, err = os.Stat(abs)
	}
	if errors.Is(err, fs.ErrNotExist) {
		// The OS phrasing differs per platform; keep the message stable.
		return "", fmt.Errorf("path does not exist: %s", abs)
	}
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("path must be a directory; use read_file or grep for a single file")
	}
	// The default CLI sandbox grants writes under cwd only, so an enclosing
	// checkout's .zvec-grep may be out of reach; index the writable search
	// root instead of failing every query launched from a subdirectory.
	var indexable func(string) bool
	if active {
		indexable = func(dir string) bool { return sandbox.WriteAllowed(cfg, filepath.Join(dir, ".zvec-grep")) == nil }
	}
	root := indexedSearchRoot(abs, indexable)
	indexDir := filepath.Join(root, ".zvec-grep")
	if active {
		if err := sandbox.ReadAllowed(cfg, root); err != nil {
			return "", err
		}
		if err := sandbox.ReadAllowed(cfg, indexDir); err != nil {
			return "", err
		}
		if err := sandbox.ReadAllowed(cfg, filepath.Join(indexDir, "manifest.json")); err != nil {
			return "", err
		}
		if err := sandbox.WriteAllowed(cfg, indexDir); err != nil {
			return "", fmt.Errorf("indexed search needs to create or refresh %s: %w; use grep or rg in bash for read-only exact search", indexDir, err)
		}
	}
	// Do not let a linked index silently change the workspace zg searches.
	if info, err := os.Lstat(indexDir); err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return "", fmt.Errorf("workspace index must be a real directory: %s", indexDir)
	} else if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	select {
	case indexedSearchGate <- struct{}{}:
		defer func() { <-indexedSearchGate }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	embedding, exists, err := indexedSearchEmbedding(indexDir, root)
	if err != nil {
		return "", err
	}
	var sb sandbox.Sandbox
	if err := t.registry.requireProcessSandbox("zvec_grep_search"); err != nil {
		return "", err
	}
	if active {
		sb, _, err = t.registry.newSandboxFor("zvec_grep_search", nil)
		if err != nil {
			return "", err
		}
	}
	// Keep writable runtime/model data in the already-authorized workspace.
	// --mode direct never hands work to an ambient, unsandboxed daemon.
	runtimeDir := filepath.Join(indexDir, "polly")
	common := []string{"--mode", "direct", "--home", runtimeDir, "--model-cache", filepath.Join(runtimeDir, "models"), "--device", "cpu", "--no-color"}
	if !exists {
		indexArgs := append([]string{"index", root, "--embedding", embedding}, common...)
		if _, diagnostic, _, err := runIndexedSearchCommand(ctx, sb, t.zvecPath, root, indexArgs); err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			return "", fmt.Errorf("zvec-grep index failed: %w: %s; use grep or rg in bash for exact search", err, diagnostic)
		}
	}
	// Scope discovery before ranking. Post-filtering below independently
	// enforces scope and the current read policy.
	rel, _ := filepath.Rel(root, abs)
	globs, insensitiveGlobs := zvecScopedGlobs(rel, req.globs, req.insensitiveGlobs)
	// The refresh rides the query process (--refresh wait): one zg launch
	// scans, embeds what changed, and answers, instead of a separate index
	// step. A refresh with nothing to embed costs a scan; one with changes
	// also loads the model, about 2.5s in a fresh process however many
	// files changed.
	runQuery := func(refresh string) (string, string, bool, error) {
		return runIndexedSearchCommand(ctx, sb, t.zvecPath, root, append(req.commandArgs(refresh, globs, insensitiveGlobs), common...))
	}
	stale := false
	out, diagnostic, truncated, err := runQuery("wait")
	if err != nil && indexedSearchDaemonOwned(diagnostic) {
		// A resident zg service maintains this index. Read its snapshot
		// directly rather than connecting to the service (outside the
		// sandbox) or stealing its write lease.
		stale = true
		out, diagnostic, truncated, err = runQuery("off")
	}
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("zvec-grep query failed: %w: %s; use grep or rg in bash for exact search", err, diagnostic)
	}
	stale = stale || strings.Contains(diagnostic, "possibly_stale")
	return filterIndexedSearch(out, root, abs, cfg, active, stale, truncated)
}

// indexedSearchDaemonOwned reports that direct-mode zg refused to refresh
// because a daemon holds the root's write lease. The error code covers both
// wordings zg uses (an owning daemon and a lease mid-creation).
func indexedSearchDaemonOwned(diagnostic string) bool {
	return strings.Contains(diagnostic, "ZVEC_GREP.ENGINE.DAEMON_LEASE_ACTIVE") ||
		strings.Contains(diagnostic, "daemon owns index writes for this root")
}

// Prefer the closest existing index or Git workspace that indexable accepts
// (nil accepts any). Unversioned directories without an ancestor index get
// their own index at the requested search root.
// Only a manifest marks an index: zg's global runtime home is also named
// .zvec-grep, and finding it must not make $HOME the workspace. Discovery
// also stops short of the home directory and the filesystem root: the
// sandbox rejects both as workspaces, and --nosandbox must not index them
// wholesale (a dotfiles checkout puts .git at $HOME).
func indexedSearchRoot(path string, indexable func(dir string) bool) string {
	home, _ := os.UserHomeDir()
	if real, err := filepath.EvalSymlinks(home); err == nil {
		home = real
	}
	for dir := path; ; dir = filepath.Dir(dir) {
		if dir != path && (dir == home || filepath.Dir(dir) == dir) {
			return path
		}
		marked := false
		if info, err := os.Lstat(filepath.Join(dir, ".zvec-grep", "manifest.json")); err == nil && info.Mode().IsRegular() {
			marked = true
		} else if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			marked = true
		}
		if marked && (indexable == nil || indexable(dir)) {
			return dir
		}
		if filepath.Dir(dir) == dir {
			return path
		}
	}
}

func indexedSearchEmbedding(indexDir, root string) (string, bool, error) {
	path := filepath.Join(indexDir, "manifest.json")
	f, err := safefile.OpenRegular(path, os.O_RDONLY, 0)
	if os.IsNotExist(err) {
		return defaultSearchEmbedding, false, nil
	}
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	var manifest struct {
		Embedding struct{ Provider, Model string }
		RootPaths []struct{ AbsolutePath string }
	}
	if err := json.NewDecoder(io.LimitReader(f, 1<<20)).Decode(&manifest); err != nil {
		return "", true, fmt.Errorf("read zvec-grep manifest: %w", err)
	}
	if manifest.Embedding.Provider != "local" || manifest.Embedding.Model == "" {
		return "", true, fmt.Errorf("automatic indexing requires a local embedding model; existing index is unchanged, use grep or rg in bash for exact search")
	}
	for _, path := range manifest.RootPaths {
		recorded := path.AbsolutePath
		if !filepath.IsAbs(recorded) {
			return "", true, fmt.Errorf("existing zvec-grep index includes paths outside the workspace; use grep or rg in bash for exact search")
		}
		// zg records the root as spelled at index creation (/tmp/proj, a
		// linked ~/code), while root arrives symlink-resolved; compare
		// filesystem identities so a valid index is not refused forever.
		if real, err := filepath.EvalSymlinks(recorded); err == nil {
			recorded = real
		}
		if !sandbox.PathWithin(recorded, root) {
			return "", true, fmt.Errorf("existing zvec-grep index includes paths outside the workspace; use grep or rg in bash for exact search")
		}
	}
	return "local/" + manifest.Embedding.Model, true, nil
}

func runIndexedSearchCommand(ctx context.Context, sb sandbox.Sandbox, binary, root string, args []string) (stdout, stderr string, truncated bool, err error) {
	out, errOut := newBoundedBuffer(searchMaxBytes), newBoundedBuffer(4096)
	_, err = runFiniteCommand(ctx, sb, finiteCommand{
		name: binary, args: args, dir: root, stdout: out, stderr: errOut,
	})
	return out.String(), strings.TrimSpace(errOut.String()), out.Truncated(), err
}

func escapeSearchGlob(path string) string {
	return strings.NewReplacer(`\`, `\\`, "*", `\*`, "?", `\?`, "[", `\[`, "]", `\]`, "{", `\{`, "}", `\}`).Replace(path)
}

// zvecScopedGlobs rewrites the model's rg-style rules relative to the search
// directory. Indexed zg filters are workspace-relative, and a leading slash
// matches nothing in zg 0.2.1. A rule without a slash matches file names
// anywhere under the scope; one with a slash is anchored at the scope. With
// no positive rule of its own the scope becomes the whitelist, placed first
// so later exclusions still override it; with one, the prefixed rules
// already confine matching to the scope.
func zvecScopedGlobs(relativeScope string, globs, insensitiveGlobs []string) ([]string, []string) {
	if relativeScope == "." {
		return globs, insensitiveGlobs
	}
	prefix := escapeSearchGlob(filepath.ToSlash(relativeScope)) + "/"
	positive := false
	rewrite := func(rules []string) []string {
		scoped := make([]string, 0, len(rules)+1)
		for _, rule := range rules {
			body, negated := strings.CutPrefix(rule, "!")
			positive = positive || !negated
			body = strings.TrimPrefix(body, "/")
			if !strings.Contains(body, "/") {
				body = "**/" + body
			}
			if negated {
				scoped = append(scoped, "!"+prefix+body)
			} else {
				scoped = append(scoped, prefix+body)
			}
		}
		return scoped
	}
	scoped, scopedInsensitive := rewrite(globs), rewrite(insensitiveGlobs)
	if !positive {
		scoped = append([]string{prefix + "**"}, scoped...)
	}
	return scoped, scopedInsensitive
}

func filterIndexedSearch(text, root, scope string, cfg sandbox.Config, active, stale, truncated bool) (string, error) {
	var out strings.Builder
	fmt.Fprintf(&out, "Indexed discovery (zvec-grep), workspace %s. Ranked sample, not exhaustive.\n", root)
	if stale {
		out.WriteString("freshness: possibly_stale; another daemon may own refreshes. Verify current text with read_file.\n")
	}
	allowed, sawHit, sawEmpty := false, false, false
	// A truncated stream ends inside its final hit; that hit is dropped
	// rather than shown incomplete.
	lastHitStart, lastBlockIsAllowedHit := 0, false
	for _, line := range strings.Split(text, "\n") {
		switch {
		case strings.HasPrefix(line, "#"):
			match := indexedHitHeader.FindStringSubmatch(line)
			if match == nil {
				return "", fmt.Errorf("unrecognized zvec-grep result format; use grep or rg in bash for exact search")
			}
			sawHit = true
			path := filepath.Join(root, filepath.FromSlash(match[1]))
			allowed = !filepath.IsAbs(match[1]) && sandbox.PathWithin(path, root) && sandbox.PathWithin(path, scope)
			if active && sandbox.ReadAllowed(cfg, path) != nil {
				allowed = false
			}
			// Cached hits for deleted files or symlink routes are not evidence
			// about currently readable workspace files.
			real, err := filepath.EvalSymlinks(path)
			info, statErr := os.Lstat(path)
			allowed = allowed && err == nil && real == path && statErr == nil && info.Mode().IsRegular()
			lastBlockIsAllowedHit = allowed
			if allowed {
				out.WriteByte('\n')
				lastHitStart = out.Len()
			}
		case indexedGroupLine.MatchString(line):
			// Query-group lines carry no file content; they frame the hits
			// that follow, even when every one of them is filtered out.
			allowed, lastBlockIsAllowedHit = false, false
			if strings.HasPrefix(line, "Q") {
				out.WriteByte('\n')
			}
			out.WriteString(line)
			out.WriteByte('\n')
			continue
		case line == "No matches." || line == "No searchable files.":
			sawEmpty = true
		}
		if allowed {
			out.WriteString(line)
			out.WriteByte('\n')
		}
	}
	if !sawHit && !sawEmpty {
		return "", fmt.Errorf("unrecognized zvec-grep result format; use grep or rg in bash for exact search")
	}
	result := out.String()
	if truncated {
		if lastBlockIsAllowedHit {
			result = result[:lastHitStart]
		}
		result += fmt.Sprintf("[output truncated at %d bytes; lower limit or add globs]\n", searchMaxBytes)
	}
	if !strings.Contains(result, "matchedBy=") {
		result += "No indexed results within the requested scope and current read policy. Try a different query or grep or rg in bash for exact matching.\n"
	}
	return strings.TrimSpace(result), nil
}
