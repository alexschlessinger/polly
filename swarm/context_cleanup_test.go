package swarm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/alexschlessinger/pollytool/sessions"
)

func contextCleanupCaller(t *testing.T, r *Runtime, via, member string) func(context.Context, string) error {
	t.Helper()
	suspendAutoRelease(t, r)
	if via == "direct" {
		return r.Cleanup
	}
	if err := r.update(context.Background(), func(s *State) error { s.Members[member].Controller = "workflow"; return nil }); err != nil {
		t.Fatal(err)
	}
	h := &workflowHost{runtime: r, controller: "workflow"}
	t.Cleanup(h.close)
	return func(ctx context.Context, id string) error { _, err := h.release(ctx, id); return err }
}

func TestContextCleanupRequiresCurrentIntegratedContents(t *testing.T) {
	for _, via := range []string{"direct", "workflow"} {
		for _, status := range []string{"unintegrated", "integrated", "edited_after_integration"} {
			t.Run(via+"/"+status, func(t *testing.T) {
				r, p := applyFixture(t, false)
				ctx := context.Background()
				ref := submittedInput(t, r, p.Parent, map[string]string{"a.txt": "change\n"})
				cleanup := contextCleanupCaller(t, r, via, ref.Task)
				if status != "unintegrated" {
					c, err := r.PrepareIntegration(ctx, []TaskReference{ref}, "")
					if err != nil {
						t.Fatal(err)
					}
					if _, err = r.AcceptIntegration(ctx, c.ID); err != nil {
						t.Fatal(err)
					}
					if _, err = r.ApplyIntegration(ctx, c.ID); err != nil {
						t.Fatal(err)
					}
				}
				if status == "unintegrated" && via == "workflow" {
					if err := r.CancelTask(ctx, ref.Task); err != nil {
						t.Fatal(err)
					}
				}
				before, _ := r.read(ctx)
				copy := before.Contexts[ref.Task]
				if status == "edited_after_integration" {
					if err := os.WriteFile(filepath.Join(copy.Root, "a.txt"), []byte("later edits\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				err := cleanup(ctx, copy.ID)
				after, _ := r.read(ctx)
				_, statErr := os.Stat(copy.Root)
				if status == "integrated" {
					if err != nil || after.Contexts[copy.ID] != nil || !os.IsNotExist(statErr) {
						t.Fatalf("integrated copy retained: %v %v", err, statErr)
					}
				} else {
					candidateError(t, err, "unintegrated_changes")
					if statErr != nil || after.Contexts[copy.ID] == nil || after.Contexts[copy.ID].Release != "" {
						t.Fatal("refused cleanup changed the copy")
					}
				}
				if after.Snapshots[before.Tasks[ref.Task].Snapshot] == nil || after.Snapshots[p.Parent.ID] == nil {
					t.Fatal("context cleanup deleted retained snapshots")
				}
			})
		}
	}
}

type releaseHookSession struct {
	sessions.CoordinationSession
	afterCommit func() error
}

func (s *releaseHookSession) UpdateCoordination(ctx context.Context, fn func(*sessions.CoordinationState) error) error {
	err := s.CoordinationSession.UpdateCoordination(ctx, fn)
	if err == nil && s.afterCommit != nil {
		hook := s.afterCommit
		s.afterCommit = nil
		return hook()
	}
	return err
}

func TestContextCleanupRecordsReleaseBeforeFilesChange(t *testing.T) {
	for _, via := range []string{"direct", "workflow"} {
		for _, failure := range []string{"caller_canceled", "lost_commit_reply"} {
			t.Run(via+"/"+failure, func(t *testing.T) {
				r, p := applyFixture(t, false)
				ref := submittedInput(t, r, p.Parent, nil)
				cleanup := contextCleanupCaller(t, r, via, ref.Task)
				if via == "workflow" {
					if _, err := r.Integrate(context.Background(), IntegrateRequest{Tasks: []TaskReference{ref}}); err != nil {
						t.Fatal(err)
					}
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				before, _ := r.read(ctx)
				copy := before.Contexts[ref.Task]
				hooked := false
				r.parent = &releaseHookSession{CoordinationSession: r.parent, afterCommit: func() error {
					hooked = true
					state, err := r.read(context.Background())
					if err != nil {
						t.Fatal(err)
					}
					if state.Contexts[copy.ID].Release == "" || state.Members[ref.Task].Control != MemberControlEnabled || state.Tasks[ref.Task].StartingSnapshot != copy.Checkout.Base.ID {
						t.Fatal("release or provenance was not durable before cleanup")
					}
					if _, err := os.Stat(copy.Root); err != nil {
						t.Fatal("files removed before release", err)
					}
					if failure == "lost_commit_reply" {
						return errors.New("lost release commit reply")
					}
					cancel()
					return nil
				}}
				err := cleanup(ctx, copy.ID)
				if !hooked {
					t.Fatal("cleanup never recorded release")
				}
				after, _ := r.read(context.Background())
				if failure == "caller_canceled" {
					if err != nil || after.Contexts[copy.ID] != nil {
						t.Fatalf("cancellation stranded release: %v", err)
					}
				} else {
					if err == nil || after.Contexts[copy.ID] == nil || after.Contexts[copy.ID].Release == "" {
						t.Fatal("lost reply erased release state")
					}
					if _, err := r.makeContext(context.Background(), r.ID, AgentRequest{Context: copy.ID, ReadOnly: true}); err == nil {
						t.Fatal("interrupted release permitted reuse")
					}
				}
			})
		}
	}
}

func TestWholeFamilyCleanupChecksEveryCopyBeforeRemovingAny(t *testing.T) {
	r, p := applyFixture(t, false)
	ctx := context.Background()
	clean := submittedInput(t, r, p.Parent, nil)
	dirty := submittedInput(t, r, p.Parent, map[string]string{"a.txt": "unintegrated\n"})
	candidateError(t, r.Cleanup(ctx, ""), "unintegrated_changes")
	state, _ := r.read(ctx)
	for _, ref := range []TaskReference{clean, dirty} {
		copy := state.Contexts[ref.Task]
		if copy == nil || copy.Release != "" {
			t.Fatal("refused family cleanup released a copy")
		}
		if _, err := os.Stat(copy.Root); err != nil {
			t.Fatal("refused family cleanup removed files", err)
		}
	}
}

type countingCoordinationSession struct {
	sessions.CoordinationSession
	updates int
	onFirst func()
}

func (s *countingCoordinationSession) UpdateCoordination(ctx context.Context, fn func(*sessions.CoordinationState) error) error {
	err := s.CoordinationSession.UpdateCoordination(ctx, fn)
	if err == nil {
		s.updates++
		if s.updates == 1 && s.onFirst != nil {
			s.onFirst()
		}
	}
	return err
}

// Whole-family cleanup records every release in one transaction, removes
// the copies, then deletes the records in one more; it does not pay two
// transactions per context.
func TestWholeFamilyCleanupBatchesTransactions(t *testing.T) {
	r, p := applyFixture(t, false)
	var refs []TaskReference
	for range 3 {
		refs = append(refs, submittedInput(t, r, p.Parent, nil))
	}
	ctx := context.Background()
	before, err := r.read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var roots []string
	for _, ref := range refs {
		roots = append(roots, before.Contexts[ref.Task].Root)
	}
	counter := &countingCoordinationSession{CoordinationSession: r.parent}
	counter.onFirst = func() {
		state, err := r.read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for i, ref := range refs {
			if c := state.Contexts[ref.Task]; c == nil || c.Release == "" || state.Members[ref.Task].Control != MemberControlEnabled {
				t.Fatalf("first commit did not record every release: %+v", c)
			}
			if _, err := os.Stat(roots[i]); err != nil {
				t.Fatalf("copy removed before its release was durable: %v", err)
			}
		}
	}
	r.parent = counter
	if err := r.Cleanup(ctx, ""); err != nil {
		t.Fatal(err)
	}
	// Mark and delete; task-owned snapshot references stay pinned.
	if counter.updates != 2 {
		t.Fatalf("whole-family cleanup used %d transactions for 3 contexts, want 2", counter.updates)
	}
	after, err := r.read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i, ref := range refs {
		if after.Contexts[ref.Task] != nil {
			t.Fatalf("context %s survived cleanup", ref.Task)
		}
		if _, err := os.Stat(roots[i]); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("copy %s survived cleanup: %v", roots[i], err)
		}
	}
}
