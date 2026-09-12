package scratch

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func skipIfWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX directory permissions and symlinks")
	}
}

func TestRemoveAllReadOnlyModuleCache(t *testing.T) {
	skipIfWindows(t)
	dir := filepath.Join(t.TempDir(), "scratch")
	module := filepath.Join(dir, "gopath", "pkg", "mod", "example@v1")
	pkg := filepath.Join(module, "pkg")
	if err := os.MkdirAll(pkg, 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(module, "go.mod"), filepath.Join(pkg, "source.go")} {
		if err := os.WriteFile(path, []byte("cached source"), 0444); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{pkg, module, dir} {
		if err := os.Chmod(path, 0555); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, path := range []string{dir, module, pkg} {
			os.Chmod(path, 0700)
		}
	})
	// Keep the symlink inside a read-only directory so the initial removal
	// cannot unlink it before the permission-repair walk sees it.
	outside := t.TempDir()
	file := filepath.Join(outside, "keep")
	if err := os.WriteFile(file, []byte("keep"), 0444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(outside, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(outside, 0700) })
	if err := os.Chmod(pkg, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(pkg, "outside")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(pkg, 0555); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("scratch remains: %v", err)
	}
	if info, err := os.Stat(outside); err != nil || info.Mode().Perm() != 0555 {
		t.Fatalf("external directory permissions changed: %v, %v", info, err)
	}
	if data, err := os.ReadFile(file); err != nil || string(data) != "keep" {
		t.Fatalf("external file changed: %q, %v", data, err)
	}
	if err := RemoveAll(dir); err != nil {
		t.Fatalf("repeated cleanup: %v", err)
	}
}

func TestRemoveAllRootSymlink(t *testing.T) {
	skipIfWindows(t)
	outside := t.TempDir()
	file := filepath.Join(outside, "keep")
	if err := os.WriteFile(file, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "scratch")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAll(link); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(link); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("scratch symlink remains: %v", err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("symlink target removed: %v", err)
	}
}
