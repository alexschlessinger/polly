package tools

import (
	"context"
	"errors"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestReadContextFileCompleteBoundedRegular(t *testing.T) {
	r := NewToolRegistry(nil)
	path := writeTestFile(t, t.TempDir(), "file", "hello\nworld")
	_, data, err := r.ReadContextFile(context.Background(), path, 11)
	if err != nil || string(data) != "hello\nworld" {
		t.Fatalf("read: %q %v", data, err)
	}
	if _, _, err := r.ReadContextFile(context.Background(), path, 10); err == nil {
		t.Fatal("oversize silently truncated")
	}
	if _, _, err := r.ReadContextFile(context.Background(), filepath.Dir(path), 100); err == nil {
		t.Fatal("directory accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := r.ReadContextFile(ctx, path, 100); err == nil {
		t.Fatal("canceled read accepted")
	}
}
func TestContextFilePathsHonorsIgnore(t *testing.T) {
	r := NewToolRegistry(nil, WithUnsafeNoSandbox())
	defer r.Close()
	root := t.TempDir()
	writeTestFile(t, root, ".gitignore", "ignored\n")
	// .git is excluded independently of ignore rules.
	os.Mkdir(filepath.Join(root, ".git"), 0700)
	writeTestFile(t, root, "ignored", "secret")
	writeTestFile(t, root, "keep.go", "package main")
	paths, err := r.ContextFilePaths(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(paths, "\n")
	if strings.Contains(joined, "ignored") || !strings.Contains(joined, "keep.go") {
		t.Fatalf("paths = %v", paths)
	}
}

func TestReadContextFileDeniesSymlinkTarget(t *testing.T) {
	skipIfWindows(t)
	denied := t.TempDir()
	root := t.TempDir()
	secret := writeTestFile(t, denied, "secret", "hidden")
	visible := writeTestFile(t, root, "visible", "shown")
	link := filepath.Join(root, "link")
	if err := os.Symlink(visible, link); err != nil {
		t.Fatal(err)
	}
	r := stubSandboxRegistry(t, sandbox.Config{DenyPaths: []string{denied}})
	if _, data, err := r.ReadContextFile(context.Background(), link, 100); err != nil || string(data) != "shown" {
		t.Fatalf("stable link: %q %v", data, err)
	}
	os.Remove(link)
	os.Symlink(secret, link)
	if _, _, err := r.ReadContextFile(context.Background(), link, 100); err == nil {
		t.Fatal("denied symlink target read")
	}
	if _, _, err := r.ReadContextFile(context.Background(), secret, 100); err == nil {
		t.Fatal("direct denied file read")
	}
}

func TestContextFilePathsReadsNestedIgnoreFilesWithoutCommands(t *testing.T) {
	t.Setenv("PATH", "")
	root := t.TempDir()
	fixtures := map[string]string{
		".gitignore": "*.log\n!keep.log\n/root.txt\nbuild/\n!build/keep.txt\nassets/**/cache?.bin\n\\#literal\n\\!literal\n",
		"skip.log":   "", "keep.log": "", "root.txt": "", "#literal": "", "!literal": "",
		"sub/.gitignore": "!nested.log\n*.tmp\n",
		"sub/nested.log": "", "sub/skip.log": "", "sub/root.txt": "", "sub/local.tmp": "",
		"other/local.tmp": "", "other/skip.log": "", "build/keep.txt": "",
		"assets/a/cache1.bin": "", "assets/cache2.bin": "", "assets/a/keep.bin": "",
		".git/config": "", "visible.txt": "",
	}
	for name, content := range fixtures {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	r := NewToolRegistry(nil)
	paths, err := r.ContextFilePaths(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{".gitignore", "assets/a/keep.bin", "keep.log", "other/local.tmp", "sub/.gitignore", "sub/nested.log", "sub/root.txt", "visible.txt"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths = %q; want %q", paths, want)
	}
}
func TestContextFilePathsPlainFolderAndCancellation(t *testing.T) {
	t.Setenv("PATH", "")
	root := t.TempDir()
	writeTestFile(t, root, "a.txt", "a")
	r := NewToolRegistry(nil)
	paths, err := r.ContextFilePaths(context.Background(), root)
	if err != nil || !reflect.DeepEqual(paths, []string{"a.txt"}) {
		t.Fatalf("plain folder: %q %v", paths, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.ContextFilePaths(ctx, root); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
}
func TestContextFilePathsDoesNotVisitDeniedDirectories(t *testing.T) {
	root := t.TempDir()
	denied := filepath.Join(root, "denied")
	os.Mkdir(denied, 0700)
	writeTestFile(t, denied, "secret", "secret")
	writeTestFile(t, root, "visible", "visible")
	r := stubSandboxRegistry(t, sandbox.Config{DenyPaths: []string{denied}})
	paths, err := r.ContextFilePaths(context.Background(), root)
	if err != nil || !reflect.DeepEqual(paths, []string{"visible"}) {
		t.Fatalf("paths = %q %v", paths, err)
	}
}
