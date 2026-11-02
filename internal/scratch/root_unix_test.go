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

func TestReleaseOwnedRefusesNonexclusiveRootAndRecord(t *testing.T) {
	for _, record := range []bool{false, true} {
		t.Run(map[bool]string{false: "root", true: "record"}[record], func(t *testing.T) {
			root := isolateRoot(t)
			slot := filepath.Join(t.TempDir(), "slot-0000")
			dir, err := Claim(slot)
			if err != nil {
				t.Fatal(err)
			}
			path, mode := root, os.FileMode(0755)
			if record {
				path, mode = dir+ownerSuffix, 0644
			}
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
			if err := ReleaseOwned(dir, slot); err == nil {
				t.Fatal("release trusted a nonexclusive ownership boundary")
			}
			if _, err := os.Stat(dir); err != nil {
				t.Fatalf("release removed untrusted scratch: %v", err)
			}
		})
	}
}

func TestReleaseSlotDoesNotRequireAccessToUnrecordedFormerRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv(RootEnv, RootUnder(os.TempDir()))
	formerRoot := Root()
	slot := filepath.Join(t.TempDir(), "slot-0000")
	old, err := Claim(slot)
	if err != nil {
		t.Fatal(err)
	}
	isolateRoot(t)
	current, err := Claim(slot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(formerRoot, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(formerRoot, 0700)
	if err := ReleaseSlot(slot, ""); err != nil {
		t.Fatalf("nested cleanup required access to a masked host root: %v", err)
	}
	if _, err := os.Stat(current); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("current scratch retained: %v", err)
	}
	if err := ReleaseSlot(slot, old); err == nil {
		t.Fatal("explicitly retained scratch silently skipped")
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

// A stat of the owner that fails for any reason other than absence says
// nothing about the owner, so a sweep leaves the scratch alone.
func TestSweepKeepsAScratchWhoseOwnerCannotBeStatted(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	isolateRoot(t)
	area := t.TempDir()
	runtime := filepath.Join(area, "runtime")
	if err := os.MkdirAll(runtime, 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := Claim(filepath.Join(runtime, "slot-0000"))
	if err != nil {
		t.Fatal(err)
	}
	defer Release(dir)
	// The owner is on disk but its enclosing directory refuses lookups, so the
	// stat fails with a permission error rather than a missing path.
	if err := os.Chmod(area, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(area, 0o700)
	if _, err := os.Stat(runtime); err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("owner stat = %v, want a permission error", err)
	}
	if err := Sweep(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dir, dir + ownerSuffix} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("sweep removed %s of a scratch whose owner could not be checked: %v", path, err)
		}
	}
}
