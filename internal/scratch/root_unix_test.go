//go:build unix

package scratch

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureRootRefusesAWorldReachableRoot(t *testing.T) {
	root := isolateRoot(t)
	EnsureRootMust(t)
	info, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("scratch root mode = %04o, want 0700", info.Mode().Perm())
	}
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureRoot(); err == nil {
		t.Fatal("accepted a scratch root other users can reach")
	}
	if _, err := EnsureNestedRoot(); err == nil {
		t.Fatal("nested root accepted inside a scratch root other users can reach")
	}
}

// A scratch that cannot be removed keeps its ownership record, the only
// thing a later Sweep can find it by.
func TestReleaseKeepsTheRecordOfAScratchItCouldNotRemove(t *testing.T) {
	root := isolateRoot(t)
	dir, err := Claim(filepath.Join(t.TempDir(), "slot-0000"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o500); err != nil {
		t.Fatal(err)
	}
	restore := func() { os.Chmod(root, 0o700) }
	defer restore()
	if err := Release(dir); err == nil {
		t.Fatal("Release removed a directory from a read-only root")
	}
	if _, err := os.Stat(dir + ownerSuffix); err != nil {
		t.Fatalf("Release dropped the ownership record of a scratch it left behind: %v", err)
	}
	restore()
	if err := Release(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir + ownerSuffix); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("record survived a successful release: %v", err)
	}
}
