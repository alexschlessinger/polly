package swarm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/workflow"
	"github.com/alexschlessinger/pollytool/worktree"
)

func releaseRecordsFixture(t *testing.T, history int) (*Runtime, sessions.CoordinationSession) {
	t.Helper()
	r := runtimeTest(t, doneModel(), 1, 4)
	suspendAutoRelease(t, r)
	if err := r.update(t.Context(), func(s *State) error {
		for i := range history {
			id := fmt.Sprintf("history-%04d", i)
			s.Tasks[id] = &Task{ID: id, Owner: "selected", Status: "done", Snapshot: id}
			s.Snapshots[id] = &worktree.Snapshot{ID: id, Source: r.config.Root, Tree: id, Commit: id}
			s.Executions[id] = &Execution{ID: id, Member: "selected", Workflow: "owner", Status: "completed", Workspace: id}
			s.Applies[id] = &ApplyRecord{ID: id, Status: "applied", Tasks: []TaskReference{{Task: id}}}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return r, r.parent
}

func seedReleaseRecords(t *testing.T, r *Runtime, parent sessions.CoordinationSession, id string) {
	t.Helper()
	r.parent = parent
	if err := r.updateRecords(t.Context(), func(tx *recordTx) error {
		if err := tx.PutMember(&Member{ID: id, Context: id, Execution: id, Task: id, Controller: "owner"}); err != nil {
			return err
		}
		if err := tx.PutExecution(&Execution{ID: id, Member: id, Workspace: id, Workflow: "owner", Generation: 7, Status: "completed"}); err != nil {
			return err
		}
		if err := tx.PutTask(&Task{ID: id, Owner: id, Status: "done"}); err != nil {
			return err
		}
		return tx.PutContext(&ExecutionContext{ID: id, Owner: id, Root: r.config.Root, SandboxPresets: []string{"read-only"}})
	}); err != nil {
		t.Fatal(err)
	}
}

type releaseMeasuredSession struct {
	*followupCountedSession
	metrics *runtimemetrics.Recorder
}

func (s *releaseMeasuredSession) ReadRecords(ctx context.Context, fn func(sessions.CoordinationReader) error) error {
	return s.followupCountedSession.ReadRecords(runtimemetrics.WithRecorder(ctx, s.metrics), fn)
}

func (s *releaseMeasuredSession) UpdateRecords(ctx context.Context, fn func(sessions.CoordinationTx) error) (sessions.CoordinationCommit, error) {
	return s.followupCountedSession.UpdateRecords(runtimemetrics.WithRecorder(ctx, s.metrics), fn)
}

func TestRecordReleaseExactBoundedAndLegacyParity(t *testing.T) {
	t.Parallel()
	baselines := map[string][6]int64{}
	nativeWork := map[string][2]uint64{}
	for _, legacy := range []bool{false, true} {
		for _, history := range []int{4, 256} {
			t.Run(fmt.Sprintf("legacy=%v/history=%d", legacy, history), func(t *testing.T) {
				r, parent := releaseRecordsFixture(t, history)
				t.Cleanup(func() { r.parent = parent })
				for _, mode := range []string{"automatic", "targeted", "cleanup", "discard", "workflow"} {
					seedReleaseRecords(t, r, parent, "selected")
					counted := &followupCountedSession{CoordinationSession: parent, records: parent.(sessions.CoordinationRecords), counts: new(followupAccessCounts)}
					metrics := runtimemetrics.New()
					if legacy {
						r.parent = repositoryLegacyOnly{parent}
					} else {
						r.parent = &releaseMeasuredSession{followupCountedSession: counted, metrics: metrics}
					}
					ctx := runtimemetrics.WithRecorder(t.Context(), metrics)
					var err error
					switch mode {
					case "automatic":
						var n int
						n, err = r.releaseWorkspaces(ctx)
						if err == nil && n != 1 {
							t.Fatalf("released %d contexts", n)
						}
					case "targeted":
						_, err = r.releaseWorkspace(ctx, "selected")
					case "cleanup":
						err = r.Cleanup(ctx, "selected")
					case "discard":
						err = r.Discard(ctx, "selected")
					case "workflow":
						_, err = (&workflowHost{runtime: r, controller: "owner"}).release(ctx, "selected")
					}
					if err != nil {
						t.Fatalf("%s: %v", mode, err)
					}
					c := counted.counts
					got := [6]int64{c.reads.Load(), c.writes.Load(), c.gets.Load(), c.queries.Load(), c.rows.Load(), c.broad.Load()}
					if !legacy {
						if c.writes.Load() != 2 || c.broad.Load() != 0 {
							t.Fatalf("%s did not preserve two exact transactions: %v", mode, got)
						}
						if history == 4 {
							baselines[mode] = got
						} else if got != baselines[mode] {
							t.Fatalf("%s history-dependent native work: small=%v large=%v", mode, baselines[mode], got)
						}
						o := metrics.Snapshot()
						work := [2]uint64{o.Count(runtimemetrics.RowsRead), o.Count(runtimemetrics.SQLStatements)}
						if history == 4 {
							nativeWork[mode] = work
						} else if work != nativeWork[mode] {
							t.Fatalf("%s native rows/statements grew with completed history: %v -> %v", mode, nativeWork[mode], work)
						}
						t.Logf("%s history=%d counts=%v native release including finish rows=%d bytes=%d SQL=%d", mode, history, got, o.Count(runtimemetrics.RowsRead), o.Count(runtimemetrics.PayloadBytesRead), o.Count(runtimemetrics.SQLStatements))
					}
					if err := r.readRecords(t.Context(), func(rd *recordReader) error {
						c, err := rd.Context("selected")
						if err != nil {
							return err
						}
						m, err := rd.Member("selected")
						if c != nil || m.Context != "" || !reflect.DeepEqual(m.SandboxPresets, []string{"read-only"}) {
							t.Fatalf("%s lost release/floor receipt: context=%+v member=%+v", mode, c, m)
						}
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	}
}

func TestRecordReleaseAdvancesPastLockedFirstPage(t *testing.T) {
	t.Parallel()
	r, parent := releaseRecordsFixture(t, 0)
	for i := range releaseBatchSize + 3 {
		seedReleaseRecords(t, r, parent, fmt.Sprintf("copy-%03d", i))
	}
	var held []func()
	for i := range releaseBatchSize {
		lock := r.contextMutex(fmt.Sprintf("copy-%03d", i))
		lock.Lock()
		held = append(held, lock.Unlock)
	}
	defer func() {
		for _, unlock := range held {
			unlock()
		}
	}()
	if n, err := r.releaseWorkspaces(t.Context()); err != nil || n != 3 {
		t.Fatalf("locked first page starved later rows: n=%d err=%v", n, err)
	}
	for _, unlock := range held {
		unlock()
	}
	held = nil
	// No release notification is sent here: the bounded retry must itself
	// reconsider eligible locked contexts rather than wait for another hint.
	r.releaseMu.Lock()
	r.releaseStopped = false
	r.releaseMu.Unlock()
	r.scheduleRelease()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	awaitState(t, r, ctx, func(s *State) bool { return len(s.Contexts) == 0 })
}

func TestRecordReleaseMidSweepInsertionContinuesPastCount(t *testing.T) {
	t.Parallel()
	for _, freshHint := range []bool{false, true} {
		t.Run(fmt.Sprintf("freshHintDuringContinuation=%v", freshHint), func(t *testing.T) {
			r, parent := releaseRecordsFixture(t, 0)
			for i := range releaseBatchSize {
				seedReleaseRecords(t, r, parent, fmt.Sprintf("a-%03d", i))
			}
			seedReleaseRecords(t, r, parent, "z-old")
			unlock, err := r.lockMaintenance(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			first, done, err := r.nextReleaseBatch(t.Context())
			if err != nil || done {
				unlock()
				t.Fatalf("first batch: done=%v err=%v", done, err)
			}
			_, err = r.releaseBatchLocked(t.Context(), first, true)
			unlock()
			if err != nil {
				t.Fatal(err)
			}
			// Do not replace r.parent here: releasing the first page has spawned
			// member wake goroutines. Insert through the unchanged native handle.
			if err := r.updateRecords(t.Context(), func(tx *recordTx) error {
				for _, id := range []string{"m-inserted", "zz-new"} {
					if id == "zz-new" && !freshHint {
						continue
					}
					status := "completed"
					if id == "m-inserted" {
						status = "running"
					}
					if err := tx.PutMember(&Member{ID: id, Context: id, Execution: id}); err != nil {
						return err
					}
					if err := tx.PutExecution(&Execution{ID: id, Member: id, Workspace: id, Status: status}); err != nil {
						return err
					}
					if err := tx.PutContext(&ExecutionContext{ID: id, Owner: id, Root: r.config.Root}); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if freshHint {
				unlock, err := r.lockMaintenance(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				_, done, err := r.nextReleaseBatch(t.Context())
				continuing := r.releaseScan.sweep != nil
				unlock()
				if err != nil || !done || !continuing {
					t.Fatalf("truncated finite sweep lost continuation: %v %v %v", done, continuing, err)
				}
			}
			r.releaseMu.Lock()
			r.releaseStopped = false
			r.releaseMu.Unlock()
			r.scheduleRelease()
			stopped := make(chan struct{})
			go func() { r.wg.Wait(); close(stopped) }()
			select {
			case <-stopped:
			case <-time.After(3 * time.Second):
				t.Fatal("release worker did not finish bounded continuation")
			}
			for _, id := range []string{"z-old", "zz-new"} {
				if id == "zz-new" && !freshHint {
					continue
				}
				p, err := r.readReleaseProof(t.Context(), id)
				if err != nil || p.context != nil {
					t.Fatalf("original tail/fresh hint stranded %s: %+v %v", id, p, err)
				}
			}
			p, err := r.readReleaseProof(t.Context(), "m-inserted")
			if err != nil || p.context == nil {
				t.Fatalf("ineligible insertion was released: %+v %v", p, err)
			}
		})
	}
}

func TestRecordReleasePartitionFairnessAndFiniteCutoff(t *testing.T) {
	t.Parallel()
	r, parent := releaseRecordsFixture(t, 0)
	for i := range releaseBatchSize * 2 {
		seedReleaseRecords(t, r, parent, fmt.Sprintf("copy-%03d", i))
	}
	seedReleaseRecords(t, r, parent, "receipt")
	if err := r.updateRecords(t.Context(), func(tx *recordTx) error {
		c, err := tx.Context("receipt")
		if err != nil {
			return err
		}
		c.Release = WorkspaceReleasing
		return tx.PutContext(c)
	}); err != nil {
		t.Fatal(err)
	}
	unlock, err := r.lockMaintenance(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	first, done, err := r.nextReleaseBatch(t.Context())
	if err != nil || done || len(first) != releaseBatchSize {
		t.Fatalf("first page: %d %v %v", len(first), done, err)
	}
	seedReleaseRecords(t, r, parent, "zz-new-after-cutoff")
	second, done, err := r.nextReleaseBatch(t.Context())
	if err != nil || done || len(second) != releaseBatchSize || second[0].ID != "receipt" {
		t.Fatalf("receipt partition starved: %d %v %v", len(second), done, err)
	}
	third, done, err := r.nextReleaseBatch(t.Context())
	if err != nil || !done || len(third) != 1 || third[0].ID == "zz-new-after-cutoff" {
		t.Fatalf("sweep did not honor finite cutoff: %+v %v %v", third, done, err)
	}
}

// Injects a lost native commit reply at a selected mark/removal transaction.
// The backend still commits; release must not publish or replay its tentative
// callback results just because the operation can reread a durable receipt.
type releaseUncertainSession struct {
	sessions.CoordinationSession
	sessions.CoordinationRecords
	writes  int
	lose    int
	failure error
}

func (s *releaseUncertainSession) UpdateRecords(ctx context.Context, fn func(sessions.CoordinationTx) error) (sessions.CoordinationCommit, error) {
	s.writes++
	commit, err := s.CoordinationRecords.UpdateRecords(ctx, fn)
	if err == nil && s.writes == s.lose {
		return commit, s.failure
	}
	return commit, err
}

type releaseUncertainLegacySession struct {
	sessions.CoordinationSession
	writes, lose int
	failure      error
}

func (s *releaseUncertainLegacySession) UpdateCoordination(ctx context.Context, fn func(*sessions.CoordinationState) error) error {
	s.writes++
	err := s.CoordinationSession.UpdateCoordination(ctx, fn)
	if err == nil && s.writes == s.lose {
		return s.failure
	}
	return err
}

func TestRecordReleaseUncertainMarkAndRemovalDoNotReplay(t *testing.T) {
	t.Parallel()
	for _, legacy := range []bool{false, true} {
		for _, lose := range []int{1, 2} {
			t.Run(fmt.Sprintf("legacy=%v/commit=%d", legacy, lose), func(t *testing.T) {
				r, parent := releaseRecordsFixture(t, 0)
				seedReleaseRecords(t, r, parent, "selected")
				failure := errors.New("lost release commit reply")
				var writes *int
				if legacy {
					wrapped := &releaseUncertainLegacySession{CoordinationSession: parent, lose: lose, failure: failure}
					r.parent, writes = wrapped, &wrapped.writes
				} else {
					wrapped := &releaseUncertainSession{CoordinationSession: parent, CoordinationRecords: parent.(sessions.CoordinationRecords), lose: lose, failure: failure}
					r.parent, writes = wrapped, &wrapped.writes
				}
				t.Cleanup(func() { r.parent = parent })
				if _, err := r.releaseWorkspaces(t.Context()); !errors.Is(err, failure) {
					t.Fatalf("lost reply became success: %v", err)
				}
				if *writes != lose || !r.releaseQuarantined("selected") {
					t.Fatalf("uncertain release replayed/not quarantined: writes=%d", *writes)
				}
				if n, err := r.releaseWorkspaces(t.Context()); err != nil || n != 0 || *writes != lose {
					t.Fatalf("automatic replay: %d %v writes=%d", n, err, *writes)
				}
				p, err := r.readReleaseProof(t.Context(), "selected")
				if err != nil {
					t.Fatal(err)
				}
				if lose == 1 && (p.context == nil || p.context.Release != WorkspaceReleasing) || lose == 2 && p.context != nil {
					t.Fatalf("durable receipt lost: %+v", p.context)
				}
				// Explicit selection is allowed to reconcile a known receipt.
				if _, err := r.releaseWorkspace(t.Context(), "selected"); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestRecordReleaseProofIdentityChanges(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"owner", "root", "retained", "releasing", "generation", "reassignment"} {
		t.Run(change, func(t *testing.T) {
			r, parent := releaseRecordsFixture(t, 0)
			seedReleaseRecords(t, r, parent, "selected")
			counted := &followupCountedSession{CoordinationSession: parent, records: parent.(sessions.CoordinationRecords), counts: new(followupAccessCounts)}
			counted.before = func(tx *recordTx) error {
				c, err := tx.Context("selected")
				if err != nil {
					return err
				}
				switch change {
				case "owner":
					c.Owner = "replacement"
				case "root":
					c.Root += "-replacement"
				case "retained":
					c.Release = WorkspaceRetained
				case "releasing":
					c.Release = WorkspaceReleasing
				case "generation":
					e, err := tx.Execution("selected")
					if err != nil {
						return err
					}
					e.Generation++
					return tx.PutExecution(e)
				case "reassignment":
					m, err := tx.Member("selected")
					if err != nil {
						return err
					}
					m.Task = "new-task"
					return tx.PutMember(m)
				}
				return tx.PutContext(c)
			}
			r.parent = counted
			t.Cleanup(func() { r.parent = parent })
			if n, err := r.releaseWorkspaces(t.Context()); err != nil || n != 0 {
				t.Fatalf("changed proof removed: %d %v", n, err)
			}
			p, err := r.readReleaseProof(t.Context(), "selected")
			if err != nil || p.context == nil {
				t.Fatalf("replacement lost: %+v %v", p, err)
			}
		})
	}
}

func TestRecordReleaseReadOnlyDirtyTreeCannotUseDoneSnapshot(t *testing.T) {
	t.Parallel()
	r, p := applyFixture(t, false)
	suspendAutoRelease(t, r)
	ref := submittedInput(t, r, p.Parent, map[string]string{"a.txt": "dirty\n"})
	if err := r.update(t.Context(), func(s *State) error {
		c := s.Contexts[ref.Task]
		c.ReadOnly = true
		s.Tasks[ref.Task].Status = "done"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.Cleanup(t.Context(), ref.Task); err == nil {
		t.Fatal("read-only dirty workspace accepted an editing snapshot")
	}
	s, err := r.read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	c := s.Contexts[ref.Task]
	if c == nil {
		t.Fatal("dirty workspace record removed")
	}
	if data, err := os.ReadFile(filepath.Join(c.Root, "a.txt")); err != nil || string(data) != "dirty\n" {
		t.Fatalf("dirty workspace files changed: %q %v", data, err)
	}
}

func TestRecordReleaseWorkflowContextReceiptExact(t *testing.T) {
	t.Parallel()
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%v", legacy), func(t *testing.T) {
			r, parent := releaseRecordsFixture(t, 0)
			if err := r.updateRecords(t.Context(), func(tx *recordTx) error {
				for i := range 128 {
					if err := tx.PutWorkflowStep("owner", i, &workflow.Step{Kind: "context", Status: "completed", Value: fmt.Sprintf("other-%03d", i)}); err != nil {
						return err
					}
				}
				if err := tx.PutWorkflowStep("owner", 128, &workflow.Step{Kind: "context", Status: "completed", Value: "selected"}); err != nil {
					return err
				}
				return tx.PutContext(&ExecutionContext{ID: "selected", Owner: "owner", Root: r.config.Root, Disposable: true})
			}); err != nil {
				t.Fatal(err)
			}
			counted := &followupCountedSession{CoordinationSession: parent, records: parent.(sessions.CoordinationRecords), counts: new(followupAccessCounts)}
			if legacy {
				r.parent = repositoryLegacyOnly{parent}
			} else {
				r.parent = counted
			}
			t.Cleanup(func() { r.parent = parent })
			h := &workflowHost{runtime: r, controller: "owner"}
			if _, err := h.release(t.Context(), "selected"); err != nil {
				t.Fatal(err)
			}
			value, err := h.release(t.Context(), "selected")
			if err != nil || value.(map[string]any)["dormant"] != true {
				t.Fatalf("exact completed context receipt unavailable: %v %v", value, err)
			}
			if _, err := (&workflowHost{runtime: r, controller: "other"}).release(t.Context(), "selected"); err == nil {
				t.Fatal("context receipt granted another workflow authority")
			}
			if err := r.updateRecords(t.Context(), func(tx *recordTx) error {
				return tx.PutWorkflowStep("owner", 128, &workflow.Step{Kind: "context", Status: "completed", Value: map[string]any{"context": "selected"}})
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := h.release(t.Context(), "selected"); err == nil {
				t.Fatal("non-string step value granted authority")
			}
		})
	}
}
