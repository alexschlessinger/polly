package swarm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
	err := r.Cleanup(ctx, "")
	candidateError(t, err, "unintegrated_changes")
	// A capture error names only the copy's root, so the refusal names the
	// context a later command can be given.
	if !strings.Contains(err.Error(), "context "+dirty.Task+": ") {
		t.Fatalf("refusal does not name the context: %v", err)
	}
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

// A copy cleanup cannot prove stays until the user discards it. The refusal
// names the command; a discard removes the files and the record whatever they
// hold, under cleanup's other refusals; and it is recorded with the release,
// so a retry after an interrupted finish discards instead of asking for the
// proof again.
func TestDiscardReleasesACopyCleanupCannotProve(t *testing.T) {
	r, p := applyFixture(t, false)
	suspendAutoRelease(t, r)
	ctx := context.Background()
	dirty := submittedInput(t, r, p.Parent, map[string]string{"a.txt": "unintegrated\n"})
	lost := submittedInput(t, r, p.Parent, map[string]string{"a.txt": "unintegrated too\n"})
	before, _ := r.read(ctx)
	roots := map[string]string{dirty.Task: before.Contexts[dirty.Task].Root, lost.Task: before.Contexts[lost.Task].Root}
	err := r.Cleanup(ctx, dirty.Task)
	candidateError(t, err, "unintegrated_changes")
	if !strings.Contains(err.Error(), "/swarm discard "+dirty.Task) {
		t.Fatalf("refusal does not offer the discard: %v", err)
	}
	if err := r.Discard(ctx, ""); err == nil {
		t.Fatal("discard without a context ID was accepted")
	}
	for name, mark := range map[string]func(){
		"member":   func() { r.active["member"] = &invocation{} },
		"workflow": func() { r.workflowCancels["workflow"] = func() {} },
	} {
		r.mu.Lock()
		mark()
		r.mu.Unlock()
		err := r.Discard(ctx, dirty.Task)
		r.mu.Lock()
		delete(r.active, "member")
		delete(r.workflowCancels, "workflow")
		r.mu.Unlock()
		if err == nil {
			t.Fatalf("discard ran beside an active %s", name)
		}
	}
	if _, err := os.Stat(roots[dirty.Task]); err != nil {
		t.Fatal("refused discard removed files", err)
	}
	if err := r.Discard(ctx, dirty.Task); err != nil {
		t.Fatal(err)
	}
	// The release commit of the second discard lands but its reply is lost,
	// which leaves the record releasing with its files in place.
	r.parent = &releaseHookSession{CoordinationSession: r.parent, afterCommit: func() error {
		return errors.New("lost release commit reply")
	}}
	if err := r.Discard(ctx, lost.Task); err == nil {
		t.Fatal("lost release reply was not reported")
	}
	interrupted, _ := r.read(ctx)
	if c := interrupted.Contexts[lost.Task]; c == nil || c.Release == "" || !c.Disposable {
		t.Fatalf("interrupted discard was not recorded with its release: %+v", c)
	}
	if err := r.Cleanup(ctx, lost.Task); err != nil {
		t.Fatalf("retry of an interrupted discard: %v", err)
	}
	after, _ := r.read(ctx)
	for _, ref := range []TaskReference{dirty, lost} {
		if after.Contexts[ref.Task] != nil || after.Members[ref.Task].Context != "" {
			t.Fatalf("discarded context %s is still recorded", ref.Task)
		}
		if _, err := os.Stat(roots[ref.Task]); !os.IsNotExist(err) {
			t.Fatalf("discarded copy's files remain: %v", err)
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
