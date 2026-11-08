package swarm

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/worktree"
)

func TestRecordReleaseRechecksIdentityAfterMarkBeforeFiles(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"root", "retained", "generation", "member_context"} {
		t.Run(change, func(t *testing.T) {
			r, p, hook := hookedApplyFixture(t)
			suspendAutoRelease(t, r)
			ref := submittedInput(t, r, p.Parent, nil)
			if err := r.update(t.Context(), func(s *State) error {
				s.Tasks[ref.Task].Status = "done"
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			before, err := r.read(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			root := before.Contexts[ref.Task].Root
			hook.arm(func() error {
				return r.updateRecords(context.Background(), func(tx *recordTx) error {
					c, err := tx.Context(ref.Task)
					if err != nil {
						return err
					}
					if c.Release != WorkspaceReleasing {
						t.Fatal("hook did not run after mark")
					}
					switch change {
					case "root":
						c.Root += "-replaced"
					case "retained":
						c.Release = WorkspaceRetained
					case "generation":
						m, err := tx.Member(c.Owner)
						if err != nil {
							return err
						}
						if m.Execution == "" {
							m.Execution = "replacement"
							if err := tx.PutMember(m); err != nil {
								return err
							}
						}
						return tx.PutExecution(&Execution{ID: m.Execution, Member: m.ID, Workspace: c.ID, Status: "completed", Generation: 99})
					case "member_context":
						m, err := tx.Member(c.Owner)
						if err != nil {
							return err
						}
						m.Context = "replacement"
						return tx.PutMember(m)
					}
					return tx.PutContext(c)
				})
			})
			if n, err := r.releaseWorkspaces(t.Context()); err == nil || n != 0 || !strings.Contains(err.Error(), "workspace changed") {
				t.Fatalf("changed post-mark proof removed: %d %v", n, err)
			}
			if _, err := os.Stat(root); err != nil {
				t.Fatalf("original files removed after identity changed: %v", err)
			}
			s, err := r.read(t.Context())
			if err != nil || s.Contexts[ref.Task] == nil {
				t.Fatalf("replacement record lost: %v", err)
			}
		})
	}
}

func TestRecordRefreshRechecksFullContextBeforeMark(t *testing.T) {
	t.Parallel()
	r, parent, counted := followupRecordsFixture(t, 0, false)
	if err := parent.UpdateCoordination(t.Context(), func(raw *sessions.CoordinationState) error {
		s, err := decodeState(raw)
		if err != nil {
			return err
		}
		s.Members["selected-member"].Context = "selected-context"
		s.Contexts["selected-context"] = &ExecutionContext{ID: "selected-context", Owner: "selected-member", Root: r.config.Root, ReadOnly: true}
		s.Followups["selected-mail"] = &FollowupCall{Member: "selected-member", Refresh: true, Phase: "preparing", PreviousTask: "selected-task", PreviousRevision: 4, Source: r.config.Root}
		s.Messages["selected-mail"] = &Mail{ID: "selected-mail", To: "selected-member", From: r.ID, Text: "refresh"}
		return encodeState(raw, s)
	}); err != nil {
		t.Fatal(err)
	}
	counted.before = func(tx *recordTx) error {
		c, err := tx.Context("selected-context")
		if err != nil {
			return err
		}
		c.Root += "-replacement"
		return tx.PutContext(c)
	}
	unlock, err := r.lockMaintenance(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	r.launchMu.Lock()
	err = r.prepareRefreshWorkspace(t.Context(), "selected-mail")
	r.parent = parent
	r.launchMu.Unlock()
	if err == nil || !strings.Contains(err.Error(), "workspace changed") {
		t.Fatalf("changed refresh context accepted: %v", err)
	}
	p, err := r.readReleaseProof(t.Context(), "selected-context")
	if err != nil || p.context == nil || p.context.Release != "" {
		t.Fatalf("vetoed refresh released context: %+v %v", p, err)
	}
}

func TestRecordReleaseReleasingReceiptDoesNotOverrideLiveExecution(t *testing.T) {
	t.Parallel()
	r, parent := releaseRecordsFixture(t, 0)
	seedReleaseRecords(t, r, parent, "selected")
	if err := r.updateRecords(t.Context(), func(tx *recordTx) error {
		c, err := tx.Context("selected")
		if err != nil {
			return err
		}
		c.Release = WorkspaceReleasing
		if err := tx.PutContext(c); err != nil {
			return err
		}
		e, err := tx.Execution("selected")
		if err != nil {
			return err
		}
		e.Status, e.Generation = "running", e.Generation+1
		return tx.PutExecution(e)
	}); err != nil {
		t.Fatal(err)
	}
	if n, err := r.releaseWorkspaces(t.Context()); err != nil || n != 0 {
		t.Fatalf("old receipt overrode live generation: %d %v", n, err)
	}
}

func TestRecordReleaseDoneTreeProofIgnoresUnrelatedSnapshotHistory(t *testing.T) {
	t.Parallel()
	r, p := applyFixture(t, false)
	suspendAutoRelease(t, r)
	ref := submittedInput(t, r, p.Parent, map[string]string{"a.txt": "integrated\n"})
	if err := r.update(t.Context(), func(s *State) error { s.Tasks[ref.Task].Status = "done"; return nil }); err != nil {
		t.Fatal(err)
	}
	state, err := r.read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	c := state.Contexts[ref.Task]
	parent := r.parent
	t.Cleanup(func() { r.parent = parent })
	var small [3]int64
	for _, history := range []int{4, 128} {
		r.parent = parent
		if err := r.updateRecords(t.Context(), func(tx *recordTx) error {
			for i := range history {
				id := fmt.Sprintf("old-%03d", i)
				if err := tx.PutSnapshot(&worktree.Snapshot{ID: id, Source: c.Root, Tree: id, Commit: id}); err != nil {
					return err
				}
				if err := tx.PutTask(&Task{ID: id, Owner: ref.Task, Status: "done", Snapshot: id}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		counted := &followupCountedSession{CoordinationSession: parent, records: parent.(sessions.CoordinationRecords), counts: new(followupAccessCounts)}
		r.parent = counted
		tree, err := r.contextCleanupTreeRecords(t.Context(), c)
		if err != nil || tree != state.Snapshots[state.Tasks[ref.Task].Snapshot].Tree {
			t.Fatalf("exact done tree not proved: %s %v", tree, err)
		}
		counts := counted.counts
		got := [3]int64{counts.gets.Load(), counts.queries.Load(), counts.rows.Load()}
		if history == 4 {
			small = got
		} else if got != small {
			t.Fatalf("done-tree proof scanned old snapshots/tasks: %v -> %v", small, got)
		}
	}
}
