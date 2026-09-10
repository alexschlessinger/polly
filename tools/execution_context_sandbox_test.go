package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/tools/sandbox"
	"os/exec"
)

func TestBoundShellRestrictionsSandbox(t *testing.T) {
	if os.Getenv("POLLYTOOL_REQUIRE_SANDBOX_TESTS") != "1" {
		t.Skip("opt-in process sandbox")
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("process sandbox")
	}
	t.Setenv("HOME", t.TempDir())
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
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
	registry := NewToolRegistry(nil, WithSandboxFactory(sandbox.New, sandbox.DefaultConfig()))
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
// write under $TMPDIR, and build Go there, while the checkout stays unwritable.
func TestReadOnlyMemberScratchWritableCheckoutNot(t *testing.T) {
	if os.Getenv("POLLYTOOL_REQUIRE_SANDBOX_TESTS") != "1" {
		t.Skip("opt-in process sandbox")
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("sandbox platform")
	}
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
	registry := NewToolRegistry(nil, WithSandboxFactory(sandbox.New, sandbox.DefaultConfig()))
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
	if out, err := run(`test "$GOCACHE" = "$TMPDIR/go-build" && test "$GOTMPDIR" = "$TMPDIR" && test "$TMP" = "$TMPDIR" && echo env-ok`); err != nil || !strings.Contains(out, "env-ok") {
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
		if out, err := run(`go build -o "$TMPDIR/bin" . && "$TMPDIR/bin" && test -d "$TMPDIR/go-build" && echo cache-ok`); err != nil || !strings.Contains(out, "hello") || !strings.Contains(out, "cache-ok") {
			t.Errorf("go build in the scratch: %q %v", out, err)
		}
	}
}
