package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

func writeTestFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	return path
}

func TestReadFileNumberedLines(t *testing.T) {
	path := writeTestFile(t, t.TempDir(), "f.txt", "alpha\nbeta\ngamma\n")
	tool := NewReadFileTool(NewToolRegistry(nil))
	out, err := tool.Execute(context.Background(), map[string]any{"path": path})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out != "1: alpha\n2: beta\n3: gamma\n" {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestReadFileOffsetAndLimit(t *testing.T) {
	path := writeTestFile(t, t.TempDir(), "f.txt", "alpha\nbeta\ngamma\n")
	tool := NewReadFileTool(NewToolRegistry(nil))
	out, err := tool.Execute(context.Background(), map[string]any{"path": path, "offset": 2, "limit": 1})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.HasPrefix(out, "2: beta\n") || strings.Contains(out, "gamma") {
		t.Fatalf("unexpected output: %q", out)
	}
	if !strings.Contains(out, "[bounded file output truncated]") {
		t.Fatalf("expected truncation note, got %q", out)
	}
}

func TestReadFileQuery(t *testing.T) {
	path := writeTestFile(t, t.TempDir(), "f.txt", "alpha\nbeta\ngamma\n")
	tool := NewReadFileTool(NewToolRegistry(nil))
	out, err := tool.Execute(context.Background(), map[string]any{"path": path, "query": "mm"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out != "3: gamma\n" {
		t.Fatalf("unexpected output: %q", out)
	}
	out, err = tool.Execute(context.Background(), map[string]any{"path": path, "query": "absent"})
	if err != nil || !strings.Contains(out, `No matches for "absent"`) {
		t.Fatalf("unexpected no-match result: %q, %v", out, err)
	}
}

func TestReadFileByteOffset(t *testing.T) {
	path := writeTestFile(t, t.TempDir(), "f.txt", "hello, world")
	tool := NewReadFileTool(NewToolRegistry(nil))
	out, err := tool.Execute(context.Background(), map[string]any{"path": path, "byte_offset": 7})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "bytes 7-12 of 12; raw window]\nworld") {
		t.Fatalf("unexpected window: %q", out)
	}
	if _, err := tool.Execute(context.Background(), map[string]any{"path": path, "byte_offset": 1, "limit": 2}); err == nil {
		t.Fatal("expected byte_offset/limit conflict error")
	}
	out, err = tool.Execute(context.Background(), map[string]any{"path": path, "byte_offset": 99})
	if err != nil || out != "File has no content at or after byte 99." {
		t.Fatalf("unexpected past-EOF result: %q, %v", out, err)
	}
}

func TestReadFileEmptyFile(t *testing.T) {
	path := writeTestFile(t, t.TempDir(), "f.txt", "")
	tool := NewReadFileTool(NewToolRegistry(nil))
	out, err := tool.Execute(context.Background(), map[string]any{"path": path})
	if err != nil || out != "File has no content at or after line 1." {
		t.Fatalf("unexpected empty-file result: %q, %v", out, err)
	}
}

func TestReadFileErrors(t *testing.T) {
	dir := t.TempDir()
	tool := NewReadFileTool(NewToolRegistry(nil))
	if _, err := tool.Execute(context.Background(), map[string]any{}); err == nil {
		t.Fatal("expected error for missing path")
	}
	if _, err := tool.Execute(context.Background(), map[string]any{"path": filepath.Join(dir, "absent")}); err == nil {
		t.Fatal("expected error for missing file")
	}
	if _, err := tool.Execute(context.Background(), map[string]any{"path": dir}); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatal("expected directory error")
	}
}

func TestReadFileRefusesBinary(t *testing.T) {
	path := writeTestFile(t, t.TempDir(), "blob.bin", "PK\x03\x04\x00\x00binary")
	tool := NewReadFileTool(NewToolRegistry(nil))
	out, err := tool.Execute(context.Background(), map[string]any{"path": path})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "binary") || !strings.Contains(out, "view_image") {
		t.Fatalf("expected binary refusal pointing at view_image, got %q", out)
	}
}

func TestReadFileHonorsSandboxDenyPaths(t *testing.T) {
	denied := t.TempDir()
	path := writeTestFile(t, denied, "secret.txt", "top secret")
	registry := stubSandboxRegistry(t, sandbox.Config{DenyPaths: []string{denied}})
	tool := NewReadFileTool(registry)
	_, err := tool.Execute(context.Background(), map[string]any{"path": path})
	if err == nil || !strings.Contains(err.Error(), "sandbox policy") {
		t.Fatalf("expected sandbox denial, got %v", err)
	}
}

func TestReadFileUnsandboxedUnrestricted(t *testing.T) {
	// Without a sandbox factory the registry applies no read policy, matching
	// bash running unsandboxed.
	path := writeTestFile(t, t.TempDir(), "open.txt", "visible")
	tool := NewReadFileTool(NewToolRegistry(nil))
	if _, err := tool.Execute(context.Background(), map[string]any{"path": path}); err != nil {
		t.Fatalf("expected unsandboxed read to succeed, got %v", err)
	}
}

func TestReadFileLoadsFromRegistryWithoutSandbox(t *testing.T) {
	registry := NewToolRegistry(nil)
	if _, err := registry.LoadToolAuto("read_file"); err != nil {
		t.Fatalf("expected read_file to load without a sandbox, got %v", err)
	}
}
