package swarm

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/workflow"
)

type followupAccessCounts struct {
	reads, writes, gets, queries, rows, broad atomic.Int64
}

type followupCountedSession struct {
	sessions.CoordinationSession
	records sessions.CoordinationRecords
	counts  *followupAccessCounts
	before  func(*recordTx) error
	after   error
}

func (s *followupCountedSession) ReadCoordination(context.Context) (*sessions.CoordinationState, error) {
	s.counts.broad.Add(1)
	return nil, errors.New("unexpected full-family followup read")
}
func (s *followupCountedSession) UpdateCoordination(context.Context, func(*sessions.CoordinationState) error) error {
	s.counts.broad.Add(1)
	return errors.New("unexpected full-family followup update")
}
func (s *followupCountedSession) ReadRecords(ctx context.Context, fn func(sessions.CoordinationReader) error) error {
	s.counts.reads.Add(1)
	return s.records.ReadRecords(ctx, func(rd sessions.CoordinationReader) error {
		return fn(&followupCountedReader{CoordinationReader: rd, counts: s.counts})
	})
}
func (s *followupCountedSession) UpdateRecords(ctx context.Context, fn func(sessions.CoordinationTx) error) (sessions.CoordinationCommit, error) {
	s.counts.writes.Add(1)
	commit, err := s.records.UpdateRecords(ctx, func(tx sessions.CoordinationTx) error {
		if s.before != nil {
			before := s.before
			s.before = nil
			rd := &recordReader{raw: tx, ctx: ctx, active: true}
			defer func() { rd.active = false }()
			if err := before(&recordTx{recordReader: rd, tx: tx}); err != nil {
				return err
			}
		}
		return fn(&followupCountedTx{CoordinationTx: tx, followupCountedReader: &followupCountedReader{CoordinationReader: tx, counts: s.counts}})
	})
	if err == nil && s.after != nil {
		err, s.after = s.after, nil
	}
	return commit, err
}

type followupCountedReader struct {
	sessions.CoordinationReader
	counts *followupAccessCounts
}

func (r *followupCountedReader) Get(key sessions.RecordKey) (sessions.CoordinationRecord, bool, error) {
	r.counts.gets.Add(1)
	v, found, err := r.CoordinationReader.Get(key)
	if found {
		r.counts.rows.Add(1)
	}
	return v, found, err
}
func (r *followupCountedReader) Query(q sessions.RecordQuery) (sessions.RecordPage, error) {
	r.counts.queries.Add(1)
	if q.Index == "" || q.Index == sessions.CoordinationIndexRecords || q.Index == sessions.CoordinationIndexKind {
		return sessions.RecordPage{}, fmt.Errorf("unexpected broad followup query: %+v", q)
	}
	page, err := r.CoordinationReader.Query(q)
	r.counts.rows.Add(int64(len(page.Records)))
	return page, err
}

// Preserve optional native candidate access through the counting wrapper. The
// record fallback is deliberately slower; hiding this capability would measure
// a legacy history scan instead of native release's exact dependency work.
func (r *followupCountedReader) QueryCoordinationCandidates(q sessions.CoordinationCandidateQuery) (sessions.RecordPage, error) {
	r.counts.queries.Add(1)
	page, err := sessions.QueryCoordinationCandidates(r.CoordinationReader, q)
	r.counts.rows.Add(int64(len(page.Records)))
	return page, err
}

type followupCountedTx struct {
	sessions.CoordinationTx
	*followupCountedReader
}

func (tx *followupCountedTx) Identity() (sessions.CoordinationIdentity, error) {
	return tx.followupCountedReader.Identity()
}
func (tx *followupCountedTx) Get(key sessions.RecordKey) (sessions.CoordinationRecord, bool, error) {
	return tx.followupCountedReader.Get(key)
}
func (tx *followupCountedTx) Query(q sessions.RecordQuery) (sessions.RecordPage, error) {
	return tx.followupCountedReader.Query(q)
}
func (tx *followupCountedTx) Revisions(scopes []sessions.RevisionScope) ([]sessions.Revision, error) {
	return tx.followupCountedReader.Revisions(scopes)
}

