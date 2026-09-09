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
	ec, err := registry.ExecutionPolicy(dir, false, nil, nil)
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
