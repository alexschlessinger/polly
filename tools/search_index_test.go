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

func TestSearchFilesLoadRequiresZG(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	registry := NewToolRegistry(nil, WithUnsafeNoSandbox())
	if SearchFilesAvailable() {
		t.Fatal("zg unexpectedly available")
	}
	if _, err := registry.LoadToolAuto("search_files"); !errors.Is(err, ErrSearchFilesUnavailable) {
		t.Fatalf("missing dependency error = %v", err)
	}
	if _, ok := registry.Get("search_files"); ok || len(registry.GetSchemas()) != 0 {
		t.Fatal("search_files loaded without zg")
	}
	installSearchDependencyForTest(t)
	if !SearchFilesAvailable() {
		t.Fatal("installed zg was not discovered")
	}
	if _, err := registry.LoadToolAuto("search_files"); err != nil {
		t.Fatalf("load with zg installed: %v", err)
	}
	tool, ok := registry.Get("search_files")
	if !ok || tool.GetSchema().Properties()["query"] == nil {
		t.Fatal("loaded search tool lacks indexed discovery")
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

func TestIndexedSearchCreatesRefreshesAndContainsCommands(t *testing.T) {
	skipIfWindows(t)
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	writeTestFile(t, root, "main.go", "package main\n")
	sb := &searchCommandSandbox{}
	registry := NewToolRegistry(nil, WithSandboxFactory(func(sandbox.Config) (sandbox.Sandbox, error) { return sb, nil }, sandbox.Config{WritablePaths: []string{root}}))
	tool := NewSearchFilesTool(registry).(*searchFilesTool)
	tool.zvecPath = fakeSearchZG(t, `
test "$POLLY_TEST_SEARCH_WRAPPED" = 1
printf '%s\n' "$*" >> calls
case "$1" in
index)
  /bin/mkdir -p .zvec-grep
  if [ ! -f .zvec-grep/manifest.json ]; then
    /bin/cat > .zvec-grep/manifest.json <<'EOF'
{"embedding":{"provider":"local","model":"potion-code-16m-v2"},"rootPaths":[]}
EOF
  fi
  ;;
query)
  test "$2" = --hybrid
  printf '%s' "$3" > query-value
  printf '#1 matchedBy=fts+vector main.go:1-1\nsource:\n1\tpackage main\n'
  ;;
*) exit 1;;
esac
`)
	query := "find `touch injected` $(touch injected) --drop"
	for range 2 {
		out, err := tool.Execute(context.Background(), map[string]any{"query": query, "path": root})
		if err != nil || !strings.Contains(out, "package main") {
			t.Fatalf("indexed search = %q, %v", out, err)
		}
	}
	if sb.calls != 4 {
		t.Fatalf("wrapped %d commands, want index + query twice", sb.calls)
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
	if !strings.HasPrefix(lines[0], "index "+root+" --embedding "+defaultSearchEmbedding) ||
		!strings.HasPrefix(lines[2], "index --embedding "+defaultSearchEmbedding) {
		t.Fatalf("first build should name root, refresh should preserve stored filters:\n%s", calls)
	}
	for _, line := range lines {
		if !strings.Contains(line, "--mode direct") || !strings.Contains(line, "--model-cache "+filepath.Join(root, ".zvec-grep", "polly", "models")) {
			t.Fatalf("missing direct mode or workspace cache: %s", line)
		}
	}
}

func TestIndexedSearchRootSkipsRuntimeStateAndBroadAncestors(t *testing.T) {
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
	tool := NewSearchFilesTool(NewToolRegistry(nil, WithUnsafeNoSandbox())).(*searchFilesTool)
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

func TestIndexedSearchRootStaysInsideWritablePolicy(t *testing.T) {
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
	tool := NewSearchFilesTool(stubSandboxRegistry(t, policy)).(*searchFilesTool)
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

func TestIndexedSearchDaemonLeaseUsesSnapshot(t *testing.T) {
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	writeTestFile(t, root, "main.go", "package main\n")
	if err := os.Mkdir(filepath.Join(root, ".zvec-grep"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(root, ".zvec-grep"), "manifest.json", `{"embedding":{"provider":"local","model":"custom-local"}}`)
	tool := NewSearchFilesTool(NewToolRegistry(nil, WithUnsafeNoSandbox())).(*searchFilesTool)
	tool.zvecPath = fakeSearchZG(t, `
if [ "$1" = index ]; then
  echo 'A zvec-grep daemon owns index writes for this root' >&2
  exit 1
fi
printf '#1 matchedBy=fts main.go:1-1\n1\tpackage main\n'
`)
	out, err := tool.Execute(context.Background(), map[string]any{"query": "entry point", "path": root})
	if err != nil || !strings.Contains(out, "possibly_stale") || !strings.Contains(out, "package main") {
		t.Fatalf("snapshot = %q, %v", out, err)
	}
}

func TestIndexedSearchScopesBeforeRanking(t *testing.T) {
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	for _, dir := range []string{".git", "src"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(t, filepath.Join(root, "src"), "main.go", "package main\n")
	tool := NewSearchFilesTool(NewToolRegistry(nil, WithUnsafeNoSandbox())).(*searchFilesTool)
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
	out, err := tool.Execute(context.Background(), map[string]any{"query": "entry point", "path": filepath.Join(root, "src"), "include": "*.go"})
	if err != nil || !strings.Contains(out, "src/main.go") {
		t.Fatalf("scoped search = %q, %v", out, err)
	}
}

func TestIndexedSearchRejectsLinkedManifestAndCachedFile(t *testing.T) {
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
	out, err := filterIndexedSearch("#1 matchedBy=fts link.go:1\n1\tCACHED_LINK_TEXT\n", root, root, "", sandbox.Config{}, false, false)
	if err != nil || strings.Contains(out, "CACHED_LINK_TEXT") {
		t.Fatalf("linked result = %q, %v", out, err)
	}
}

func TestIndexedSearchManifestRootsCompareResolvedPaths(t *testing.T) {
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

func TestIndexedSearchBoundsCommandOutput(t *testing.T) {
	script := fakeSearchZG(t, "printf '%s' '"+strings.Repeat("x", searchMaxBytes+1000)+"'\n")
	out, _, err := runIndexedSearchCommand(context.Background(), nil, script, t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), "output limit") || len(out) > searchMaxBytes+200 {
		t.Fatalf("unbounded or unreported truncation: %d bytes, %v", len(out), err)
	}
}

func TestIndexedSearchFailureIsNotExactFallback(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "needle.txt", "needle")
	tool := NewSearchFilesTool(NewToolRegistry(nil, WithUnsafeNoSandbox())).(*searchFilesTool)
	tool.zvecPath = fakeSearchZG(t, "echo 'model download unavailable' >&2\nexit 1\n")
	out, err := tool.Execute(context.Background(), map[string]any{"query": "needle", "path": root})
	if err == nil || !strings.Contains(err.Error(), "model download unavailable") || out != "" {
		t.Fatalf("expected index error, not a lexical result: %q, %v", out, err)
	}
}

func TestIndexedSearchFiltersCachedDeniedDeletedAndOutsideHits(t *testing.T) {
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	if err := os.Mkdir(filepath.Join(root, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(root, "src"), "open.go", "allowed")
	secret := writeTestFile(t, filepath.Join(root, "src"), "secret.go", "private")
	writeTestFile(t, root, "outside.go", "outside")
	input := "#1 matchedBy=fts+vector src/open.go:1-1\n1\tallowed\n" +
		"#2 matchedBy=fts src/secret.go:1-1\n1\tCACHED_SECRET\n" +
		"#3 matchedBy=vector outside.go:1-1\n1\tOUTSIDE_SCOPE\n" +
		"#4 matchedBy=fts ../escape.go:1-1\n1\tESCAPED_ROOT\n" +
		"#5 matchedBy=fts src/deleted.go:1-1\n1\tDELETED_CONTENT\n"
	out, err := filterIndexedSearch(input, root, filepath.Join(root, "src"), "*.go", sandbox.Config{DenyPaths: []string{secret}}, true, false)
	if err != nil || !strings.Contains(out, "allowed") {
		t.Fatalf("filter = %q, %v", out, err)
	}
	for _, forbidden := range []string{"CACHED_SECRET", "secret.go", "OUTSIDE_SCOPE", "ESCAPED_ROOT", "DELETED_CONTENT"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("leaked %s:\n%s", forbidden, out)
		}
	}
	if _, err := filterIndexedSearch("unexpected format", root, root, "", sandbox.Config{}, false, false); err == nil {
		t.Fatal("unrecognized output must fail closed")
	}
}

func TestIndexedSearchModelAndPolicyFailuresDoNotLaunch(t *testing.T) {
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
			tool := NewSearchFilesTool(registry).(*searchFilesTool)
			tool.zvecPath = filepath.Join(root, "must-not-run")
			_, err := tool.Execute(context.Background(), map[string]any{"query": "something", "path": root})
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "must-not-run") {
				t.Fatalf("got %v, want %q before spawning", err, tc.want)
			}
		})
	}
}

func TestIndexedSearchCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	indexedSearchGate <- struct{}{}
	cancel()
	tool := NewSearchFilesTool(NewToolRegistry(nil, WithUnsafeNoSandbox())).(*searchFilesTool)
	tool.zvecPath = "must-not-run"
	_, err := tool.searchIndexed(ctx, Args(map[string]any{"path": t.TempDir()}), "query")
	<-indexedSearchGate
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waiting cancellation: %v", err)
	}
	script := fakeSearchZG(t, "exec /bin/sleep 30\n")
	ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, _, err = runIndexedSearchCommand(ctx, nil, script, t.TempDir(), nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("process cancellation: %v", err)
	}
}

