package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

func TestWriteFileCreatesWithParents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a", "b", "new.txt")
	tool := NewWriteFileTool(NewToolRegistry(nil))
	out, err := tool.Execute(context.Background(), map[string]any{"path": path, "content": "one\ntwo\n"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "Created "+path) || !strings.Contains(out, "8 bytes, 2 lines") {
		t.Fatalf("unexpected result: %q", out)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "one\ntwo\n" {
		t.Fatalf("unexpected file content: %q, %v", data, err)
	}
}

func TestWriteFileOverwriteReporting(t *testing.T) {
	path := writeTestFile(t, t.TempDir(), "f.txt", "old content here")
	tool := NewWriteFileTool(NewToolRegistry(nil))
	out, err := tool.Execute(context.Background(), map[string]any{"path": path, "content": "new"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "Overwrote "+path) || !strings.Contains(out, "was 16 bytes") {
		t.Fatalf("unexpected result: %q", out)
	}
}

func TestWriteFileEmptyContentAllowed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.txt")
	tool := NewWriteFileTool(NewToolRegistry(nil))
	out, err := tool.Execute(context.Background(), map[string]any{"path": path, "content": ""})
	if err != nil || !strings.Contains(out, "0 bytes, 0 lines") {
		t.Fatalf("unexpected result: %q, %v", out, err)
	}
}

func TestWriteFileErrors(t *testing.T) {
	dir := t.TempDir()
	tool := NewWriteFileTool(NewToolRegistry(nil))
	if _, err := tool.Execute(context.Background(), map[string]any{"content": "x"}); err == nil {
		t.Fatal("expected error for missing path")
	}
	if _, err := tool.Execute(context.Background(), map[string]any{"path": filepath.Join(dir, "f")}); err == nil {
		t.Fatal("expected error for missing content")
	}
	if _, err := tool.Execute(context.Background(), map[string]any{"path": dir, "content": "x"}); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatal("expected directory error")
	}
}

func TestWriteFileSandboxDenyWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	registry := stubSandboxRegistry(t, sandbox.Config{DenyWrite: true})
	tool := NewWriteFileTool(registry)
	_, err := tool.Execute(context.Background(), map[string]any{"path": path, "content": "x"})
	if err == nil || !strings.Contains(err.Error(), "sandbox policy") {
		t.Fatalf("expected DenyWrite denial, got %v", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("expected no file to be created, got %v", statErr)
	}
}

func TestWriteFileSandboxDenyWritePathIsland(t *testing.T) {
	dir := t.TempDir()
	island := filepath.Join(dir, "protected")
	if err := os.Mkdir(island, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	registry := stubSandboxRegistry(t, sandbox.Config{WritablePaths: []string{dir}, DenyWritePaths: []string{island}})
	tool := NewWriteFileTool(registry)
	_, err := tool.Execute(context.Background(), map[string]any{"path": filepath.Join(island, "f"), "content": "x"})
	if err == nil || !strings.Contains(err.Error(), "sandbox policy") {
		t.Fatalf("expected deny-write island denial, got %v", err)
	}
	if _, err := tool.Execute(context.Background(), map[string]any{"path": filepath.Join(dir, "ok.txt"), "content": "x"}); err != nil {
		t.Fatalf("expected sibling write to succeed, got %v", err)
	}
}

func TestWriteToolsFailClosedWithoutSandbox(t *testing.T) {
	registry := NewToolRegistry(nil)
	for _, name := range []string{"write_file", "edit_file"} {
		if _, err := registry.LoadToolAuto(name); err == nil || !strings.Contains(err.Error(), "requires sandboxing") {
			t.Fatalf("expected %s to fail closed without a sandbox, got %v", name, err)
		}
	}
	unsafe := NewToolRegistry(nil, WithUnsafeNoSandbox())
	for _, name := range []string{"write_file", "edit_file"} {
		if _, err := unsafe.LoadToolAuto(name); err != nil {
			t.Fatalf("expected %s to load with WithUnsafeNoSandbox, got %v", name, err)
		}
	}
	sandboxed := stubSandboxRegistry(t, sandbox.Config{})
	for _, name := range []string{"write_file", "edit_file"} {
		if _, err := sandboxed.LoadToolAuto(name); err != nil {
			t.Fatalf("expected %s to load with a sandbox factory, got %v", name, err)
		}
	}
}
