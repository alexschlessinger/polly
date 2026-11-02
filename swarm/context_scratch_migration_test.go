package swarm

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/alexschlessinger/pollytool/internal/scratch"
)

func TestContextScratchCleanupAfterRootMigration(t *testing.T) {
	// Setenv keeps this serial: both restored live contexts and old checkout
	// manifests must use the retained path, not the new process's scratch root.
	for _, git := range []bool{false, true} {
		for _, disposable := range []bool{false, true} {
			name := map[bool]string{false: "live", true: "checkout"}[git]
			if disposable {
				name += "/discard"
			}
			t.Run(name, func(t *testing.T) {
				t.Setenv(scratch.RootEnv, filepath.Join(t.TempDir(), "old-root"))
				r := scratchRuntime(t, doneModel(), git)
				suspendAutoRelease(t, r)
				ctx := context.Background()
				if _, err := r.Agent(ctx, "", AgentRequest{Label: "test", Task: "inspect", ReadOnly: true, Tools: []string{}}); err != nil {
					t.Fatal(err)
				}
				s, err := r.State(ctx)
				if err != nil {
					t.Fatal(err)
				}
				c := onlyContext(t, s)
				seedReadOnlyScratchCache(t, c.Scratch)
				if err := r.update(ctx, func(s *State) error {
					stored := s.Contexts[c.ID]
					stored.Disposable = disposable
					if stored.Checkout != nil {
						stored.Checkout.Scratch = "" // pre-migration checkout schema
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if err := r.Close(); err != nil {
					t.Fatal(err)
				}
				t.Setenv(scratch.RootEnv, filepath.Join(t.TempDir(), "new-root"))
				recovered, _, err := newRuntime(r.config)
				if err != nil {
					t.Fatal(err)
				}
				defer recovered.Close()
				suspendAutoRelease(t, recovered)
				if err := recovered.Cleanup(ctx, c.ID); err != nil {
					t.Fatal(err)
				}
				after, err := recovered.State(ctx)
				if err != nil || after.Contexts[c.ID] != nil {
					t.Fatalf("context retained: %v", err)
				}
				for _, path := range []string{c.Scratch, c.Scratch + ".owner"} {
					if _, err := os.Lstat(path); !os.IsNotExist(err) {
						t.Fatalf("cleanup dropped context but kept %s: %v", path, err)
					}
				}
			})
		}
	}
}

func TestContextScratchMigrationRefusesWrongOwnerAndRetainsRelease(t *testing.T) {
	for _, git := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "checkout"}[git], func(t *testing.T) {
			t.Setenv(scratch.RootEnv, filepath.Join(t.TempDir(), "old-root"))
			r := scratchRuntime(t, doneModel(), git)
			suspendAutoRelease(t, r)
			ctx := context.Background()
			if _, err := r.Agent(ctx, "", AgentRequest{Label: "test", Task: "inspect", ReadOnly: true, Tools: []string{}}); err != nil {
				t.Fatal(err)
			}
			s, err := r.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			c := onlyContext(t, s)
			owner, err := os.ReadFile(c.Scratch + ".owner")
			if err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(c.Scratch, "keep")
			if err := os.WriteFile(marker, []byte("retained"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(c.Scratch+".owner", []byte(filepath.Join(t.TempDir(), "foreign-slot")), 0600); err != nil {
				t.Fatal(err)
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			t.Setenv(scratch.RootEnv, filepath.Join(t.TempDir(), "new-root"))
			recovered, _, err := newRuntime(r.config)
			if err != nil {
				t.Fatal(err)
			}
			defer recovered.Close()
			suspendAutoRelease(t, recovered)
			if err := recovered.Cleanup(ctx, c.ID); err == nil {
				t.Fatal("cleanup accepted another slot's scratch")
			}
			after, err := recovered.State(ctx)
			if err != nil || after.Contexts[c.ID] == nil || after.Contexts[c.ID].Release != WorkspaceReleasing {
				t.Fatalf("failed cleanup lost its release obligation: %+v, %v", after, err)
			}
			if data, err := os.ReadFile(marker); err != nil || string(data) != "retained" {
				t.Fatalf("wrong-owner scratch modified: %s, %v", data, err)
			}
			if err := os.WriteFile(c.Scratch+".owner", owner, 0600); err != nil {
				t.Fatal(err)
			}
			if err := recovered.Cleanup(ctx, c.ID); err != nil {
				t.Fatalf("retry cleanup after restoring ownership: %v", err)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("retry retained scratch: %v", err)
			}
		})
	}
}
