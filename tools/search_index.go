package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/internal/safefile"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

const defaultSearchEmbedding = "local/potion-code-16m-v2"

// Serialize local refresh/query pairs across registries and child agents.
// zg owns cross-process locking; Polly never removes its locks or stops a
// daemon that already owns an index.
var indexedSearchGate = make(chan struct{}, 1)

// The installed CLI's agent output has one independently scoped block per
// ranked hit. Only validated blocks are returned: cached text must obey the
// current read policy even if another application built the index.
var indexedHitHeader = regexp.MustCompile(`^#[0-9]+(?: \[[^\r\n]*\])? matchedBy=\S+ (.+):[^:\r\n]+$`)

func (t *searchFilesTool) searchIndexed(ctx context.Context, args Args, query string) (string, error) {
	if t.zvecPath == "" {
		return "", fmt.Errorf("indexed discovery requires zg on PATH and a process-enabled registry; use search_files with pattern for exact matching")
	}
	limit := args.Int("limit", 7)
	if limit < 1 || limit > 50 {
		return "", fmt.Errorf("indexed search limit must be between 1 and 50")
	}
	include := args.String("include")
	if _, err := filepath.Match(include, "probe"); err != nil {
		return "", fmt.Errorf("invalid include glob %q: %w", include, err)
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
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("query path must be a directory; use pattern to search a single file")
	}
	root := indexedSearchRoot(abs)
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
			return "", fmt.Errorf("indexed search needs to create or refresh %s: %w; use pattern for read-only exact search", indexDir, err)
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
	if err := t.registry.requireProcessSandbox("indexed search"); err != nil {
		return "", err
	}
	if active {
		sb, _, err = t.registry.newSandboxFor("search_files", nil)
		if err != nil {
			return "", err
		}
	}
	// Keep writable runtime/model data in the already-authorized workspace.
	// --mode direct never hands work to an ambient, unsandboxed daemon.
	runtimeDir := filepath.Join(indexDir, "polly")
	common := []string{"--mode", "direct", "--home", runtimeDir, "--model-cache", filepath.Join(runtimeDir, "models"), "--device", "cpu", "--no-color"}
	indexArgs := []string{"index"}
	if !exists {
		indexArgs = append(indexArgs, root)
	}
	indexArgs = append(indexArgs, "--embedding", embedding)
	indexArgs = append(indexArgs, common...)
	_, diagnostic, indexErr := runIndexedSearchCommand(ctx, sb, t.zvecPath, root, indexArgs)
	stale := false
	if indexErr != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		// A resident zg service may already maintain this index. Read the
		// existing snapshot directly; do not bypass containment by connecting
		// to that service or steal its write lease.
		if exists && strings.Contains(diagnostic, "A zvec-grep daemon owns index writes for this root") {
			stale = true
		} else {
			return "", fmt.Errorf("zvec-grep index failed: %w: %s; use pattern for exact search", indexErr, diagnostic)
		}
	}
	queryArgs := append([]string{"query", "--hybrid", query, "--fuse", "--refresh", "off", "--preview", "short", "--limit", strconv.Itoa(limit)}, common...)
	// Scope discovery before ranking. Post-filtering below independently
	// enforces scope, the existing include contract, and current read policy.
	rel, _ := filepath.Rel(root, abs)
	if glob := indexedSearchGlob(rel, include); glob != "" {
		queryArgs = append(queryArgs, "--glob", glob)
	}
	out, diagnostic, err := runIndexedSearchCommand(ctx, sb, t.zvecPath, root, queryArgs)
	if err != nil {
		return "", fmt.Errorf("zvec-grep query failed: %w: %s; use pattern for exact search", err, diagnostic)
	}
	stale = stale || strings.Contains(diagnostic, "possibly_stale")
	return filterIndexedSearch(out, root, abs, include, cfg, active, stale)
}

