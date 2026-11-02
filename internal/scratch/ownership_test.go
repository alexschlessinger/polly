package scratch

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestReleaseSlotAfterRootChange(t *testing.T) {
	isolateRoot(t)
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
	if err := ReleaseSlot(slot, old); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{old, old + ownerSuffix, current, current + ownerSuffix} {
		if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("release kept %s: %v", path, err)
		}
	}
	if err := ReleaseSlot(slot, old); err != nil {
		t.Fatalf("repeat release: %v", err)
	}
}

func TestReleaseOwnedRefusesUntrustedPaths(t *testing.T) {
	for _, kind := range []string{"wrong_owner", "missing_record", "symlink_record", "symlink_root", "wrong_name", "root_itself"} {
		t.Run(kind, func(t *testing.T) {
			root := isolateRoot(t)
			slot := filepath.Join(t.TempDir(), "slot-0000")
			dir, err := Claim(slot)
			if err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(dir, "keep")
			if err := os.WriteFile(marker, []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			candidate := dir
			switch kind {
			case "wrong_owner":
				if err := os.WriteFile(dir+ownerSuffix, []byte(filepath.Join(t.TempDir(), "slot-0000")), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing_record", "symlink_record":
				if err := os.Remove(dir + ownerSuffix); err != nil {
					t.Fatal(err)
				}
				if kind == "symlink_record" {
					outside := filepath.Join(t.TempDir(), "owner")
					if err := os.WriteFile(outside, []byte(slot), 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(outside, dir+ownerSuffix); err != nil {
						t.Skip(err)
					}
				}
			case "symlink_root":
				alias := filepath.Join(t.TempDir(), "alias")
				if err := os.Symlink(root, alias); err != nil {
					t.Skip(err)
				}
				candidate = filepath.Join(alias, filepath.Base(dir))
			case "wrong_name":
				candidate = filepath.Join(root, "unrelated")
				if err := os.Rename(dir, candidate); err != nil {
					t.Fatal(err)
				}
				marker = filepath.Join(candidate, "keep")
			case "root_itself":
				candidate = root
			}
			isolateRoot(t)
			if err := ReleaseOwned(candidate, slot); err == nil {
				t.Fatal("untrusted scratch accepted")
			}
			if data, err := os.ReadFile(marker); err != nil || string(data) != "keep" {
				t.Fatalf("untrusted scratch modified: %s, %v", data, err)
			}
		})
	}
}

func TestSweepFormerRootKeepsLiveOwnersAndUntrustedRecords(t *testing.T) {
	// The platform's former default remains discoverable without taking any
	// caller-provided directory as a new root to scan.
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("TMP", os.Getenv("TMPDIR"))
	t.Setenv("TEMP", os.Getenv("TMPDIR"))
	t.Setenv(RootEnv, RootUnder(os.TempDir()))
	living, err := Claim(filepath.Join(t.TempDir(), "slot-0000"))
	if err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(t.TempDir(), "gone", "slot-0000")
	orphan, err := Claim(gone)
	if err != nil {
		t.Fatal(err)
	}
	parked, err := Park(orphan, "held-test", filepath.Join(filepath.Dir(gone), "held-test"))
	if err != nil {
		t.Fatal(err)
	}
	// Park leaves the source slot's record; recreate its directory to cover
	// ordinary orphan claims and held directories in the same sweep.
	if _, err := Claim(gone); err != nil {
		t.Fatal(err)
	}
	root := Root()
	untrusted := filepath.Join(root, "unrelated")
	if err := os.Mkdir(untrusted, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(untrusted+ownerSuffix, []byte(gone), 0600); err != nil {
		t.Fatal(err)
	}
	isolateRoot(t)
	if err := Sweep(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{living, living + ownerSuffix, untrusted, untrusted + ownerSuffix} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("sweep removed %s: %v", path, err)
		}
	}
	for _, path := range []string{orphan, orphan + ownerSuffix, parked, parked + ownerSuffix} {
		if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("sweep kept %s: %v", path, err)
		}
	}
}
