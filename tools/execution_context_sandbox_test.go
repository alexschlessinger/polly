package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/internal/scratch"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
	"os/exec"
)

func TestBoundShellRestrictionsSandbox(t *testing.T) {
	skipUnlessSandboxTests(t)
	t.Setenv("HOME", t.TempDir())
	dir := realTempDir(t)
	secret := filepath.Join(dir, "private-data")
	public := filepath.Join(dir, "public-data")
	if err := os.WriteFile(public, []byte("public-value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secret, []byte("restricted-value"), 0600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "reader.sh")
	schema, err := json.Marshal(map[string]any{"title": "reader", "description": "read fixture", "type": "object", "properties": map[string]any{}, "sandbox": map[string]any{"denyPaths": []string{secret}, "denyWrite": true}})
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = --schema ]; then\ncat <<'SCHEMA'\n%s\nSCHEMA\nelse\ncat '%s'\ncat '%s'\nfi\n", schema, public, secret)
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	registry := NewToolRegistry(nil, WithNativeTools(), WithSandboxFactory(sandbox.New, sandbox.DefaultConfig()))
	defer registry.Close()
	_, err = registry.LoadShellToolWithNamespace(script, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	name := registry.All()[0].GetName()
	parent, _ := registry.Get(name)
	if out, err := parent.Execute(context.Background(), map[string]any{}); err == nil || strings.Contains(out, "restricted-value") {
		t.Fatalf("parent restriction did not apply: %q %v", out, err)
	}
	ec, err := registry.ExecutionPolicy(dir, ExecutionGrant{})
	if err != nil {
		t.Fatal(err)
	}
	bound, omitted, err := registry.BindExecutionContext(ec, []string{name})
	if err != nil {
		t.Fatalf("bind: %v omissions %v", err, omitted)
	}
	defer bound.Close()
	child, ok := bound.Get(name)
	if !ok {
		t.Fatal("tool omitted")
	}
	if out, err := child.Execute(context.Background(), map[string]any{}); err == nil || strings.Contains(out, "restricted-value") || !strings.Contains(out, "public-value") {
		t.Fatalf("bound shell widened authority: %q %v", out, err)
	}
}

// A read-only member with a scratch can use heredocs from inside its checkout,
// write under $TMPDIR, and build there with a tool's cache pointed at it,
// while the checkout stays unwritable.
func TestReadOnlyMemberScratchWritableCheckoutNot(t *testing.T) {
	skipUnlessSandboxTests(t)
	canonical := func(path string) string {
		t.Helper()
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			t.Fatal(err)
		}
		return resolved
	}
	root, scratch := canonical(t.TempDir()), canonical(t.TempDir())
	for name, body := range map[string]string{"fixture.txt": "fixture\n", "go.mod": "module probe\n\ngo 1.22\n", "main.go": "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"hello\") }\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	base := sandbox.DefaultConfig()
	base.ReadPaths = sandbox.HomeToolchainGrants()
	registry := NewToolRegistry(nil, WithNativeTools(), WithSandboxFactory(sandbox.New, base))
	defer registry.Close()
	for _, name := range []string{"bash", "write_file", "read_file"} {
		if _, err := registry.LoadToolAuto(name); err != nil {
			t.Fatal(err)
		}
	}
	ec, err := registry.ExecutionPolicy(root, ExecutionGrant{ReadOnly: true, Scratch: scratch})
	if err != nil {
		t.Fatal(err)
	}
	bound, _, err := registry.BindExecutionContext(ec, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer bound.Close()
	bash, _ := bound.Get("bash")
	ctx := context.Background()
	run := func(command string) (string, error) {
		t.Helper()
		return bash.Execute(ctx, map[string]any{"command": command})
	}
	if out, err := run("cat <<'EOF'\nheredoc ok\nEOF"); err != nil || !strings.Contains(out, "heredoc ok") {
		t.Errorf("heredoc from the checkout: %q %v", out, err)
	}
	if out, err := run(`printf x > "$TMPDIR/f" && cat "$TMPDIR/f"`); err != nil || strings.TrimSpace(out) != "x" {
		t.Errorf("scratch write through $TMPDIR: %q %v", out, err)
	} else if data, err := os.ReadFile(filepath.Join(scratch, "f")); err != nil || string(data) != "x" {
		t.Errorf("$TMPDIR write did not land in the scratch: %q %v", data, err)
	}
	if out, err := run(`test "$TMP" = "$TMPDIR" && test "$TEMP" = "$TMPDIR" && echo env-ok`); err != nil || !strings.Contains(out, "env-ok") {
		t.Errorf("scratch environment: %q %v", out, err)
	}
	for _, command := range []string{"printf x > f", "printf x > '" + root + "/g'"} {
		if out, err := run(command); err == nil {
			t.Errorf("checkout write succeeded: %s (%s)", command, out)
		}
	}
	for _, name := range []string{"f", "g"} {
		if _, err := os.Stat(filepath.Join(root, name)); err == nil {
			t.Errorf("checkout gained %s", name)
		}
	}
	writer, _ := bound.Get("write_file")
	if _, err := writer.Execute(ctx, map[string]any{"path": "note.txt", "content": "bad"}); err == nil {
		t.Error("native write into the checkout succeeded")
	}
	if _, err := writer.Execute(ctx, map[string]any{"path": filepath.Join(scratch, "note.txt"), "content": "ok"}); err != nil {
		t.Errorf("native write into the scratch: %v", err)
	}
	reader, _ := bound.Get("read_file")
	if out, err := reader.Execute(ctx, map[string]any{"path": "fixture.txt"}); err != nil || !strings.Contains(out, "fixture") {
		t.Errorf("checkout read: %q %v", out, err)
	}
	if _, err := exec.LookPath("go"); err == nil && !testing.Short() {
		if out, err := run(`GOCACHE="$TMPDIR/go-build" go build -o "$TMPDIR/bin" . && "$TMPDIR/bin" && test -d "$TMPDIR/go-build" && echo cache-ok`); err != nil || !strings.Contains(out, "hello") || !strings.Contains(out, "cache-ok") {
			t.Errorf("go build in the scratch: %q %v", out, err)
		}
	}
}

// moduleCacheGrant is the Go module cache as the explicit read grant an
// operator who builds Go in members adds: the private home hides it, and the
// presets grant no toolchain's cache. A cache outside the home needs none.
func moduleCacheGrant(t *testing.T) []string {
	t.Helper()
	raw, err := exec.Command("go", "env", "GOMODCACHE").Output()
	if err != nil {
		t.Fatal(err)
	}
	modcache := strings.TrimSpace(string(raw))
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	if real, err := filepath.EvalSymlinks(home); err == nil {
		home = real
	}
	if real, err := filepath.EvalSymlinks(modcache); err == nil {
		modcache = real
	}
	if !sandbox.PathWithin(modcache, home) {
		return nil
	}
	return []string{modcache}
}

// A read-only member builds, vets and tests a package with real dependencies
// once the operator grants the module cache: the grant in the base reaches
// the member, and the member points the build cache at its scratch. The
// fixture module above has none, so only a real checkout reaches the cache.
func TestReadOnlyMemberBuildsAgainstGrantedModuleCache(t *testing.T) {
	skipUnlessSandboxTests(t)
	if _, err := exec.LookPath("go"); err != nil || testing.Short() {
		t.Skip("go toolchain")
	}
	grant := moduleCacheGrant(t)
	if len(grant) == 0 {
		t.Skip("module cache outside the home needs no grant")
	}
	repo, err := filepath.EvalSymlinks("..")
	if err != nil {
		t.Fatal(err)
	}
	scratch := realTempDir(t)
	base, err := sandbox.ParsePreset("workspace+net+git")
	if err != nil {
		t.Fatal(err)
	}
	base.ReadPaths = append(base.ReadPaths, grant...)
	registry := NewToolRegistry(nil, WithNativeTools(), WithSandboxFactory(sandbox.New, base))
	defer registry.Close()
	if _, err := registry.LoadToolAuto("bash"); err != nil {
		t.Fatal(err)
	}
	ec, err := registry.ExecutionPolicy(repo, ExecutionGrant{ReadOnly: true, Scratch: scratch})
	if err != nil {
		t.Fatal(err)
	}
	bound, _, err := registry.BindExecutionContext(ec, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer bound.Close()
	bash, _ := bound.Get("bash")
	// With GOPROXY off this passes only by reading the granted cache.
	command := `export CGO_ENABLED=0 GOPROXY=off GOCACHE="$TMPDIR/go-build" && ` +
		`go build ./cmd/polly/internal/style && ` +
		`go vet ./cmd/polly/internal/style && ` +
		`go test -count=1 ./cmd/polly/internal/style`
	if out, err := bash.Execute(context.Background(), map[string]any{"command": command}); err != nil {
		t.Fatalf("read-only member could not build against the module cache: %v\n%s", err, out)
	}
}

// A member runs a package's own tests inside its scratch. internal/safefile
// opens every component of a path in turn to refuse a symlinked route, so the
// open fails on the first ancestor the policy denies; scratch sits outside the
// private home, and its root stays traversable, for exactly this reason. A
// scratch whose ancestors are denied makes such a suite unrunnable in a member
// however the leaf is granted.
func TestMemberRunsTestsThatWalkTheirScratchPath(t *testing.T) {
	skipUnlessSandboxTests(t)
	if _, err := exec.LookPath("go"); err != nil || testing.Short() {
		t.Skip("go toolchain")
	}
	repo, err := filepath.EvalSymlinks("..")
	if err != nil {
		t.Fatal(err)
	}
	dir, err := scratch.Claim(filepath.Join(t.TempDir(), "slot-0000"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { scratch.RemoveAll(dir) })
	base, err := sandbox.ParsePreset("workspace+net+git")
	if err != nil {
		t.Fatal(err)
	}
	base.ReadPaths = append(base.ReadPaths, moduleCacheGrant(t)...)
	registry := NewToolRegistry(nil, WithNativeTools(), WithSandboxFactory(sandbox.New, base))
	defer registry.Close()
	if _, err := registry.LoadToolAuto("bash"); err != nil {
		t.Fatal(err)
	}
	ec, err := registry.ExecutionPolicy(repo, ExecutionGrant{ReadOnly: true, Scratch: dir})
	if err != nil {
		t.Fatal(err)
	}
	bound, _, err := registry.BindExecutionContext(ec, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer bound.Close()
	bash, _ := bound.Get("bash")
	if out, err := bash.Execute(context.Background(), map[string]any{"command": `CGO_ENABLED=0 GOPROXY=off GOCACHE="$TMPDIR/go-build" go test -count=1 ./internal/safefile`}); err != nil {
		t.Fatalf("member could not run a suite that walks its scratch path: %v\n%s", err, out)
	}
}

// A shell tool that lives under the home directory stays loadable and bindable
// into a member context: its script is exposed inside the private home.
func TestShellToolUnderPrivateHomeLoadsAndBinds(t *testing.T) {
	skipUnlessSandboxTests(t)
	home := realTempDir(t)
	t.Setenv("HOME", home)
	root := realTempDir(t)
	fixture := filepath.Join(root, "fixture.txt")
	if err := os.WriteFile(fixture, []byte("fixture-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(home, "tools", "reader.sh")
	if err := os.MkdirAll(filepath.Dir(script), 0o700); err != nil {
		t.Fatal(err)
	}
	schema, err := json.Marshal(map[string]any{"title": "reader", "description": "read fixture", "type": "object", "properties": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	// The script sources a sibling: its directory, not only the file, must
	// be visible inside the private home.
	lib := filepath.Join(filepath.Dir(script), "lib.sh")
	if err := os.WriteFile(lib, []byte("read_fixture() { cat fixture.txt; }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("#!/bin/sh\n. \"$(dirname \"$0\")/lib.sh\"\nif [ \"$1\" = --schema ]; then\ncat <<'SCHEMA'\n%s\nSCHEMA\nelse\nread_fixture\nfi\n", schema)
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	registry := NewToolRegistry(nil, WithNativeTools(), WithSandboxFactory(sandbox.New, sandbox.DefaultConfig()))
	defer registry.Close()
	if _, err := registry.LoadShellToolWithNamespace(script, "fixture"); err != nil {
		t.Fatal(err)
	}
	name := registry.All()[0].GetName()
	ec, err := registry.ExecutionPolicy(root, ExecutionGrant{})
	if err != nil {
		t.Fatal(err)
	}
	bound, omitted, err := registry.BindExecutionContext(ec, []string{name})
	if err != nil {
		t.Fatalf("bind: %v omissions %v", err, omitted)
	}
	defer bound.Close()
	child, ok := bound.Get(name)
	if !ok {
		t.Fatalf("tool omitted: %v", omitted)
	}
	if out, err := child.Execute(context.Background(), map[string]any{}); err != nil || !strings.Contains(out, "fixture-value") {
		t.Fatalf("bound shell tool under the home directory: %q %v", out, err)
	}
}
