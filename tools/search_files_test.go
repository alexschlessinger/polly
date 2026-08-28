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

func searchTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeTestFile(t, dir, "main.go", "package main\n\nfunc main() {\n\tretryLoop()\n}\n")
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeTestFile(t, filepath.Join(dir, "sub"), "retry.go", "package sub\n\n// retryLoop retries.\nfunc retryLoop() {}\n")
	writeTestFile(t, dir, "notes.txt", "no matches here\n")
	return dir
}

func TestSearchFilesLiteralAcrossTree(t *testing.T) {
	dir := searchTree(t)
	tool := NewSearchFilesTool(NewToolRegistry(nil))
	out, err := tool.Execute(context.Background(), map[string]any{"pattern": "retryLoop", "path": dir})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, want := range []string{
		"main.go:4: \tretryLoop()",
		filepath.Join("sub", "retry.go") + ":3: // retryLoop retries.",
		filepath.Join("sub", "retry.go") + ":4: func retryLoop() {}",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "notes.txt") {
		t.Fatalf("unexpected non-matching file in results:\n%s", out)
	}
}

func TestSearchFilesIncludeGlob(t *testing.T) {
	dir := searchTree(t)
	tool := NewSearchFilesTool(NewToolRegistry(nil))
	out, err := tool.Execute(context.Background(), map[string]any{
		"pattern": "package", "path": dir, "include": "*.txt",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "No matches") {
		t.Fatalf("expected include glob to exclude .go files, got:\n%s", out)
	}
	out, err = tool.Execute(context.Background(), map[string]any{
		"pattern": "package", "path": dir, "include": "*.go",
	})
	if err != nil || !strings.Contains(out, "main.go:1: package main") {
		t.Fatalf("expected .go matches, got %q, %v", out, err)
	}
}

func TestSearchFilesRegex(t *testing.T) {
	dir := searchTree(t)
	tool := NewSearchFilesTool(NewToolRegistry(nil))
	out, err := tool.Execute(context.Background(), map[string]any{
		"pattern": `^func \w+\(\)`, "path": dir, "regex": true,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "main.go:3: func main() {") {
		t.Fatalf("unexpected regex results:\n%s", out)
	}
	_, err = tool.Execute(context.Background(), map[string]any{
		"pattern": `(?=lookahead)x`, "path": dir, "regex": true,
	})
	if err == nil || !strings.Contains(err.Error(), "RE2") {
		t.Fatalf("expected RE2 error guidance, got %v", err)
	}
}

func TestSearchFilesLimitCap(t *testing.T) {
	dir := t.TempDir()
	var lines strings.Builder
	for i := 0; i < 10; i++ {
		fmt.Fprintf(&lines, "needle %d\n", i)
	}
	writeTestFile(t, dir, "many.txt", lines.String())
	tool := NewSearchFilesTool(NewToolRegistry(nil))
	out, err := tool.Execute(context.Background(), map[string]any{"pattern": "needle", "path": dir, "limit": 3})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "results capped at 3 matching lines") || strings.Contains(out, "needle 3") {
		t.Fatalf("unexpected capped output:\n%s", out)
	}
}

func TestSearchFilesSkipsGitBinariesAndSymlinks(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "keep.txt", "needle\n")
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeTestFile(t, filepath.Join(dir, ".git"), "config", "needle\n")
	writeTestFile(t, dir, "blob.bin", "needle\x00needle")
	if err := os.Symlink(filepath.Join(dir, "keep.txt"), filepath.Join(dir, "alias.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	tool := NewSearchFilesTool(NewToolRegistry(nil))
	out, err := tool.Execute(context.Background(), map[string]any{"pattern": "needle", "path": dir})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "keep.txt:1: needle") {
		t.Fatalf("expected keep.txt match:\n%s", out)
	}
	for _, banned := range []string{".git", "blob.bin", "alias.txt"} {
		if strings.Contains(out, banned) {
			t.Fatalf("expected %s to be skipped:\n%s", banned, out)
		}
	}
}

func TestSearchFilesSingleFileAndOverlongLine(t *testing.T) {
	dir := t.TempDir()
	long := "needle " + strings.Repeat("x", 5000)
	path := writeTestFile(t, dir, "long.txt", long+"\n")
	tool := NewSearchFilesTool(NewToolRegistry(nil))
	out, err := tool.Execute(context.Background(), map[string]any{"pattern": "needle", "path": path})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, path+":1: needle") || !strings.Contains(out, "bytes]") {
		t.Fatalf("expected bounded single-file match:\n%s", out)
	}
	if len(out) > 1024 {
		t.Fatalf("display not bounded: %d bytes", len(out))
	}
}

func TestSearchFilesNoMatches(t *testing.T) {
	dir := searchTree(t)
	tool := NewSearchFilesTool(NewToolRegistry(nil))
	out, err := tool.Execute(context.Background(), map[string]any{"pattern": "absent-token", "path": dir})
	if err != nil || !strings.Contains(out, `No matches for "absent-token"`) {
		t.Fatalf("unexpected result: %q, %v", out, err)
	}
}

func TestSearchFilesArgumentErrors(t *testing.T) {
	dir := t.TempDir()
	tool := NewSearchFilesTool(NewToolRegistry(nil))
	if _, err := tool.Execute(context.Background(), map[string]any{"path": dir}); err == nil {
		t.Fatal("expected error for missing pattern")
	}
	if _, err := tool.Execute(context.Background(), map[string]any{"pattern": "a\nb", "path": dir}); err == nil {
		t.Fatal("expected error for multi-line literal pattern")
	}
	if _, err := tool.Execute(context.Background(), map[string]any{"pattern": "x", "path": dir, "limit": 0}); err == nil {
		t.Fatal("expected error for limit below 1")
	}
	if _, err := tool.Execute(context.Background(), map[string]any{"pattern": "x", "path": dir, "include": "[bad"}); err == nil {
		t.Fatal("expected error for invalid include glob")
	}
	if _, err := tool.Execute(context.Background(), map[string]any{"pattern": "x", "path": filepath.Join(dir, "absent")}); err == nil {
		t.Fatal("expected error for missing path")
	}
}

func TestSearchFilesSandboxSkipsDeniedSubtree(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "open.txt", "needle\n")
	secret := filepath.Join(dir, "secret")
	if err := os.Mkdir(secret, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeTestFile(t, secret, "hidden.txt", "needle\n")
	registry := stubSandboxRegistry(t, sandbox.Config{DenyPaths: []string{secret}})
	tool := NewSearchFilesTool(registry)
	out, err := tool.Execute(context.Background(), map[string]any{"pattern": "needle", "path": dir})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "open.txt:1: needle") || strings.Contains(out, "hidden.txt") {
		t.Fatalf("expected denied subtree to be skipped:\n%s", out)
	}
}

func TestSearchFilesSandboxDeniesRoot(t *testing.T) {
	denied := t.TempDir()
	writeTestFile(t, denied, "f.txt", "needle\n")
	registry := stubSandboxRegistry(t, sandbox.Config{DenyPaths: []string{denied}})
	tool := NewSearchFilesTool(registry)
	_, err := tool.Execute(context.Background(), map[string]any{"pattern": "needle", "path": denied})
	if err == nil || !strings.Contains(err.Error(), "sandbox policy") {
		t.Fatalf("expected root denial, got %v", err)
	}
}
