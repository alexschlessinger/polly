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
					if statErr != nil || after.Contexts[copy.ID] == nil || after.Contexts[copy.ID].Retiring {
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

type retirementHookSession struct {
	sessions.CoordinationSession
	afterCommit func() error
}

func (s *retirementHookSession) UpdateCoordination(ctx context.Context, fn func(*sessions.CoordinationState) error) error {
	err := s.CoordinationSession.UpdateCoordination(ctx, fn)
	if err == nil && s.afterCommit != nil {
		hook := s.afterCommit
		s.afterCommit = nil
		return hook()
	}
	return err
}

func TestContextCleanupRecordsRetirementBeforeFilesChange(t *testing.T) {
	for _, via := range []string{"direct", "workflow"} {
		for _, failure := range []string{"caller_canceled", "lost_commit_reply"} {
			t.Run(via+"/"+failure, func(t *testing.T) {
				r, p := applyFixture(t, false)
				ref := submittedInput(t, r, p.Parent, nil)
				cleanup := contextCleanupCaller(t, r, via, ref.Task)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				before, _ := r.read(ctx)
				copy := before.Contexts[ref.Task]
				hooked := false
				r.parent = &retirementHookSession{CoordinationSession: r.parent, afterCommit: func() error {
					hooked = true
					state, err := r.read(context.Background())
					if err != nil {
						t.Fatal(err)
					}
					if !state.Contexts[copy.ID].Retiring || state.Members[ref.Task].Control != MemberControlRetired || state.Tasks[ref.Task].StartingSnapshot != copy.Checkout.Base.ID {
						t.Fatal("retirement or provenance was not durable before cleanup")
					}
					if _, err := os.Stat(copy.Root); err != nil {
						t.Fatal("files removed before retirement", err)
					}
					if failure == "lost_commit_reply" {
						return errors.New("lost retirement commit reply")
					}
					cancel()
					return nil
				}}
				err := cleanup(ctx, copy.ID)
				if !hooked {
					t.Fatal("cleanup never recorded retirement")
				}
				after, _ := r.read(context.Background())
				if failure == "caller_canceled" {
					if err != nil || after.Contexts[copy.ID] != nil {
						t.Fatalf("cancellation stranded retirement: %v", err)
					}
				} else {
					if err == nil || after.Contexts[copy.ID] == nil || !after.Contexts[copy.ID].Retiring {
						t.Fatal("lost reply erased retirement state")
					}
					if _, err := r.makeContext(context.Background(), r.ID, AgentRequest{Context: copy.ID, ReadOnly: true}); err == nil {
						t.Fatal("interrupted retirement permitted reuse")
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
		if copy == nil || copy.Retiring {
			t.Fatal("refused family cleanup retired a copy")
		}
		if _, err := os.Stat(copy.Root); err != nil {
			t.Fatal("refused family cleanup removed files", err)
		}
	}
}
