package worktree

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/internal/scratch"
)

func formerScratchRoot(t *testing.T) string {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("TMP", os.Getenv("TMPDIR"))
	t.Setenv("TEMP", os.Getenv("TMPDIR"))
	root := scratch.RootUnder(os.TempDir())
	t.Setenv(scratch.RootEnv, root)
	return root
}

func TestFinishCleanupMigratedScratch(t *testing.T) {
	for _, phase := range []string{"checkout", "tree_removed", "claim_removed", "discard"} {
		t.Run(phase, func(t *testing.T) {
			formerScratchRoot(t)
			m, root := fixture(t)
			ctx := context.Background()
			base, err := m.Capture(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			c, err := m.Create(ctx, base)
			if err != nil {
				t.Fatal(err)
			}
			old := c.ScratchDir()
			writeTest(t, filepath.Join(old, "retained"), "scratch")
			if phase == "tree_removed" || phase == "claim_removed" {
				gitTest(t, root, "worktree", "remove", "--force", c.Path)
			}
			if phase == "claim_removed" {
				if err := os.Remove(filepath.Join(filepath.Dir(c.Path), "owner")); err != nil {
					t.Fatal(err)
				}
			}
			// A checkout written before paths were persisted still discovers
			// its previous TMPDIR-based root through the masked private roots.
			c.Scratch = ""
			t.Setenv(scratch.RootEnv, filepath.Join(t.TempDir(), "new-root"))
			if phase == "discard" {
				writeTest(t, filepath.Join(c.Path, "a.txt"), "disposable changes")
				err = m.FinishDiscard(ctx, c)
			} else {
				err = m.FinishCleanup(ctx, c, "")
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{old, old + ".owner", c.Path, filepath.Join(filepath.Dir(c.Path), "owner"), filepath.Join(m.Directory, c.ID+".json")} {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatalf("finish kept %s: %v", path, err)
				}
			}
		})
	}
}

func TestNewReclaimsMigratedScratchOfStaleSlotsOnly(t *testing.T) {
	formerScratchRoot(t)
	m, root := fixture(t)
	ctx := context.Background()
	base, err := m.Capture(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	live, err := m.Create(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	stale := m.Slots[1]
	if err := os.Mkdir(stale, 0700); err != nil {
		t.Fatal(err)
	}
	owner := filepath.Join(stale, "owner")
	writeTest(t, owner, "abandoned")
	old := time.Now().Add(-2 * staleClaim)
	if err := os.Chtimes(owner, old, old); err != nil {
		t.Fatal(err)
	}
	orphan, err := scratch.Claim(stale)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(scratch.RootEnv, filepath.Join(t.TempDir(), "new-root"))
	if _, err := New(ctx, m.Config); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{orphan, orphan + ".owner", owner} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("recovery kept stale %s: %v", path, err)
		}
	}
	for _, path := range []string{live.Path, live.ScratchDir(), live.ScratchDir() + ".owner"} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("recovery removed live %s: %v", path, err)
		}
	}
}

func TestFinishCleanupMigratedScratchPreservesReusedSlot(t *testing.T) {
	t.Setenv(scratch.RootEnv, filepath.Join(t.TempDir(), "old-root"))
	m, root := fixture(t)
	ctx := context.Background()
	base, err := m.Capture(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	c, err := m.Create(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	old := c.ScratchDir()
	writeTest(t, filepath.Join(old, "keep"), "scratch")
	t.Setenv(scratch.RootEnv, filepath.Join(t.TempDir(), "new-root"))
	current, err := scratch.Claim(filepath.Dir(c.Path))
	if err != nil {
		t.Fatal(err)
	}
	writeTest(t, filepath.Join(filepath.Dir(c.Path), "owner"), "new-owner")
	if err := m.FinishDiscard(ctx, c); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{c.Path, old, current, old + ".owner", current + ".owner"} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("old release touched reused slot %s: %v", path, err)
		}
	}
}
