package safefile

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// resolvedTempDir returns a temp dir whose spelling contains no symlink, since
// OpenRegular deliberately refuses symlinked components (macOS places
// t.TempDir under the /var -> /private/var link).
func resolvedTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestOpenRegularReadsRegularFile(t *testing.T) {
	dir := resolvedTempDir(t)
	path := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := OpenRegular(path, os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("OpenRegular: %v", err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Fatalf("read %q, want hello", data)
	}
}

func TestOpenRegularCreatesAndWrites(t *testing.T) {
	dir := resolvedTempDir(t)
	path := filepath.Join(dir, "new.txt")
	f, err := OpenRegular(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatalf("OpenRegular: %v", err)
	}
	if _, err := f.WriteString("created"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "created" {
		t.Fatalf("read %q, want created", data)
	}
}

func TestOpenRegularRejectsDirectory(t *testing.T) {
	dir := resolvedTempDir(t)
	_, err := OpenRegular(dir, os.O_RDONLY, 0)
	var notRegular *NotRegularError
	if !errors.As(err, &notRegular) || !notRegular.Mode.IsDir() {
		t.Fatalf("OpenRegular(dir) error = %v, want NotRegularError for a directory", err)
	}
}

func TestOpenRegularReportsMissingFile(t *testing.T) {
	dir := resolvedTempDir(t)
	_, err := OpenRegular(filepath.Join(dir, "missing"), os.O_RDONLY, 0)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("OpenRegular(missing) error = %v, want ErrNotExist", err)
	}
}

func TestOpenRegularRefusesSymlinkedFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink refusal is a unix behavior")
	}
	dir := resolvedTempDir(t)
	target := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(target, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenRegular(link, os.O_RDONLY, 0); err == nil {
		t.Fatal("OpenRegular followed a symlinked final component")
	}
	if _, err := OpenRegular(link, os.O_WRONLY|os.O_CREATE, 0o644); err == nil {
		t.Fatal("OpenRegular followed a symlinked final component for writing")
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "secret" {
		t.Fatalf("target changed: %q %v", data, err)
	}
}

func TestOpenRegularRefusesSymlinkedAncestor(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink refusal is a unix behavior")
	}
	dir := resolvedTempDir(t)
	real := filepath.Join(dir, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "file.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenRegular(filepath.Join(link, "file.txt"), os.O_RDONLY, 0); err == nil {
		t.Fatal("OpenRegular followed a symlinked ancestor")
	}
	// The resolved spelling opens normally.
	f, err := OpenRegular(filepath.Join(real, "file.txt"), os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("OpenRegular(resolved): %v", err)
	}
	f.Close()
}

func TestOpenRegularRequiresAbsolutePath(t *testing.T) {
	if _, err := OpenRegular("relative.txt", os.O_RDONLY, 0); err == nil {
		t.Fatal("OpenRegular accepted a relative path")
	}
}

func TestOpenDirectoryListsEntries(t *testing.T) {
	dir := resolvedTempDir(t)
	for _, name := range []string{"b.txt", "a.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f, err := OpenDirectory(dir)
	if err != nil {
		t.Fatalf("OpenDirectory: %v", err)
	}
	defer f.Close()
	entries, err := f.ReadDir(-1)
	if err != nil || len(entries) != 2 {
		t.Fatalf("ReadDir = %d entries, %v", len(entries), err)
	}
	if _, err := OpenDirectory(filepath.Join(dir, "a.txt")); err == nil {
		t.Fatal("OpenDirectory opened a regular file")
	}
	if _, err := OpenDirectory(filepath.Join(dir, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("OpenDirectory(missing) error = %v, want ErrNotExist", err)
	}
}

func TestOpenDirectoryRefusesSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink refusal is a unix behavior")
	}
	dir := resolvedTempDir(t)
	real := filepath.Join(dir, "real")
	if err := os.MkdirAll(filepath.Join(real, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenDirectory(link); err == nil {
		t.Fatal("OpenDirectory followed a symlinked final component")
	}
	if _, err := OpenDirectory(filepath.Join(link, "inner")); err == nil {
		t.Fatal("OpenDirectory followed a symlinked ancestor")
	}
	for _, path := range []string{real, filepath.Join(real, "inner"), "/"} {
		f, err := OpenDirectory(path)
		if err != nil {
			t.Fatalf("OpenDirectory(%s): %v", path, err)
		}
		if _, err := f.ReadDir(-1); err != nil {
			t.Fatalf("ReadDir(%s): %v", path, err)
		}
		f.Close()
	}
}