func TestIndexedSearchAvailabilityAndSchema(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	tool := NewSearchFilesTool(NewToolRegistry(nil, WithUnsafeNoSandbox())).(*searchFilesTool)
	if strings.Contains(tool.GetSchema().Description(), "automatically creates") {
		t.Fatal("schema advertised absent zg")
	}
	if _, err := tool.Execute(context.Background(), map[string]any{"query": "find things"}); err == nil {
		t.Fatal("missing zg must be reported")
	}
	tool.zvecPath = filepath.Join(t.TempDir(), "must-not-run")
	if !strings.Contains(tool.GetSchema().Description(), "Start here before") {
		t.Fatal("schema must prefer indexed discovery")
	}
	// Each rejection must come from argument validation, not from the
	// missing binary every query would otherwise fail on.
	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"query": "q", "pattern": "p"}, "not both"},
		{map[string]any{"query": "q", "regex": true}, "not both"},
		{map[string]any{"query": "q", "limit": 51}, "between 1 and 50"},
		{map[string]any{"query": "q", "include": "[bad"}, "invalid include glob"},
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
func TestIndexedSearchLive(t *testing.T) {
	if os.Getenv("POLLYTOOL_REQUIRE_ZG_TESTS") != "1" {
		t.Skip("set POLLYTOOL_REQUIRE_ZG_TESTS=1 to test installed zg with the real sandbox")
	}
	skipIfWindows(t)
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	registry := NewToolRegistry(nil, WithSandboxFactory(sandbox.New, sandbox.Config{WritablePaths: []string{root}, AllowNetwork: true}))
	tool := NewSearchFilesTool(registry)
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
	out, err = tool.Execute(ctx, map[string]any{"query": "QuartzDelivery dispatch quartz parcels", "path": sub, "include": "*.go", "limit": 3})
	if err != nil || !strings.Contains(out, "QuartzDelivery") || strings.Contains(out, "retry.go") {
		t.Fatalf("refresh and scope: %q, %v", out, err)
	}
}