func followupRecordsFixture(t *testing.T, unrelated int, legacy bool) (*Runtime, sessions.CoordinationSession, *followupCountedSession) {
	t.Helper()
	r := runtimeTest(t, doneModel(), 1, 9)
	suspendAutoRelease(t, r)
	if err := r.update(t.Context(), func(s *State) error {
		s.Runs["selected-run"] = &Run{ID: "selected-run", Status: "running", Starts: 3, Limit: 9}
		s.Members["selected-member"] = &Member{ID: "selected-member", AgentName: "/root/selected", ReadOnly: true, Task: "selected-task", Execution: "selected-execution"}
		s.Tasks["selected-task"] = &Task{ID: "selected-task", Owner: "selected-member", Execution: "selected-execution", Run: "selected-run", Status: "done", Requirement: RequirementDelivered, Revision: 4, AcceptedRevision: 4, SourceRoot: r.config.Root}
		s.Executions["selected-execution"] = &Execution{ID: "selected-execution", Member: "selected-member", Run: "selected-run", Status: "completed", Generation: 2, SourceRoot: r.config.Root}
		for i := range unrelated {
			id := fmt.Sprintf("unrelated-%04d", i)
			s.Members[id] = &Member{ID: id, Task: id, Execution: id, AgentName: "/root/" + id}
			s.Tasks[id] = &Task{ID: id, Owner: id, Execution: id, Run: id, Status: "pending", FollowupCallID: "same-call", Follows: id}
			s.Executions[id] = &Execution{ID: id, Member: id, Run: id, Workflow: id, Status: "completed"}
			s.Runs[id] = &Run{ID: id, Status: "completed"}
			s.Messages[id] = &Mail{ID: id, From: id, To: id, CallID: "same-call", Start: true}
			s.Followups[id] = &FollowupCall{Member: id, Refresh: true, Phase: "preparing"}
			s.Workflows[id] = &workflow.Report{ID: id, Run: id, Status: "failed"}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	original := r.parent
	counted := &followupCountedSession{CoordinationSession: original, records: original.(sessions.CoordinationRecords), counts: new(followupAccessCounts)}
	if legacy {
		r.parent = repositoryLegacyOnly{original}
	} else {
		r.parent = counted
	}
	// Cleanup remains cold and intentionally uses its ordinary full-state API.
	t.Cleanup(func() { r.parent = original })
	return r, original, counted
}

func TestRecordFollowupCreationReplayBounded(t *testing.T) {
	t.Parallel()
	var small [6]int64
	for _, legacy := range []bool{false, true} {
		for _, size := range []int{4, 512} {
			t.Run(fmt.Sprintf("legacy=%t/unrelated=%d", legacy, size), func(t *testing.T) {
				r, parent, counted := followupRecordsFixture(t, size, legacy)
				req := FollowupRequest{Task: "selected-task", Question: "inspect again", CallID: "same-call"}
				created, err := r.Followup(t.Context(), "", req)
				if err != nil {
					t.Fatal(err)
				}
				if created.Follows != req.Task || created.Owner != "selected-member" || created.Requirement != RequirementDelivered || created.Status != "pending" || created.Run != "selected-run" || created.SourceRoot != r.config.Root {
					t.Fatalf("wrong followup: %+v", created)
				}
				replay, err := r.Followup(t.Context(), "", req)
				if err != nil || !reflect.DeepEqual(created, replay) {
					t.Fatalf("replay: %+v %v", replay, err)
				}
				c := counted.counts
				got := [6]int64{c.reads.Load(), c.writes.Load(), c.gets.Load(), c.queries.Load(), c.rows.Load(), c.broad.Load()}
				if !legacy {
					if size == 4 {
						small = got
					} else if got != small {
						t.Fatalf("unrelated family amplified records: small=%v large=%v", small, got)
					}
					if c.broad.Load() != 0 {
						t.Fatalf("broad access: %v", got)
					}
				}
				raw, err := parent.ReadCoordination(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				s, err := decodeState(raw)
				if err != nil {
					t.Fatal(err)
				}
				if s.Tasks[req.Task].AcceptedRevision != 4 || s.Tasks[req.Task].Status != "done" || s.Runs["selected-run"].Starts != 3 || len(s.Tasks) != size+2 {
					t.Fatal("followup changed prior acceptance/accounting or duplicated task")
				}
			})
		}
	}
}

func TestRecordFollowupStaleUncertainCanceled(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"stale", "uncertain", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			r, parent, counted := followupRecordsFixture(t, 16, false)
			sentinel := errors.New("lost followup commit reply")
			ctx := t.Context()
			switch mode {
			case "stale":
				counted.before = func(tx *recordTx) error {
					task, err := tx.Task("selected-task")
					if err != nil {
						return err
					}
					task.Revision++
					return tx.PutTask(task)
				}
			case "uncertain":
				counted.after = sentinel
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			req := FollowupRequest{Task: "selected-task", Question: "inspect", CallID: "fault"}
			_, err := r.Followup(ctx, "", req)
			if err == nil {
				t.Fatal("fault was ignored")
			}
			if mode == "uncertain" {
				if !errors.Is(err, sentinel) || counted.counts.writes.Load() != 1 {
					t.Fatalf("uncertain write replayed: %v writes=%d", err, counted.counts.writes.Load())
				}
				if task, err := r.Followup(t.Context(), "", req); err != nil || task == nil {
					t.Fatalf("explicit durable replay: %+v %v", task, err)
				}
				if counted.counts.writes.Load() != 1 {
					t.Fatal("explicit replay wrote another task")
				}
			}
			raw, err := parent.ReadCoordination(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			s, err := decodeState(raw)
			if err != nil {
				t.Fatal(err)
			}
			want := 17
			if mode == "uncertain" {
				want++
			}
			if len(s.Tasks) != want || s.Runs["selected-run"].Starts != 3 {
				t.Fatalf("fault changed task count/budget: %d %+v", len(s.Tasks), s.Runs["selected-run"])
			}
		})
	}
}

func TestRecordFollowupDeferralExactFanout(t *testing.T) {
	t.Parallel()
	var smallRows int64
	for _, legacy := range []bool{false, true} {
		for _, size := range []int{4, 512} {
			t.Run(fmt.Sprintf("legacy=%t/unrelated=%d", legacy, size), func(t *testing.T) {
				r, parent, counted := followupRecordsFixture(t, size, legacy)
				if err := parent.UpdateCoordination(t.Context(), func(raw *sessions.CoordinationState) error {
					s, err := decodeState(raw)
					if err != nil {
						return err
					}
					s.Workflows["selected-workflow"] = &workflow.Report{ID: "selected-workflow", Run: "selected-run", Status: "failed"}
					s.Executions["selected-execution"].Workflow = "selected-workflow"
					s.Tasks["selected-task"].Status = "awaiting_review"
					s.Tasks["selected-task"].AcceptedRevision = 0
					// Same execution reverse relation is necessary but insufficient authority.
					s.Tasks["wrong-owner"] = &Task{ID: "wrong-owner", Execution: "selected-execution", Owner: "other", Run: "selected-run", Status: "pending"}
					return encodeState(raw, s)
				}); err != nil {
					t.Fatal(err)
				}
				if err := r.DeferWorkflow(t.Context(), "selected-workflow", "retain for later"); err != nil {
					t.Fatal(err)
				}
				if !legacy {
					rows := counted.counts.rows.Load()
					if size == 4 {
						smallRows = rows
					} else if rows != smallRows {
						t.Fatalf("unrelated deferral rows: %d -> %d", smallRows, rows)
					}
				}
				raw, err := parent.ReadCoordination(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				s, err := decodeState(raw)
				if err != nil {
					t.Fatal(err)
				}
				if !TaskDeferred(s, s.Tasks["selected-task"]) || s.Tasks["wrong-owner"].Deferral != nil || s.Tasks["selected-task"].AcceptedRevision != 0 || s.Workflows["unrelated-0000"].Acknowledged {
					t.Fatal("deferral lost authority or altered unrelated work")
				}
				s.Executions["selected-execution"].Generation++
				if TaskDeferred(s, s.Tasks["selected-task"]) {
					t.Fatal("deferral ignored generation fence")
				}
			})
		}
	}
}

func TestRecordFollowupRefreshReleaseBounded(t *testing.T) {
	t.Parallel()
	var small [5]int64
	for _, legacy := range []bool{false, true} {
		for _, size := range []int{4, 512} {
			t.Run(fmt.Sprintf("legacy=%t/unrelated=%d", legacy, size), func(t *testing.T) {
				r, parent, counted := followupRecordsFixture(t, size, legacy)
				if err := parent.UpdateCoordination(t.Context(), func(raw *sessions.CoordinationState) error {
					s, err := decodeState(raw)
					if err != nil {
						return err
					}
					s.Members["selected-member"].Context = "selected-context"
					s.Contexts["selected-context"] = &ExecutionContext{ID: "selected-context", Owner: "selected-member", Root: r.config.Root, ReadOnly: true, SandboxPresets: []string{"workspace"}}
					s.Followups["selected-mail"] = &FollowupCall{Member: "selected-member", Refresh: true, Phase: "preparing", PreviousTask: "selected-task", PreviousRevision: 4, Source: r.config.Root}
					s.Messages["selected-mail"] = &Mail{ID: "selected-mail", To: "selected-member", From: r.ID, Text: "refresh"}
					return encodeState(raw, s)
				}); err != nil {
					t.Fatal(err)
				}
				// The production caller holds both locks; queued wake checks cannot
				// run until after the authoritative handle is restored below.
				unlock, err := r.lockMaintenance(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer unlock()
				r.launchMu.Lock()
				err = r.prepareRefreshWorkspace(t.Context(), "selected-mail")
				c := counted.counts
				got := [5]int64{c.reads.Load(), c.writes.Load(), c.gets.Load(), c.queries.Load(), c.rows.Load()}
				r.parent = parent
				r.launchMu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
				if !legacy {
					if size == 4 {
						small = got
					} else if got != small {
						t.Fatalf("unrelated refresh-release amplification: %v -> %v", small, got)
					}
				}
				s, err := r.read(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if s.Contexts["selected-context"] != nil || s.Members["selected-member"].Context != "" || s.Tasks["selected-task"].AcceptedRevision != 4 || s.Followups["selected-mail"].Phase != "preparing" || s.Messages["selected-mail"].Start {
					t.Fatal("release changed acceptance or made preparation runnable")
				}
				if !reflect.DeepEqual(s.Members["selected-member"].SandboxPresets, []string{"workspace"}) {
					t.Fatal("release lost sandbox floor")
				}
			})
		}
	}
}

func TestRecordFollowupRollbackPreservesCommittedWorkspace(t *testing.T) {
	t.Parallel()
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			r, parent, _ := followupRecordsFixture(t, 16, legacy)
			if err := parent.UpdateCoordination(t.Context(), func(raw *sessions.CoordinationState) error {
				s, err := decodeState(raw)
				if err != nil {
					return err
				}
				s.Members["selected-member"].Context = "committed-context"
				s.Contexts["committed-context"] = &ExecutionContext{ID: "committed-context", Owner: "selected-member"}
				s.Contexts["unassigned-context"] = &ExecutionContext{ID: "unassigned-context", Owner: "selected-member"}
				return encodeState(raw, s)
			}); err != nil {
				t.Fatal(err)
			}
			r.rollbackWorkspaceWithScratch(&ExecutionContext{ID: "committed-context", Owner: "selected-member"}, "missing-mail")
			r.rollbackWorkspaceWithScratch(&ExecutionContext{ID: "unassigned-context", Owner: "selected-member"}, "missing-mail")
			raw, err := parent.ReadCoordination(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			s, err := decodeState(raw)
			if err != nil {
				t.Fatal(err)
			}
			if s.Contexts["committed-context"].Release != "" || s.Contexts["unassigned-context"].Release != WorkspaceReleasing {
				t.Fatal("rollback ignored committed assignment or lost cleanup obligation")
			}
		})
	}
}

func TestRecordFollowupRefreshEligibilityAndWorkspace(t *testing.T) {
	t.Parallel()
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			r, parent, _ := followupRecordsFixture(t, 512, legacy)
			var b *memberWorkspace
			if err := r.readRecords(t.Context(), func(rd *recordReader) error {
				m, err := rd.Member("selected-member")
				if err != nil {
					return err
				}
				f := &FollowupCall{PreviousTask: "selected-task", PreviousRevision: 4}
				if err := r.refreshEligibleRecords(rd, m, f, "", ""); err != nil {
					return err
				}
				f.PreviousRevision++
				if err := r.refreshEligibleRecords(rd, m, f, "", ""); err == nil {
					return errors.New("stale refresh was accepted")
				}
				task, err := rd.Task(m.Task)
				if err != nil {
					return err
				}
				b, err = rd.MemberWorkspace(m, task)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if b.Source != r.config.Root || b.Base != "" || b.Context != nil || b.Execution.Generation != 2 {
				t.Fatalf("workspace provenance: %+v", b)
			}
			if err := parent.UpdateCoordination(t.Context(), func(raw *sessions.CoordinationState) error {
				s, err := decodeState(raw)
				if err != nil {
					return err
				}
				s.Applies["unresolved"] = &ApplyRecord{ID: "unresolved", Status: "recovery_required", Tasks: []TaskReference{{Task: "selected-task", Revision: 4}}}
				return encodeState(raw, s)
			}); err != nil {
				t.Fatal(err)
			}
			if err := r.readRecords(t.Context(), func(rd *recordReader) error {
				m, err := rd.Member("selected-member")
				if err != nil {
					return err
				}
				return r.refreshEligibleRecords(rd, m, nil, "", "")
			}); err == nil {
				t.Fatal("unresolved exact task apply was ignored")
			}
		})
	}
}
