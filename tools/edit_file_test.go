package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

func TestEditFileReplacesUniqueString(t *testing.T) {
	path := writeTestFile(t, t.TempDir(), "f.txt", "alpha\nbeta\ngamma\n")
	tool := NewEditFileTool(NewToolRegistry(nil))
	out, err := tool.Execute(context.Background(), map[string]any{
		"path": path, "old_string": "beta", "new_string": "delta",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "1 replacement(s)") {
		t.Fatalf("unexpected result: %q", out)
	}
	if !strings.Contains(out, "2: delta") {
		t.Fatalf("expected numbered snippet of the edit, got %q", out)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "alpha\ndelta\ngamma\n" {
		t.Fatalf("unexpected file content: %q", data)
	}
}

func TestEditFileReplaceAll(t *testing.T) {
	path := writeTestFile(t, t.TempDir(), "f.txt", "x=1\nx=2\nx=3\n")
	tool := NewEditFileTool(NewToolRegistry(nil))
	out, err := tool.Execute(context.Background(), map[string]any{
		"path": path, "old_string": "x=", "new_string": "y=", "replace_all": true,
	})
	if err != nil || !strings.Contains(out, "3 replacement(s)") {
		t.Fatalf("unexpected result: %q, %v", out, err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "y=1\ny=2\ny=3\n" {
		t.Fatalf("unexpected file content: %q", data)
	}
}

func TestEditFileDeletion(t *testing.T) {
	path := writeTestFile(t, t.TempDir(), "f.txt", "keep\ndrop me\nkeep too\n")
	tool := NewEditFileTool(NewToolRegistry(nil))
	if _, err := tool.Execute(context.Background(), map[string]any{
		"path": path, "old_string": "drop me\n", "new_string": "",
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "keep\nkeep too\n" {
		t.Fatalf("unexpected file content: %q", data)
	}
}

func TestEditFileNotFoundError(t *testing.T) {
	path := writeTestFile(t, t.TempDir(), "f.txt", "alpha\n")
	tool := NewEditFileTool(NewToolRegistry(nil))
	_, err := tool.Execute(context.Background(), map[string]any{
		"path": path, "old_string": "missing", "new_string": "x",
	})
	if err == nil || !strings.Contains(err.Error(), "read_file") {
		t.Fatalf("expected not-found error pointing at read_file, got %v", err)
	}
}

func TestEditFileAmbiguousMatchError(t *testing.T) {
	path := writeTestFile(t, t.TempDir(), "f.txt", "dup\ndup\n")
	tool := NewEditFileTool(NewToolRegistry(nil))
	_, err := tool.Execute(context.Background(), map[string]any{
		"path": path, "old_string": "dup", "new_string": "x",
	})
	if err == nil || !strings.Contains(err.Error(), "occurs 2 times") || !strings.Contains(err.Error(), "replace_all") {
		t.Fatalf("expected ambiguity error suggesting replace_all, got %v", err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "dup\ndup\n" {
		t.Fatalf("file must be unchanged on error, got %q", data)
	}
}

func TestEditFileArgumentErrors(t *testing.T) {
	path := writeTestFile(t, t.TempDir(), "f.txt", "alpha\n")
	tool := NewEditFileTool(NewToolRegistry(nil))
	if _, err := tool.Execute(context.Background(), map[string]any{
		"path": path, "old_string": "", "new_string": "x",
	}); err == nil || !strings.Contains(err.Error(), "write_file") {
		t.Fatalf("expected empty old_string error pointing at write_file, got %v", err)
	}
	if _, err := tool.Execute(context.Background(), map[string]any{
		"path": path, "old_string": "same", "new_string": "same",
	}); err == nil || !strings.Contains(err.Error(), "identical") {
		t.Fatalf("expected identical-strings error, got %v", err)
	}
	if _, err := tool.Execute(context.Background(), map[string]any{
		"path": filepath.Join(t.TempDir(), "absent"), "old_string": "a", "new_string": "b",
	}); err == nil {
		t.Fatal("expected missing-file error")
	}
}

func TestEditFilePreservesPermissions(t *testing.T) {
	path := writeTestFile(t, t.TempDir(), "f.txt", "alpha\n")
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	tool := NewEditFileTool(NewToolRegistry(nil))
	if _, err := tool.Execute(context.Background(), map[string]any{
		"path": path, "old_string": "alpha", "new_string": "beta",
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("expected 0600 permissions preserved, got %v, %v", info.Mode(), err)
	}
}

func TestEditFileRefusesBinary(t *testing.T) {
	path := writeTestFile(t, t.TempDir(), "blob.bin", "a\x00b")
	tool := NewEditFileTool(NewToolRegistry(nil))
	if _, err := tool.Execute(context.Background(), map[string]any{
		"path": path, "old_string": "a", "new_string": "c",
	}); err == nil || !strings.Contains(err.Error(), "binary") {
		t.Fatalf("expected binary refusal, got %v", err)
	}
}

func TestEditFileSandboxDenyWrite(t *testing.T) {
	path := writeTestFile(t, t.TempDir(), "f.txt", "alpha\n")
	registry := stubSandboxRegistry(t, sandbox.Config{DenyWrite: true})
	tool := NewEditFileTool(registry)
	_, err := tool.Execute(context.Background(), map[string]any{
		"path": path, "old_string": "alpha", "new_string": "beta",
	})
	if err == nil || !strings.Contains(err.Error(), "sandbox policy") {
		t.Fatalf("expected DenyWrite denial, got %v", err)
	}
}

func TestEditFileSandboxDenyPathsBlocksRead(t *testing.T) {
	denied := t.TempDir()
	path := writeTestFile(t, denied, "f.txt", "alpha\n")
	registry := stubSandboxRegistry(t, sandbox.Config{DenyPaths: []string{denied}})
	tool := NewEditFileTool(registry)
	_, err := tool.Execute(context.Background(), map[string]any{
		"path": path, "old_string": "alpha", "new_string": "beta",
	})
	if err == nil || !strings.Contains(err.Error(), "sandbox policy") {
		t.Fatalf("expected read denial, got %v", err)
	}
}
