package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	skipIfWindows(t) // POSIX permission bits don't round-trip on windows
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

func TestEditFileConcurrentEditsBothApply(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := NewEditFileTool(NewToolRegistry(nil))
	for round := 0; round < 20; round++ {
		if err := os.WriteFile(path, []byte("alpha\nbeta\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for _, edit := range [][2]string{{"alpha", "ALPHA"}, {"beta", "BETA"}} {
			wg.Add(1)
			go func(oldText, newText string) {
				defer wg.Done()
				_, err := tool.Execute(context.Background(), map[string]any{"path": path, "old_string": oldText, "new_string": newText})
				errs <- err
			}(edit[0], edit[1])
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "ALPHA\nBETA\n" {
			t.Fatalf("round %d: concurrent edits lost a change: %q", round, data)
		}
	}
}

func TestEditFileRefusesSymlinkSwappedAfterCheck(t *testing.T) {
	// A symlinked path is resolved and edited in place; the edit lands on the
	// resolved target, never through a link at open time.
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(target, []byte("hello world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	tool := NewEditFileTool(NewToolRegistry(nil))
	if _, err := tool.Execute(context.Background(), map[string]any{"path": link, "old_string": "hello", "new_string": "goodbye"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	data, _ := os.ReadFile(target)
	if string(data) != "goodbye world\n" {
		t.Fatalf("edit through link did not reach target: %q", data)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link was replaced: %v %v", info, err)
	}
}
