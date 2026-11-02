package swarm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/internal/scratch"
)

func TestLiveScratchCleanupAfterCapacityReduction(t *testing.T) {
	for _, migrate := range []bool{false, true} {
		t.Run(map[bool]string{false: "same_root", true: "new_root"}[migrate], func(t *testing.T) {
			t.Setenv(scratch.RootEnv, filepath.Join(t.TempDir(), "old-root"))
			r := rebuildRuntime(t, scratchRuntime(t, doneModel(), false), func(c *Config) { c.MaxWorktrees = 2 })
			suspendAutoRelease(t, r)
			ctx := context.Background()
			for _, label := range []string{"first", "second"} {
				if _, err := r.Agent(ctx, "", AgentRequest{Label: label, Task: "inspect", ReadOnly: true, Tools: []string{}}); err != nil {
					t.Fatal(err)
				}
			}
			s, err := r.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(s.Contexts) != 2 {
				t.Fatalf("contexts = %d, want two retained contexts", len(s.Contexts))
			}
			var first, second *ExecutionContext
			for _, c := range s.Contexts {
				switch {
				case strings.HasPrefix(filepath.Base(c.Scratch), "live-0000-"):
					first = c
				case strings.HasPrefix(filepath.Base(c.Scratch), "live-0001-"):
					second = c
				}
			}
			if first == nil || second == nil {
				t.Fatalf("missing expected live slots: %+v", s.Contexts)
			}
			seedReadOnlyScratchCache(t, second.Scratch)
			config := r.config
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			config.MaxWorktrees = 1
			if migrate {
				t.Setenv(scratch.RootEnv, filepath.Join(t.TempDir(), "new-root"))
			}
			recovered, _, err := newRuntime(config)
			if err != nil {
				t.Fatal(err)
			}
			defer recovered.Close()
			suspendAutoRelease(t, recovered)
			if err := recovered.Cleanup(ctx, second.ID); err != nil {
				t.Fatalf("cleanup above reduced capacity: %v", err)
			}
			after, err := recovered.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if after.Contexts[second.ID] != nil || after.Contexts[first.ID] == nil {
				t.Fatalf("cleanup removed the wrong context: %+v", after.Contexts)
			}
			for _, path := range []string{second.Scratch, second.Scratch + ".owner"} {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatalf("released scratch retained %s: %v", path, err)
				}
			}
			for _, path := range []string{first.Scratch, first.Scratch + ".owner"} {
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("sibling scratch removed %s: %v", path, err)
				}
			}
		})
	}
}

func TestLiveScratchCleanupRequiresCanonicalRuntimeSlot(t *testing.T) {
	for _, tc := range []struct {
		name    string
		slot    string
		foreign bool
		rename  bool
		valid   bool
	}{
		{name: "above_capacity", slot: "live-0001", valid: true},
		{name: "five_digits", slot: "live-10000", valid: true},
		{name: "short_index", slot: "live-1"},
		{name: "excess_padding", slot: "live-00001"},
		{name: "signed_index", slot: "live-+001"},
		{name: "negative_index", slot: "live--001"},
		{name: "overflow_index", slot: "live-999999999999999999999999"},
		{name: "extra_suffix", slot: "live-0001-extra"},
		{name: "checkout_slot", slot: "slot-0001"},
		{name: "foreign_runtime", slot: "live-0001", foreign: true},
		{name: "foreign_owner_matching_name", slot: "live-0001", foreign: true, rename: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(scratch.RootEnv, filepath.Join(t.TempDir(), "scratch-root"))
			r := &Runtime{config: Config{Directory: t.TempDir(), MaxWorktrees: 1}}
			dir, err := r.runtimeDirectory()
			if err != nil {
				t.Fatal(err)
			}
			ownerDir := dir
			if tc.foreign {
				ownerDir = canonicalPath(t, t.TempDir())
			}
			owned, err := scratch.Reserve(filepath.Join(ownerDir, tc.slot))
			if err != nil {
				t.Fatal(err)
			}
			if tc.rename {
				// A matching basename alone is insufficient: the protected
				// owner record must still identify this runtime's logical slot.
				forged := scratch.DirFor(filepath.Join(dir, tc.slot))
				if err := os.Rename(owned, forged); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(owned+".owner", forged+".owner"); err != nil {
					t.Fatal(err)
				}
				owned = forged
			}
			marker := filepath.Join(owned, "keep")
			if err := os.WriteFile(marker, []byte("retained"), 0600); err != nil {
				t.Fatal(err)
			}
			err = r.removeContextFiles(context.Background(), &ExecutionContext{Scratch: owned}, "")
			if tc.valid {
				if err != nil {
					t.Fatalf("valid canonical slot refused: %v", err)
				}
				for _, path := range []string{owned, owned + ".owner"} {
					if _, err := os.Lstat(path); !os.IsNotExist(err) {
						t.Fatalf("release kept %s: %v", path, err)
					}
				}
				return
			}
			if err == nil {
				t.Fatal("untrusted live slot accepted")
			}
			if data, err := os.ReadFile(marker); err != nil || string(data) != "retained" {
				t.Fatalf("untrusted scratch changed: %s, %v", data, err)
			}
			if _, err := os.Stat(owned + ".owner"); err != nil {
				t.Fatalf("untrusted ownership record removed: %v", err)
			}
		})
	}
}
