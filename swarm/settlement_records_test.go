package swarm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/workflow"
	"github.com/alexschlessinger/pollytool/worktree"
)

// Reject both whole-family methods and kind enumeration, and reject unrelated
// payloads even if a future implementation disguises its broad query as Get.
// Preserve only explicitly supplied native candidate capability on each reader.
type settlementGuard struct {
	sessions.CoordinationSession
	records       sessions.CoordinationRecords
	reads, writes atomic.Int64
	beforeUpdate  func()
	failAfterPut  bool
}

func (s *settlementGuard) CoordinationCapabilities() sessions.CoordinationCapabilities {
	return sessions.CoordinationCapabilities{NativeIndexed: true, DurableRevisions: true}
}
func (*settlementGuard) ReadCoordination(context.Context) (*sessions.CoordinationState, error) {
	return nil, errHotLegacy
}
func (*settlementGuard) UpdateCoordination(context.Context, func(*sessions.CoordinationState) error) error {
	return errHotLegacy
}
func (s *settlementGuard) ReadRecords(ctx context.Context, fn func(sessions.CoordinationReader) error) error {
	s.reads.Add(1)
	return s.records.ReadRecords(ctx, func(rd sessions.CoordinationReader) error {
		return fn(settlementGuardReader{rd})
	})
}
func (s *settlementGuard) UpdateRecords(ctx context.Context, fn func(sessions.CoordinationTx) error) (sessions.CoordinationCommit, error) {
	s.writes.Add(1)
	if s.beforeUpdate != nil {
		before := s.beforeUpdate
		s.beforeUpdate = nil
		before()
	}
	return s.records.UpdateRecords(ctx, func(tx sessions.CoordinationTx) error {
		return fn(settlementGuardTx{settlementGuardReader{tx}, tx, s})
	})
}

type settlementGuardReader struct{ sessions.CoordinationReader }

func (r settlementGuardReader) Get(key sessions.RecordKey) (sessions.CoordinationRecord, bool, error) {
	if strings.HasPrefix(key.ID, "retained-") {
		return sessions.CoordinationRecord{}, false, fmt.Errorf("unrelated payload: %v", key)
	}
	return r.CoordinationReader.Get(key)
}
func guardSettlementPage(page sessions.RecordPage, err error) (sessions.RecordPage, error) {
	if err == nil {
		for _, record := range page.Records {
			if strings.HasPrefix(record.Key.ID, "retained-") {
				return sessions.RecordPage{}, fmt.Errorf("unrelated query payload: %v", record.Key)
			}
		}
	}
	return page, err
}
func (r settlementGuardReader) Query(q sessions.RecordQuery) (sessions.RecordPage, error) {
	if q.Index == "" || q.Index == sessions.CoordinationIndexRecords || q.Index == sessions.CoordinationIndexKind {
		return sessions.RecordPage{}, errHotLegacy
	}
	return guardSettlementPage(r.CoordinationReader.Query(q))
}
func (r settlementGuardReader) QueryCoordinationCandidates(q sessions.CoordinationCandidateQuery) (sessions.RecordPage, error) {
	native, ok := r.CoordinationReader.(sessions.CoordinationCandidateReader)
	if !ok {
		return sessions.RecordPage{}, errors.New("missing native candidate capability")
	}
	return guardSettlementPage(native.QueryCoordinationCandidates(q))
}

func (r settlementGuardReader) SemanticCapabilities() sessions.CoordinationSemanticCapabilities {
	return r.CoordinationReader.(sessions.CoordinationSemanticReader).SemanticCapabilities()
}
func (r settlementGuardReader) SemanticMark() (sessions.SemanticMark, error) {
	return r.CoordinationReader.(sessions.CoordinationSemanticReader).SemanticMark()
}
func (r settlementGuardReader) SemanticPendingMail(actor string) (bool, error) {
	return r.CoordinationReader.(sessions.CoordinationSemanticReader).SemanticPendingMail(actor)
}
func (r settlementGuardReader) SemanticChanges(q sessions.SemanticComparison) (sessions.SemanticChanges, error) {
	return r.CoordinationReader.(sessions.CoordinationSemanticReader).SemanticChanges(q)
}

