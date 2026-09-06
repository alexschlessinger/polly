package tools

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

func installSearchDependencyForTest(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	name := "zg"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("test dependency; must not execute"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestZvecGrepSearchLoadRequiresZG(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	registry := NewToolRegistry(nil, WithUnsafeNoSandbox())
	if ZvecGrepSearchAvailable() {
		t.Fatal("zg unexpectedly available")
	}
	if _, err := registry.LoadToolAuto("zvec_grep_search"); !errors.Is(err, ErrZvecGrepSearchUnavailable) {
		t.Fatalf("missing dependency error = %v", err)
	}
	if _, ok := registry.Get("zvec_grep_search"); ok || len(registry.GetSchemas()) != 0 {
		t.Fatal("zvec_grep_search loaded without zg")
	}
	installSearchDependencyForTest(t)
	if !ZvecGrepSearchAvailable() {
		t.Fatal("installed zg was not discovered")
	}
	if _, err := registry.LoadToolAuto("zvec_grep_search"); err != nil {
		t.Fatalf("load with zg installed: %v", err)
	}
	tool, ok := registry.Get("zvec_grep_search")
	if !ok {
		t.Fatal("zvec_grep_search not registered")
	}
	props := tool.GetSchema().Properties()
	for _, name := range []string{"query", "queries", "fts", "vector", "fuse", "limit", "path", "globs", "insensitiveGlobs", "fileTypes", "excludedFileTypes", "preferSymbol", "symbolTypes", "modifiedAfter", "modifiedBefore"} {
		if props[name] == nil {
			t.Fatalf("schema lacks zg's %s field", name)
		}
	}
	for _, name := range []string{"root", "apiKey", "device", "hidden", "noIgnore", "follow", "freshness", "autoUpdate", "trace", "pattern"} {
		if props[name] != nil {
			t.Fatalf("schema offers %s, which Polly decides itself", name)
		}
	}
}

func TestFileToolSchemasMentionZvecGrepSearchOnlyWhenLoaded(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	registry := NewToolRegistry(nil, WithUnsafeNoSandbox())
	for _, name := range []string{"read_file", "list_dir"} {
		if _, err := registry.LoadToolAuto(name); err != nil {
			t.Fatalf("load %s: %v", name, err)
		}
	}
	// Through GetSchemas: the descriptions consult the registry, which must
	// not deadlock on its own lock.
	descriptions := func() map[string]string {
		out := map[string]string{}
		for _, s := range registry.GetSchemas() {
			out[s.Title()] = s.Description()
		}
		return out
	}
	for name, desc := range descriptions() {
		if strings.Contains(desc, "zvec_grep_search") {
			t.Fatalf("%s steers toward absent zvec_grep_search: %q", name, desc)
		}
	}
	installSearchDependencyForTest(t)
	if _, err := registry.LoadToolAuto("zvec_grep_search"); err != nil {
		t.Fatalf("load zvec_grep_search: %v", err)
	}
	for _, name := range []string{"read_file", "list_dir"} {
		if desc := descriptions()[name]; !strings.Contains(desc, "zvec_grep_search") {
			t.Fatalf("%s does not steer toward loaded zvec_grep_search: %q", name, desc)
		}
	}
}

func fakeSearchZG(t *testing.T, body string) string {
	t.Helper()
	skipIfWindows(t)
	path := filepath.Join(t.TempDir(), "zg")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

type searchCommandSandbox struct{ calls int }

func (s *searchCommandSandbox) Wrap(cmd *exec.Cmd) error {
	s.calls++
	cmd.Env = append(os.Environ(), "POLLY_TEST_SEARCH_WRAPPED=1")
	return nil
}

// indexedWorkspace prepares a root with a local-model manifest so a query
// skips creation, and returns the root and a reader of the fake's call log.
func indexedWorkspace(t *testing.T) (string, func() []string) {
	t.Helper()
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	writeTestFile(t, root, "main.go", "package main\n")
	if err := os.Mkdir(filepath.Join(root, ".zvec-grep"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(root, ".zvec-grep"), "manifest.json", `{"embedding":{"provider":"local","model":"potion-code-16m-v2"}}`)
	return root, func() []string {
		data, _ := os.ReadFile(filepath.Join(root, "calls"))
		return strings.Split(strings.TrimSpace(string(data)), "\n")
	}
}

func TestZvecGrepSearchCreatesRefreshesAndContainsCommands(t *testing.T) {
	skipIfWindows(t)
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	writeTestFile(t, root, "main.go", "package main\n")
	sb := &searchCommandSandbox{}
	registry := NewToolRegistry(nil, WithSandboxFactory(func(sandbox.Config) (sandbox.Sandbox, error) { return sb, nil }, sandbox.Config{WritablePaths: []string{root}}))
	tool := NewZvecGrepSearchTool(registry).(*zvecGrepSearchTool)
	tool.zvecPath = fakeSearchZG(t, `
test "$POLLY_TEST_SEARCH_WRAPPED" = 1
printf '%s\n' "$*" >> calls
case "$1" in
index)
  /bin/mkdir -p .zvec-grep
  if [ ! -f .zvec-grep/manifest.json ]; then
    /bin/cat > .zvec-grep/manifest.json <<'EOF2'
{"embedding":{"provider":"local","model":"potion-code-16m-v2"},"rootPaths":[]}
EOF2
  fi
  ;;
query)
  test "$2" = --hybrid
  printf '%s' "$3" > query-value
  printf 'Q1 [primary]: q\nhits: 1\n\n#1 matchedBy=fts+vector main.go:1-1\nsource:\n1\tpackage main\n'
  ;;
*) exit 1;;
esac
`)
	query := "find `touch injected` $(touch injected) --drop"
	for range 2 {
		out, err := tool.Execute(context.Background(), map[string]any{"query": query, "path": root})
		if err != nil || !strings.Contains(out, "package main") || !strings.Contains(out, "hits: 1") {
			t.Fatalf("indexed search = %q, %v", out, err)
		}
	}
	if sb.calls != 3 {
		t.Fatalf("wrapped %d commands, want one index build and two queries", sb.calls)
	}
	got, err := os.ReadFile(filepath.Join(root, "query-value"))
	if err != nil || string(got) != query {
		t.Fatalf("query argument = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(root, "injected")); !os.IsNotExist(err) {
		t.Fatal("query was evaluated as shell code")
	}
	calls, _ := os.ReadFile(filepath.Join(root, "calls"))
	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "index "+root+" --embedding "+defaultSearchEmbedding) {
		t.Fatalf("first call should build the index at root:\n%s", calls)
	}
	// The refresh rides each query process instead of a separate index step.
	for _, line := range lines[1:] {
		if !strings.HasPrefix(line, "query ") || !strings.Contains(line, "--refresh wait") {
			t.Fatalf("queries should refresh in-process:\n%s", calls)
		}
	}
	for _, line := range lines {
		if !strings.Contains(line, "--mode direct") || !strings.Contains(line, "--model-cache "+filepath.Join(root, ".zvec-grep", "polly", "models")) {
			t.Fatalf("missing direct mode or workspace cache: %s", line)
		}
	}
}

func TestZvecGrepSearchRendersRequestAsZGFlags(t *testing.T) {
	root, calls := indexedWorkspace(t)
	tool := NewZvecGrepSearchTool(NewToolRegistry(nil, WithUnsafeNoSandbox())).(*zvecGrepSearchTool)
	tool.zvecPath = fakeSearchZG(t, `
printf '%s\n' "$*" >> calls
echo 'No matches.'
`)
	_, err := tool.Execute(context.Background(), map[string]any{
		"query":             "how are sessions leased",
		"queries":           "tab lifecycle",
		"fts":               []any{"ReadAllowed", "ErrSessionInUse"},
		"vector":            "approval prompts",
		"fuse":              true,
		"limit":             3,
		"globs":             []any{"*.go", "!vendor/**"},
		"insensitiveGlobs":  "*.MD",
		"fileTypes":         "go",
		"excludedFileTypes": []any{"md"},
		"preferSymbol":      true,
		"symbolTypes":       []any{"function", "value"},
		"modifiedAfter":     float64(1700000000000),
		"modifiedBefore":    "2026-09-01",
		"path":              root,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "query --hybrid how are sessions leased --hybrid tab lifecycle --fts ReadAllowed --fts ErrSessionInUse --vector approval prompts --fuse --refresh wait --preview short --limit 3 --prefer-symbol --symbol-type function --symbol-type value --modified-after 1700000000000 --modified-before 2026-09-01 --type go --type-not md --glob *.go --glob !vendor/** --iglob *.MD --mode direct"
	if got := calls(); len(got) != 1 || !strings.HasPrefix(got[0], want) {
		t.Fatalf("zg flags:\n got %s\nwant %s…", strings.Join(got, "\n"), want)
	}
}

func TestZvecScopedGlobs(t *testing.T) {
	for _, tc := range []struct {
		scope           string
		globs, want     []string
		insensitive     []string
		wantInsensitive []string
	}{
		{scope: ".", globs: []string{"*.go", "!vendor/**"}, want: []string{"*.go", "!vendor/**"}},
		{scope: "src", want: []string{"src/**"}},
		{scope: "src", globs: []string{"*.go"}, want: []string{"src/**/*.go"}},
		{scope: "src", globs: []string{"cmd/*.go", "/lib/**"}, want: []string{"src/cmd/*.go", "src/lib/**"}},
		{scope: "src", globs: []string{"!*.md"}, want: []string{"src/**", "!src/**/*.md"}},
		{scope: "src", globs: []string{"*.go", "!*_test.go"}, want: []string{"src/**/*.go", "!src/**/*_test.go"}},
		{scope: "a[1]", globs: []string{"*.go"}, want: []string{`a\[1\]/**/*.go`}},
		{scope: "src", insensitive: []string{"*.md"}, want: nil, wantInsensitive: []string{"src/**/*.md"}},
		{scope: "src", globs: []string{"!*.md"}, insensitive: []string{"readme*"}, want: []string{"!src/**/*.md"}, wantInsensitive: []string{"src/**/readme*"}},
	} {
		got, gotInsensitive := zvecScopedGlobs(tc.scope, tc.globs, tc.insensitive)
		if strings.Join(got, " ") != strings.Join(tc.want, " ") || strings.Join(gotInsensitive, " ") != strings.Join(tc.wantInsensitive, " ") {
			t.Errorf("scope %q globs %v iglobs %v = %v %v, want %v %v", tc.scope, tc.globs, tc.insensitive, got, gotInsensitive, tc.want, tc.wantInsensitive)
		}
	}
}

func TestZvecGrepSearchRootSkipsRuntimeStateAndBroadAncestors(t *testing.T) {
	base := t.TempDir()
	base, _ = filepath.EvalSymlinks(base)
	// zg's own runtime home is named .zvec-grep but holds no workspace manifest.
	for _, dir := range []string{".zvec-grep/daemon", ".zvec-grep/models", "Documents/notes"} {
		if err := os.MkdirAll(filepath.Join(base, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	notes := filepath.Join(base, "Documents", "notes")
	if got := indexedSearchRoot(notes, nil); got != notes {
		t.Fatalf("runtime state directory selected as workspace: %s", got)
	}
	calls := filepath.Join(t.TempDir(), "calls")
	tool := NewZvecGrepSearchTool(NewToolRegistry(nil, WithUnsafeNoSandbox())).(*zvecGrepSearchTool)
	tool.zvecPath = fakeSearchZG(t, `
printf '%s\t%s\n' "$PWD" "$*" >> '`+calls+`'
if [ "$1" = index ]; then exit 0; fi
echo 'No matches.'
`)
	if _, err := tool.Execute(context.Background(), map[string]any{"query": "meeting notes", "path": notes}); err != nil {
		t.Fatalf("query beside runtime state: %v", err)
	}
	recorded, _ := os.ReadFile(calls)
	if first := strings.SplitN(string(recorded), "\n", 2)[0]; !strings.HasPrefix(first, notes+"\tindex "+notes+" ") {
		t.Fatalf("index must build the requested directory in that directory, got: %s", first)
	}

	writeTestFile(t, filepath.Join(base, ".zvec-grep"), "manifest.json", `{}`)
	if got := indexedSearchRoot(notes, nil); got != base {
		t.Fatalf("manifest-bearing ancestor not preferred: %s", got)
	}

	// A dotfiles checkout versions $HOME itself; discovery must not climb
	// into it, while an explicitly requested home search still names it.
	home := t.TempDir()
	home, _ = filepath.EvalSymlinks(home)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, dir := range []string{".git", "notes"} {
		if err := os.Mkdir(filepath.Join(home, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if got := indexedSearchRoot(filepath.Join(home, "notes"), nil); got != filepath.Join(home, "notes") {
		t.Fatalf("versioned home directory selected as workspace: %s", got)
	}
	if got := indexedSearchRoot(home, nil); got != home {
		t.Fatalf("explicit home search root = %s", got)
	}
}

func TestZvecGrepSearchRootStaysInsideWritablePolicy(t *testing.T) {
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	sub := filepath.Join(root, "cmd")
	for _, dir := range []string{".git", "cmd"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if got := indexedSearchRoot(sub, func(dir string) bool { return dir == sub }); got != sub {
		t.Fatalf("unwritable checkout selected as workspace: %s", got)
	}
	if got := indexedSearchRoot(sub, func(string) bool { return true }); got != root {
		t.Fatalf("writable checkout not preferred: %s", got)
	}
	// The default CLI sandbox: polly launched in cmd/ may write only there.
	// Temp directories are always writable, so the checkout's index is
	// denied explicitly to model the cwd-only grant.
	calls := filepath.Join(t.TempDir(), "calls")
	policy := sandbox.Config{WritablePaths: []string{sub}, DenyWritePaths: []string{filepath.Join(root, ".zvec-grep")}}
	tool := NewZvecGrepSearchTool(stubSandboxRegistry(t, policy)).(*zvecGrepSearchTool)
	tool.zvecPath = fakeSearchZG(t, `
printf '%s\t%s\n' "$PWD" "$*" >> '`+calls+`'
if [ "$1" = index ]; then exit 0; fi
echo 'No matches.'
`)
	if _, err := tool.Execute(context.Background(), map[string]any{"query": "entry point", "path": sub}); err != nil {
		t.Fatalf("query from a writable subdirectory: %v", err)
	}
	recorded, _ := os.ReadFile(calls)
	first := strings.SplitN(string(recorded), "\n", 2)[0]
	if !strings.HasPrefix(first, sub+"\tindex "+sub+" ") || !strings.Contains(first, "--model-cache "+filepath.Join(sub, ".zvec-grep", "polly", "models")) {
		t.Fatalf("index must live in the writable search root, got: %s", first)
	}
}

func TestZvecGrepSearchDaemonLeaseUsesSnapshot(t *testing.T) {
	root, calls := indexedWorkspace(t)
	tool := NewZvecGrepSearchTool(NewToolRegistry(nil, WithUnsafeNoSandbox())).(*zvecGrepSearchTool)
	tool.zvecPath = fakeSearchZG(t, `
printf '%s\n' "$*" >> calls
case "$*" in
*"--refresh wait"*)
  echo 'Error: A zvec-grep daemon owns index writes for this root' >&2
  echo 'Code: ZVEC_GREP.ENGINE.DAEMON_LEASE_ACTIVE' >&2
  exit 1;;
esac
printf '#1 matchedBy=fts main.go:1-1\n1\tpackage main\n'
`)
	out, err := tool.Execute(context.Background(), map[string]any{"query": "entry point", "path": root})
	if err != nil || !strings.Contains(out, "possibly_stale") || !strings.Contains(out, "package main") {
		t.Fatalf("snapshot = %q, %v", out, err)
	}
	if got := calls(); len(got) != 2 || !strings.Contains(got[0], "--refresh wait") || !strings.Contains(got[1], "--refresh off") {
		t.Fatalf("refused refresh should fall back to the snapshot:\n%s", strings.Join(got, "\n"))
	}
}

func TestZvecGrepSearchScopesBeforeRanking(t *testing.T) {
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	for _, dir := range []string{".git", "src"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(t, filepath.Join(root, "src"), "main.go", "package main\n")
	tool := NewZvecGrepSearchTool(NewToolRegistry(nil, WithUnsafeNoSandbox())).(*zvecGrepSearchTool)
	tool.zvecPath = fakeSearchZG(t, `
if [ "$1" = index ]; then exit 0; fi
while [ "$#" -gt 0 ]; do
  if [ "$1" = --glob ]; then
    test "$2" = 'src/**/*.go'
    printf '#1 matchedBy=fts src/main.go:1-1\n1\tpackage main\n'
    exit 0
  fi
  shift
done
exit 1
`)
	out, err := tool.Execute(context.Background(), map[string]any{"query": "entry point", "path": filepath.Join(root, "src"), "globs": "*.go"})
	if err != nil || !strings.Contains(out, "src/main.go") {
		t.Fatalf("scoped search = %q, %v", out, err)
	}
}

func TestZvecGrepSearchRejectsLinkedManifestAndCachedFile(t *testing.T) {
	skipIfWindows(t)
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	index := filepath.Join(root, ".zvec-grep")
	if err := os.Mkdir(index, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := writeTestFile(t, root, "other.json", `{"embedding":{"provider":"local","model":"anything"}}`)
	if err := os.Symlink(manifest, filepath.Join(index, "manifest.json")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := indexedSearchEmbedding(index, root); err == nil {
		t.Fatal("linked manifest was accepted")
	}
	target := writeTestFile(t, root, "other.go", "CACHED_LINK_TEXT")
	if err := os.Symlink(target, filepath.Join(root, "link.go")); err != nil {
		t.Fatal(err)
	}
	out, err := filterIndexedSearch("#1 matchedBy=fts link.go:1\n1\tCACHED_LINK_TEXT\n", root, root, sandbox.Config{}, false, false, false)
	if err != nil || strings.Contains(out, "CACHED_LINK_TEXT") {
		t.Fatalf("linked result = %q, %v", out, err)
	}
}

func TestZvecGrepSearchManifestRootsCompareResolvedPaths(t *testing.T) {
	skipIfWindows(t)
	base := t.TempDir()
	base, _ = filepath.EvalSymlinks(base)
	root := filepath.Join(base, "real")
	index := filepath.Join(root, ".zvec-grep")
	if err := os.MkdirAll(index, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	// The index was created through the alias spelling; the workspace is
	// the same directory.
	writeTestFile(t, index, "manifest.json", `{"embedding":{"provider":"local","model":"potion-code-16m-v2"},"rootPaths":[{"absolutePath":"`+alias+`"}]}`)
	if embedding, exists, err := indexedSearchEmbedding(index, root); err != nil || !exists || embedding != "local/potion-code-16m-v2" {
		t.Fatalf("alias-spelled root rejected: %q, %v, %v", embedding, exists, err)
	}
	outside := filepath.Join(base, "elsewhere")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	// A recorded root that only lexically sits inside the workspace but
	// resolves elsewhere is still outside it.
	writeTestFile(t, index, "manifest.json", `{"embedding":{"provider":"local","model":"potion-code-16m-v2"},"rootPaths":[{"absolutePath":"`+filepath.Join(root, "escape")+`"}]}`)
	if _, _, err := indexedSearchEmbedding(index, root); err == nil {
		t.Fatal("root resolving outside the workspace was accepted")
	}
}

func TestZvecGrepSearchTruncatesAtHitBoundary(t *testing.T) {
	script := fakeSearchZG(t, "printf '%s' '"+strings.Repeat("x", searchMaxBytes+1000)+"'\n")
	out, _, truncated, err := runIndexedSearchCommand(context.Background(), nil, script, t.TempDir(), nil)
	if err != nil || !truncated || len(out) > searchMaxBytes+200 {
		t.Fatalf("unbounded or unreported truncation: %d bytes, %v, %v", len(out), truncated, err)
	}
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	writeTestFile(t, root, "a.go", "a")
	writeTestFile(t, root, "b.go", "b")
	// The stream ended inside the second hit: the first survives, the
	// partial one is dropped, and the cut is announced.
	text := "Q1 [primary]: q\nhits: 2\n\n#1 matchedBy=fts a.go:1-1\n1\tFIRST_HIT\n\n#2 matchedBy=fts b.go:1-1\n1\tPARTIAL"
	out, err = filterIndexedSearch(text, root, root, sandbox.Config{}, false, false, true)
	if err != nil || !strings.Contains(out, "FIRST_HIT") || strings.Contains(out, "PARTIAL") || !strings.Contains(out, "truncated") {
		t.Fatalf("truncated filter = %q, %v", out, err)
	}
	// A cut that lands after a complete hit and a following group line
	// keeps the hit.
	text = "Q1 [primary]: q\nhits: 1\n\n#1 matchedBy=fts a.go:1-1\n1\tFIRST_HIT\n\nQ2 [primary]: r\nhits: 3\n"
	out, err = filterIndexedSearch(text, root, root, sandbox.Config{}, false, false, true)
	if err != nil || !strings.Contains(out, "FIRST_HIT") || !strings.Contains(out, "truncated") {
		t.Fatalf("truncated after group line = %q, %v", out, err)
	}
}

func TestZvecGrepSearchFailureIsNotExactFallback(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "needle.txt", "needle")
	tool := NewZvecGrepSearchTool(NewToolRegistry(nil, WithUnsafeNoSandbox())).(*zvecGrepSearchTool)
	tool.zvecPath = fakeSearchZG(t, "echo 'model download unavailable' >&2\nexit 1\n")
	out, err := tool.Execute(context.Background(), map[string]any{"query": "needle", "path": root})
	if err == nil || !strings.Contains(err.Error(), "model download unavailable") || out != "" {
		t.Fatalf("expected index error, not a lexical result: %q, %v", out, err)
	}
}

func TestZvecGrepSearchFiltersCachedDeniedDeletedAndOutsideHits(t *testing.T) {
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	if err := os.Mkdir(filepath.Join(root, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(root, "src"), "open.go", "allowed")
	secret := writeTestFile(t, filepath.Join(root, "src"), "secret.go", "private")
	writeTestFile(t, root, "outside.go", "outside")
	input := "Q1 [primary]: q\nhits: 5\n\n" +
		"#1 matchedBy=fts+vector src/open.go:1-1\n1\tallowed\n" +
		"#2 matchedBy=fts src/secret.go:1-1\n1\tCACHED_SECRET\n" +
		"#3 matchedBy=vector outside.go:1-1\n1\tOUTSIDE_SCOPE\n" +
		"#4 matchedBy=fts ../escape.go:1-1\n1\tESCAPED_ROOT\n" +
		"#5 matchedBy=fts src/deleted.go:1-1\n1\tDELETED_CONTENT\n" +
		"\nQ2 [supplemental]: anchor\nhits: 1\n\n" +
		"#1 matchedBy=fts src/secret.go:1-1\n1\tCACHED_SECRET\n"
	out, err := filterIndexedSearch(input, root, filepath.Join(root, "src"), sandbox.Config{DenyPaths: []string{secret}}, true, false, false)
	if err != nil || !strings.Contains(out, "allowed") {
		t.Fatalf("filter = %q, %v", out, err)
	}
	for _, forbidden := range []string{"CACHED_SECRET", "secret.go", "OUTSIDE_SCOPE", "ESCAPED_ROOT", "DELETED_CONTENT"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("leaked %s:\n%s", forbidden, out)
		}
	}
	// Group lines frame the hits even when a whole group is filtered out.
	for _, want := range []string{"Q1 [primary]: q", "hits: 5", "Q2 [supplemental]: anchor", "hits: 1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing group line %q:\n%s", want, out)
		}
	}
	if _, err := filterIndexedSearch("unexpected format", root, root, sandbox.Config{}, false, false, false); err == nil {
		t.Fatal("unrecognized output must fail closed")
	}
}

func TestZvecGrepSearchModelAndPolicyFailuresDoNotLaunch(t *testing.T) {
	local := `{"embedding":{"provider":"local","model":"potion-code-16m-v2"}}`
	// Each scenario pins the guard it is named for: an acceptable manifest
	// everywhere else keeps the embedding check from masking later guards.
	for _, tc := range []struct{ scenario, manifest, want string }{
		{"remote", `{"embedding":{"provider":"qwen","model":"remote"}}`, "local embedding model"},
		{"readonly", local, "needs to create or refresh"},
		{"uncontained", local, "requires sandboxing"},
		{"denied_manifest", local, "blocked from reads"},
	} {
		t.Run(tc.scenario, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, ".zvec-grep"), 0o700); err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, filepath.Join(root, ".zvec-grep"), "manifest.json", tc.manifest)
			registry := NewToolRegistry(nil, WithUnsafeNoSandbox())
			switch tc.scenario {
			case "readonly":
				registry = stubSandboxRegistry(t, sandbox.Config{DenyWrite: true})
			case "denied_manifest":
				registry = stubSandboxRegistry(t, sandbox.Config{WritablePaths: []string{root}, DenyPaths: []string{filepath.Join(root, ".zvec-grep", "manifest.json")}})
			case "uncontained":
				registry = NewToolRegistry(nil)
			}
			tool := NewZvecGrepSearchTool(registry).(*zvecGrepSearchTool)
			tool.zvecPath = filepath.Join(root, "must-not-run")
			_, err := tool.Execute(context.Background(), map[string]any{"query": "something", "path": root})
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "must-not-run") {
				t.Fatalf("got %v, want %q before spawning", err, tc.want)
			}
		})
	}
}

func TestZvecGrepSearchCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	indexedSearchGate <- struct{}{}
	cancel()
	tool := NewZvecGrepSearchTool(NewToolRegistry(nil, WithUnsafeNoSandbox())).(*zvecGrepSearchTool)
	tool.zvecPath = "must-not-run"
	_, err := tool.Execute(ctx, map[string]any{"query": "query", "path": t.TempDir()})
	<-indexedSearchGate
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waiting cancellation: %v", err)
	}
	script := fakeSearchZG(t, "exec /bin/sleep 30\n")
	ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, _, _, err = runIndexedSearchCommand(ctx, nil, script, t.TempDir(), nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("process cancellation: %v", err)
	}
}

func TestZvecGrepSearchAvailabilityAndValidation(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	tool := NewZvecGrepSearchTool(NewToolRegistry(nil, WithUnsafeNoSandbox())).(*zvecGrepSearchTool)
	if _, err := tool.Execute(context.Background(), map[string]any{"query": "find things"}); err == nil || !strings.Contains(err.Error(), "requires zg") {
		t.Fatalf("missing zg must be reported: %v", err)
	}
	tool.zvecPath = filepath.Join(t.TempDir(), "must-not-run")
	// Each rejection must come from request validation, not from the
	// missing binary every query would otherwise fail on.
	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{}, "requires query, queries, fts, or vector"},
		{map[string]any{"query": "   "}, "requires query, queries, fts, or vector"},
		{map[string]any{"query": 5}, "must be a string or a list of strings"},
		{map[string]any{"query": "q", "limit": 51}, "between 1 and 50"},
		{map[string]any{"query": "q", "limit": 0}, "between 1 and 50"},
		{map[string]any{"query": "q", "symbolTypes": []any{"blob"}}, "symbolTypes accepts"},
		{map[string]any{"query": "q", "modifiedAfter": float64(-5)}, "modifiedAfter must be"},
		{map[string]any{"query": "q", "modifiedBefore": "-1d"}, "modifiedBefore must be"},
		{map[string]any{"query": "q", "fileTypes": "-x"}, "not a ripgrep file type"},
		{map[string]any{"query": "q", "globs": strings.Repeat("a", zvecMaxPathChars+1)}, "limited to"},
		{map[string]any{"query": "q", "path": filepath.Join(t.TempDir(), "absent")}, "path does not exist"},
	} {
		_, err := tool.Execute(context.Background(), tc.args)
		if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "must-not-run") {
			t.Fatalf("args %v: got %v, want validation error %q", tc.args, err, tc.want)
		}
	}
}

// Exercises the external CLI contract, automatic creation, refresh, and
// filtering under the real process sandbox. Downloads the small local model
// into the temporary workspace; keep normal unit tests offline.
func TestZvecGrepSearchLive(t *testing.T) {
	if os.Getenv("POLLYTOOL_REQUIRE_ZG_TESTS") != "1" {
		t.Skip("set POLLYTOOL_REQUIRE_ZG_TESTS=1 to test installed zg with the real sandbox")
	}
	skipIfWindows(t)
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	registry := NewToolRegistry(nil, WithSandboxFactory(sandbox.New, sandbox.Config{WritablePaths: []string{root}, AllowNetwork: true}))
	tool := NewZvecGrepSearchTool(registry)
	writeTestFile(t, root, "retry.go", "package demo\n// RetryWithBackoff retries failed network requests with increasing delays.\nfunc RetryWithBackoff() {}\n")
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	out, err := tool.Execute(ctx, map[string]any{"query": "where are network requests retried with increasing delays", "path": root, "limit": 3})
	if err != nil || !strings.Contains(out, "RetryWithBackoff") {
		t.Fatalf("first search: %q, %v", out, err)
	}
	sub := filepath.Join(root, "src")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, sub, "quartz.go", "package demo\n// QuartzDelivery dispatches quartz parcels to their destination.\nfunc QuartzDelivery() {}\n")
	out, err = tool.Execute(ctx, map[string]any{"queries": []any{"dispatch quartz parcels"}, "fts": "QuartzDelivery", "fuse": true, "path": sub, "globs": "*.go", "fileTypes": "go", "symbolTypes": []any{"function"}, "limit": 3})
	if err != nil || !strings.Contains(out, "QuartzDelivery") || strings.Contains(out, "retry.go") {
		t.Fatalf("refresh, routes, and scope: %q, %v", out, err)
	}
}
