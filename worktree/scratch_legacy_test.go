package worktree

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/alexschlessinger/pollytool/internal/scratch"
)

func TestCleanupLegacySlotScratch(t *testing.T) {
	for _, phase := range []string{"cleanup", "finish_cleanup", "tree_removed", "claim_removed", "discard"} {
		t.Run(phase, func(t *testing.T) {
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
			if err := scratch.Release(c.ScratchDir()); err != nil {
				t.Fatal(err)
			}
			c.Scratch = filepath.Join(filepath.Dir(c.Path), "scratch")
			if err := os.Mkdir(c.Scratch, 0700); err != nil {
				t.Fatal(err)
			}
			writeTest(t, filepath.Join(c.Scratch, "retained"), "old scratch")
			if phase == "tree_removed" || phase == "claim_removed" {
				gitTest(t, root, "worktree", "remove", "--force", c.Path)
			}
			if phase == "claim_removed" {
				if err := os.Remove(filepath.Join(filepath.Dir(c.Path), "owner")); err != nil {
					t.Fatal(err)
				}
			}
			switch phase {
			case "cleanup":
				err = m.Cleanup(ctx, c, "")
			case "discard":
				writeTest(t, filepath.Join(c.Path, "a.txt"), "disposable changes")
				err = m.FinishDiscard(ctx, c)
			default:
				err = m.FinishCleanup(ctx, c, "")
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{c.Scratch, c.Path, filepath.Join(filepath.Dir(c.Path), "owner"), filepath.Join(m.Directory, c.ID+".json")} {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatalf("cleanup kept %s: %v", path, err)
				}
			}
			if err := m.FinishCleanup(ctx, c, ""); err != nil {
				t.Fatalf("repeat cleanup: %v", err)
			}
		})
	}
}

func TestLegacySlotScratchCleanupPreservesReusedOwner(t *testing.T) {
	for _, discard := range []bool{false, true} {
		t.Run(map[bool]string{false: "cleanup", true: "discard"}[discard], func(t *testing.T) {
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
			c.Scratch = filepath.Join(filepath.Dir(c.Path), "scratch")
			if err := os.Mkdir(c.Scratch, 0700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(c.Scratch, "keep")
			writeTest(t, marker, "new occupant scratch")
			owner := filepath.Join(filepath.Dir(c.Path), "owner")
			writeTest(t, owner, "new-owner")
			if discard {
				err = m.FinishDiscard(ctx, c)
			} else {
				err = m.FinishCleanup(ctx, c, "")
			}
			if err != nil {
				t.Fatal(err)
			}
			for path, want := range map[string]string{marker: "new occupant scratch", owner: "new-owner"} {
				if data, err := os.ReadFile(path); err != nil || string(data) != want {
					t.Fatalf("cleanup changed reused slot %s: %s, %v", path, data, err)
				}
			}
			if _, err := os.Stat(c.Path); err != nil {
				t.Fatalf("cleanup removed reused checkout: %v", err)
			}
		})
	}
}

func TestLegacySlotScratchCleanupRefusesOtherPaths(t *testing.T) {
	for _, kind := range []string{"outside", "sibling_slot", "noncanonical_slot", "symlink_slot"} {
		t.Run(kind, func(t *testing.T) {
			m, _ := fixture(t)
			// A release record can survive both tree and claim removal. Even
			// in that phase, only the exact managed slot layout is authority
			// for legacy scratch that has no adjacent .owner record.
			c := Checkout{ID: "released", Path: filepath.Join(m.Directory, "slot-0000", "tree")}
			keep := filepath.Join(t.TempDir(), "scratch")
			switch kind {
			case "sibling_slot":
				keep = filepath.Join(m.Directory, "slot-0001", "scratch")
			case "noncanonical_slot":
				c.Path = filepath.Join(m.Directory, "unmanaged", "tree")
				keep = filepath.Join(filepath.Dir(c.Path), "scratch")
			case "symlink_slot":
				if err := os.Symlink(filepath.Dir(keep), filepath.Dir(c.Path)); err != nil {
					t.Skip(err)
				}
			}
			if err := os.MkdirAll(keep, 0700); err != nil {
				t.Fatal(err)
			}
			c.Scratch = keep
			if kind == "symlink_slot" {
				c.Scratch = filepath.Join(filepath.Dir(c.Path), "scratch")
			}
			marker := filepath.Join(keep, "keep")
			writeTest(t, marker, "unowned scratch")
			if err := m.FinishCleanup(context.Background(), c, ""); err == nil {
				t.Fatal("cleanup accepted an untrusted legacy scratch path")
			}
			if data, err := os.ReadFile(marker); err != nil || string(data) != "unowned scratch" {
				t.Fatalf("cleanup changed untrusted scratch: %s, %v", data, err)
			}
		})
	}
}