type settlementGuardTx struct {
	settlementGuardReader
	tx    sessions.CoordinationTx
	owner *settlementGuard
}

func (tx settlementGuardTx) Put(key sessions.RecordKey, payload json.RawMessage) error {
	if err := tx.tx.Put(key, payload); err != nil {
		return err
	}
	if tx.owner.failAfterPut {
		return errHotWrite
	}
	return nil
}
func (tx settlementGuardTx) Delete(key sessions.RecordKey) error          { return tx.tx.Delete(key) }
func (tx settlementGuardTx) Effects(e sessions.CoordinationEffects) error { return tx.tx.Effects(e) }

func guardSettlement(t *testing.T, r *Runtime, root sessions.CoordinationSession) *settlementGuard {
	t.Helper()
	guard := &settlementGuard{CoordinationSession: root, records: root.(sessions.CoordinationRecords)}
	r.parent = guard
	localWaitNotifications(t, r)
	return guard
}

func seedSettlement(t *testing.T, r *Runtime, mutate func(*State)) *State {
	t.Helper()
	var expected *State
	if err := r.update(t.Context(), func(s *State) error {
		mutate(s)
		expected = s
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return expected
}

func settlementDelivery(s *State) {
	t := s.Tasks["selected"]
	t.Requirement = RequirementDelivered
	e := s.Executions[t.Execution]
	e.Status = "completed"
	e.Result = &AgentResult{Task: t.ID, Execution: e.ID, Session: t.Owner, Revision: t.Revision}
}
func settlementNoop(s *State) {
	t := s.Tasks["selected"]
	t.Status, t.Requirement, t.AcceptedRevision = "awaiting_review", RequirementApplied, t.Revision
	t.StartingSnapshot, t.Snapshot = "base", "submitted"
	s.Members[t.Owner].ReadOnly = false
	s.Executions[t.Execution].Status = "completed"
	s.Snapshots["base"] = &worktree.Snapshot{ID: "base", Tree: "tree", Commit: "base-commit", Source: "/source"}
	s.Snapshots["submitted"] = &worktree.Snapshot{ID: "submitted", Tree: "tree", Commit: "submitted-commit", Source: "/source"}
}

func settlementDeferred(s *State) {
	t := s.Tasks["selected"]
	e := s.Executions[t.Execution]
	t.Status, e.Status, e.Workflow = "blocked", "paused", "w"
	s.Runs["run"].Status = "paused"
	s.Workflows["w"] = &workflow.Report{ID: "w", Run: "run", Status: "failed", Acknowledged: true}
	t.Deferral = &TaskDeferral{Workflow: "w", Execution: e.ID, Owner: t.Owner, Revision: t.Revision, Generation: e.Generation, Status: t.Status, Note: "retained"}
}

func settlementCandidate(s *State) {
	settlementNoop(s)
	t := s.Tasks["selected"]
	t.AcceptedRevision = 0
	s.Snapshots[t.Snapshot].Tree = "changed"
	s.Integrations["candidate"] = &IntegrationCandidate{ID: "candidate", Run: "run", Status: "ready", Inputs: []IntegrationInput{{TaskReference: TaskReference{Task: t.ID, Revision: t.Revision}, Base: *s.Snapshots[t.StartingSnapshot], Submitted: *s.Snapshots[t.Snapshot]}}}
}

func TestSettlementRecordsParity(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(*State)
	}{
		{"running", func(*State) {}},
		{"empty", func(s *State) { s.Tasks["selected"].Status = "done"; s.Executions["selected"].Status = "completed" }},
		{"pending", func(s *State) { s.Tasks["selected"].Status = "pending" }},
		{"blocked", func(s *State) { s.Tasks["selected"].Status = "blocked" }},
		{"delivery repair", settlementDelivery},
		{"accepted no-op", settlementNoop},
		{"valid deferral suppresses budget", settlementDeferred},
		{"stale deferral generation", func(s *State) { settlementDeferred(s); s.Executions["selected"].Generation++ }},
		{"stale deferral prior run", func(s *State) {
			settlementDeferred(s)
			s.Runs["run"].Status = "completed"
			s.Runs["new"] = &Run{ID: "new", Status: "running"}
			s.Tasks["selected"].Revision++
		}},
		{"ready candidate", settlementCandidate},
		{"conflicted candidate", func(s *State) { settlementCandidate(s); s.Integrations["candidate"].Status = "conflicted" }},
		{"candidate invalid dependency", func(s *State) {
			settlementCandidate(s)
			s.Integrations["candidate"].Repairs = []IntegrationInput{{TaskReference: TaskReference{Task: "missing", Revision: 1}}}
		}},
		{"candidate receipt", func(s *State) {
			settlementCandidate(s)
			s.Applies["candidate"] = &ApplyRecord{ID: "candidate", Status: "applied"}
		}},
		{"delivered matching notice still blocks delivery", func(s *State) {
			settlementDelivery(s)
			s.Messages["notice"] = &Mail{ID: "notice", Task: "selected", Execution: "selected", Revision: 1, Delivered: true, To: "elsewhere"}
		}},
		{"stale notice repaired", func(s *State) {
			settlementDelivery(s)
			s.Messages["notice"] = &Mail{ID: "notice", Task: "selected", Execution: "old", Revision: 1, Delivered: true}
		}},
		{"delivery repair despite unrelated uncertainty", func(s *State) {
			settlementDelivery(s)
			s.Applies["uncertain"] = &ApplyRecord{ID: "uncertain", Status: "recovery_required"}
		}},
		{"stale acceptance", func(s *State) { settlementNoop(s); s.Tasks["selected"].AcceptedRevision = 0 }},
		{"missing base", func(s *State) { settlementNoop(s); delete(s.Snapshots, "base") }},
		{"uncertain before request and repair", func(s *State) {
			settlementNoop(s)
			s.Applies["uncertain"] = &ApplyRecord{ID: "uncertain", Status: "applying"}
			s.Messages["request"] = &Mail{ID: "request", Kind: "request", To: "actor"}
		}},
		{"delivered unanswered request", func(s *State) {
			s.Messages["request"] = &Mail{ID: "request", Kind: "request", To: "actor", Delivered: true}
		}},
		{"unread reply", func(s *State) { s.Messages["reply"] = &Mail{ID: "reply", Kind: "reply", To: "actor"} }},
		{"budget before task", func(s *State) { s.Runs["run"].Status = "paused" }},
		{"iteration limit count", func(s *State) {
			s.Executions["selected"].Status = "paused"
			s.Executions["selected"].StopReason = messages.StopReasonMaxIterations
			s.Tasks["z"] = &Task{ID: "z", Run: "run", Status: "pending"}
		}},
		{"workflow output before task", func(s *State) {
			s.Workflows["w"] = &workflow.Report{ID: "w", Run: "run", Status: "completed"}
			s.Messages["notice"] = &Mail{ID: "notice", Workflow: "w", To: "actor"}
		}},
		{"failed workflow", func(s *State) {
			s.Tasks["selected"].Status = "done"
			s.Executions["selected"].Status = "completed"
			s.Workflows["w"] = &workflow.Report{ID: "w", Run: "run", Status: "failed"}
		}},
		{"host workflow control", func(s *State) {
			settlementDelivery(s)
			s.Executions["selected"].Workflow = "w"
			s.Workflows["w"] = &workflow.Report{ID: "w", Status: "running", Steps: []workflow.Step{{ID: "w/0", Kind: "agent", Status: "running"}}}
		}},
		{"display controller not authority", func(s *State) {
			settlementDelivery(s)
			s.Members["selected"].Controller = "w"
			s.Workflows["w"] = &workflow.Report{ID: "w", Status: "running", Steps: []workflow.Step{{ID: "w/0", Kind: "agent", Status: "running"}}}
		}},
		{"running host step", func(s *State) {
			s.Workflows["w"] = &workflow.Report{ID: "w", Status: "running", Steps: []workflow.Step{{ID: "w/0", Kind: "exec", Status: "running"}}}
		}},
		{"workflow startup", func(s *State) { s.Workflows["w"] = &workflow.Report{ID: "w", Status: "running"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, root := lifecycleRecordFixture(t, 2, false)
			expected := seedSettlement(t, r, func(s *State) {
				tc.mutate(s)
				for _, m := range s.Messages {
					if m.To == "actor" {
						m.To = r.ID
					}
				}
			})
			// The original rules form the oracle, independent of indexed inputs.
			r.ensureDeliveryNotices(expected)
			uncertain := false
			for _, a := range expected.Applies {
				uncertain = uncertain || a.Status == "applying" || a.Status == "recovery_required"
			}
			if !uncertain {
				for _, task := range expected.Tasks {
					if acceptedUnchangedTask(expected, task) {
						task.Status = "done"
					}
				}
			}
			want := deriveFacts(expected, r.ID)
			wantErr := blockerError(want, settlementBlockers(want))
			guard := guardSettlement(t, r, root)
			got, _, err := r.settlementFacts(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			gotErr := blockerError(got, settlementBlockers(got))
			if fmt.Sprint(gotErr) != fmt.Sprint(wantErr) || got.running != want.running {
				t.Fatalf("facts = (%v, running %v), want (%v, running %v)", gotErr, got.running, wantErr, want.running)
			}
			if err := r.Settle(t.Context()); fmt.Sprint(err) != fmt.Sprint(wantErr) {
				t.Fatalf("Settle = %v, want %v", err, wantErr)
			}
			var gotLimit, wantLimit *IterationLimitError
			if errors.As(gotErr, &gotLimit) != errors.As(wantErr, &wantLimit) || errors.Is(gotErr, ErrBudget) != errors.Is(wantErr, ErrBudget) {
				t.Fatal("lost typed blocker")
			}
			if guard.reads.Load() == 0 {
				t.Fatal("supplied native handle was bypassed")
			}
		})
	}
}

func TestSettlementRecordsBoundedCompletedHistory(t *testing.T) {
	t.Parallel()
	var baseline [4]uint64
	for _, n := range []int{16, 512} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			r, root := lifecycleRecordFixture(t, n, false)
			seedSettlement(t, r, func(s *State) {
				for i := range n {
					id := fmt.Sprintf("retained-%06d", i)
					s.Workflows[id] = &workflow.Report{ID: id, Status: "completed", Acknowledged: true, Output: strings.Repeat("x", 4096), Steps: []workflow.Step{{ID: id + "/0", Kind: "exec", Status: "completed", Value: strings.Repeat("y", 4096)}}}
					s.Applies[id] = &ApplyRecord{ID: id, Status: "applied"}
				}
			})
			guard := guardSettlement(t, r, root)
			recorder := runtimemetrics.New()
			ctx := runtimemetrics.WithRecorder(t.Context(), recorder)
			if err := r.Settle(ctx); err == nil || !strings.Contains(err.Error(), "task selected revision") {
				t.Fatalf("Settle: %v", err)
			}
			observed := recorder.Snapshot()
			work := [4]uint64{observed.Count(runtimemetrics.SQLStatements), observed.Count(runtimemetrics.RowsRead), observed.Count(runtimemetrics.PayloadBytesRead), observed.Count(runtimemetrics.RecordsDecoded)}
			if n == 16 {
				baseline = work
			} else if work != baseline {
				t.Fatalf("completed history scaled settlement work: %v versus %v", work, baseline)
			}
			if guard.writes.Load() != 0 {
				t.Fatal("read-only settlement wrote coordination")
			}
			t.Logf("completed history groups=%d: SQL=%d rows=%d bytes=%d decoded=%d", n, work[0], work[1], work[2], work[3])
		})
	}
}