// Prefer the closest existing index or Git workspace. Unversioned directories
// without an ancestor index get their own index at the requested search root.
func indexedSearchRoot(path string) string {
	for dir := path; ; dir = filepath.Dir(dir) {
		if _, err := os.Lstat(filepath.Join(dir, ".zvec-grep")); err == nil {
			return dir
		}
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
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
		return "", true, fmt.Errorf("automatic indexing requires a local embedding model; existing index is unchanged, use pattern for exact search")
	}
	for _, path := range manifest.RootPaths {
		if !filepath.IsAbs(path.AbsolutePath) || !searchPathWithin(root, path.AbsolutePath) {
			return "", true, fmt.Errorf("existing zvec-grep index includes paths outside the workspace; use pattern for exact search")
		}
	}
	return "local/" + manifest.Embedding.Model, true, nil
}

func runIndexedSearchCommand(ctx context.Context, sb sandbox.Sandbox, binary, root string, args []string) (string, string, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = root
	cmd.WaitDelay = time.Second
	cleanup, err := sandbox.WrapCmdManaged(sb, cmd)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = cleanup() }()
	stdout, stderr := newBoundedBuffer(searchMaxBytes), newBoundedBuffer(4096)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err = cmd.Run()
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if stdout.Truncated() && err == nil {
		err = errors.New("indexed results exceeded the output limit; narrow the query or reduce limit")
	}
	return stdout.String(), strings.TrimSpace(stderr.String()), err
}

func searchPathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func escapeSearchGlob(path string) string {
	return strings.NewReplacer(`\`, `\\`, "*", `\*`, "?", `\?`, "[", `\[`, "]", `\]`, "{", `\{`, "}", `\}`).Replace(path)
}

func indexedSearchGlob(relativeScope, include string) string {
	// Indexed zg filters are workspace-relative; a leading slash matches
	// nothing in zg 0.2.1 even though ordinary rg accepts anchored globs.
	prefix := ""
	if relativeScope != "." {
		prefix += escapeSearchGlob(filepath.ToSlash(relativeScope)) + "/"
	}
	if include == "" {
		if relativeScope == "." {
			return ""
		}
		return prefix + "**"
	}
	// filepath.Match accepts braces literally, while zg uses rg globs.
	include = strings.NewReplacer("{", `\{`, "}", `\}`).Replace(filepath.ToSlash(include))
	if !strings.Contains(include, "/") {
		return prefix + "**/" + include
	}
	return prefix + include
}

func filterIndexedSearch(text, root, scope, include string, cfg sandbox.Config, active, stale bool) (string, error) {
	var out strings.Builder
	fmt.Fprintf(&out, "Indexed discovery (zvec-grep), workspace %s. Ranked sample, not exhaustive.\n", root)
	if stale {
		out.WriteString("freshness: possibly_stale; another daemon may own refreshes. Verify current text with read_file.\n")
	}
	allowed, sawHit, sawEmpty := false, false, false
	state := searchState{root: scope, include: include}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "#") {
			match := indexedHitHeader.FindStringSubmatch(line)
			if match == nil {
				return "", fmt.Errorf("unrecognized zvec-grep result format; use pattern for exact search")
			}
			sawHit = true
			path := filepath.Join(root, filepath.FromSlash(match[1]))
			allowed = !filepath.IsAbs(match[1]) && searchPathWithin(root, path) && searchPathWithin(scope, path) && state.includeMatch(path)
			if active && sandbox.ReadAllowed(cfg, path) != nil {
				allowed = false
			}
			// Cached hits for deleted files or symlink routes are not evidence
			// about currently readable workspace files.
			real, err := filepath.EvalSymlinks(path)
			info, statErr := os.Lstat(path)
			allowed = allowed && err == nil && real == path && statErr == nil && info.Mode().IsRegular()
			if allowed {
				out.WriteByte('\n')
			}
		} else if line == "No matches." || line == "No searchable files." {
			sawEmpty = true
		}
		if allowed {
			out.WriteString(line)
			out.WriteByte('\n')
		}
	}
	if !sawHit && !sawEmpty {
		return "", fmt.Errorf("unrecognized zvec-grep result format; use pattern for exact search")
	}
	if !strings.Contains(out.String(), "matchedBy=") {
		out.WriteString("No indexed results within the requested scope and current read policy. Try a different query or pattern for exact matching.\n")
	}
	return strings.TrimSpace(out.String()), nil
}
