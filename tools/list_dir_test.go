package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

func TestListDirEntriesDirsFirst(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "zebra.txt", "zz")
	writeTestFile(t, dir, "alpha.txt", "aaaa")
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(filepath.Join(dir, "alpha.txt"), filepath.Join(dir, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	tool := NewListDirTool(NewToolRegistry(nil))
	out, err := tool.Execute(context.Background(), map[string]any{"path": dir})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	want := fmt.Sprintf("4 entries in %s:\nsub/\nalpha.txt (4 bytes)\nlink@\nzebra.txt (2 bytes)", dir)
	if out != want {
		t.Fatalf("unexpected listing:\n%q\nwant\n%q", out, want)
	}
}

func TestListDirOffsetPaging(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 3; i++ {
		writeTestFile(t, dir, fmt.Sprintf("f%d.txt", i), "x")
	}
	tool := NewListDirTool(NewToolRegistry(nil))
	out, err := tool.Execute(context.Background(), map[string]any{"path": dir, "offset": 3})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "f2.txt") || strings.Contains(out, "f0.txt") {
		t.Fatalf("unexpected page: %q", out)
	}
	out, err = tool.Execute(context.Background(), map[string]any{"path": dir, "offset": 9})
	if err != nil || !strings.Contains(out, "no entries at or after offset 9 (total 3)") {
		t.Fatalf("unexpected past-end result: %q, %v", out, err)
	}
}

func TestListDirEmpty(t *testing.T) {
	dir := t.TempDir()
	tool := NewListDirTool(NewToolRegistry(nil))
	out, err := tool.Execute(context.Background(), map[string]any{"path": dir})
	if err != nil || !strings.Contains(out, "is empty") {
		t.Fatalf("unexpected empty result: %q, %v", out, err)
	}
}

func TestListDirErrors(t *testing.T) {
	dir := t.TempDir()
	path := writeTestFile(t, dir, "f.txt", "x")
	tool := NewListDirTool(NewToolRegistry(nil))
	if _, err := tool.Execute(context.Background(), map[string]any{}); err == nil {
		t.Fatal("expected error for missing path")
	}
	if _, err := tool.Execute(context.Background(), map[string]any{"path": filepath.Join(dir, "absent")}); err == nil {
		t.Fatal("expected error for missing directory")
	}
	if _, err := tool.Execute(context.Background(), map[string]any{"path": path}); err == nil {
		t.Fatal("expected error for non-directory path")
	}
}

func TestListDirHonorsSandboxDenyPaths(t *testing.T) {
	denied := t.TempDir()
	registry := stubSandboxRegistry(t, sandbox.Config{DenyPaths: []string{denied}})
	tool := NewListDirTool(registry)
	_, err := tool.Execute(context.Background(), map[string]any{"path": denied})
	if err == nil || !strings.Contains(err.Error(), "sandbox policy") {
		t.Fatalf("expected sandbox denial, got %v", err)
	}
}