func TestSettlementRecordsRepairBoundedHistory(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"notice", "no-op"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			var baseline [3]uint64
			for _, count := range []int{16, 512} {
				t.Run(fmt.Sprint(count), func(t *testing.T) {
					r, root := lifecycleRecordFixture(t, count, false)
					seedSettlement(t, r, func(s *State) {
						if mode == "notice" {
							settlementDelivery(s)
						} else {
							settlementNoop(s)
							s.Tasks["selected"].Result = json.Number("9007199254740993")
						}
					})
					guard := guardSettlement(t, r, root)
					recorder := runtimemetrics.New()
					ctx := runtimemetrics.WithRecorder(t.Context(), recorder)
					err := r.Settle(ctx)
					if mode == "notice" {
						if err == nil || !strings.Contains(err.Error(), "awaits delivery") {
							t.Fatalf("repair: %v", err)
						}
					} else if err != nil {
						t.Fatal(err)
					}
					if guard.writes.Load() != 1 {
						t.Fatalf("repair writes = %d", guard.writes.Load())
					}
					observed := recorder.Snapshot()
					work := [3]uint64{observed.Count(runtimemetrics.SQLStatements), observed.Count(runtimemetrics.RowsRead), observed.Count(runtimemetrics.RecordsDecoded)}
					if count == 16 {
						baseline = work
					} else if work != baseline {
						t.Fatalf("repair scaled with completed history: %v vs %v", work, baseline)
					}
					if mode == "no-op" {
						if err := root.(sessions.CoordinationRecords).ReadRecords(t.Context(), func(rd sessions.CoordinationReader) error {
							record, _, err := rd.Get(sessions.RecordKey{Kind: "task", ID: "selected"})
							if err == nil && !strings.Contains(string(record.Payload), `"result":9007199254740993`) {
								t.Error("repair rounded opaque task result")
							}
							return err
						}); err != nil {
							t.Fatal(err)
						}
					}
					t.Logf("completed history groups=%d: %s repair SQL=%d rows=%d decoded=%d", count, mode, work[0], work[1], work[2])
				})
			}
		})
	}
}

func TestSettlementRecordsRepairRechecksAndRollsBack(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"deliver-concurrently", "uncertain-apply", "rollback-notice", "rollback-noop"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			r, root := lifecycleRecordFixture(t, 2, false)
			seedSettlement(t, r, func(s *State) {
				if mode == "uncertain-apply" || mode == "rollback-noop" {
					settlementNoop(s)
				} else {
					settlementDelivery(s)
				}
			})
			guard := guardSettlement(t, r, root)
			guard.failAfterPut = strings.HasPrefix(mode, "rollback")
			guard.beforeUpdate = func() {
				if mode == "deliver-concurrently" {
					commitWaitRecord(t, r, root, "task", "selected", &Task{ID: "selected", Run: "run", Owner: "selected", Execution: "selected", Requirement: RequirementDelivered, Status: "done", Revision: 1, Delivery: &TaskDelivery{Execution: "selected", Revision: 1}})
				} else if mode == "uncertain-apply" {
					commitWaitRecord(t, r, root, "apply", "uncertain", &ApplyRecord{ID: "uncertain", Status: "applying"})
				}
			}
			err := r.Settle(t.Context())
			if strings.HasPrefix(mode, "rollback") && !errors.Is(err, errHotWrite) {
				t.Fatalf("repair did not roll back: %v", err)
			}
			if mode == "deliver-concurrently" && err != nil {
				t.Fatalf("stale delivery repair: %v", err)
			}
			if mode == "uncertain-apply" && (err == nil || !strings.Contains(err.Error(), "unconfirmed outcome")) {
				t.Fatalf("apply race lost priority: %v", err)
			}
			raw, err := root.ReadCoordination(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			s, err := decodeState(raw)
			if err != nil {
				t.Fatal(err)
			}
			if len(s.Messages) != 3 {
				t.Fatalf("stale/rolled-back repair added mail: %d", len(s.Messages))
			}
			if (mode == "uncertain-apply" || mode == "rollback-noop") && s.Tasks["selected"].Status != "awaiting_review" {
				t.Fatal("unsafe no-op completion committed")
			}
		})
	}
}
