package swarm

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/alexschlessinger/pollytool/sessions"
)

var errSettlementBootstrapWrite = errors.New("read-only settlement attempted a write")
var errSettlementBootstrapQuery = errors.New("prebootstrap settlement queried swarm candidates")

// Only checkFormatFor's bounded prebootstrap probe is allowed. Candidate reads
// need a format and must not run once that probe identifies an empty root or its
// own journal. Both write interfaces reject calls, including attempted no-ops.
type settlementBootstrapSession struct {
	sessions.CoordinationSession
	records            sessions.CoordinationRecords
	writes, candidates atomic.Int64
	format             json.RawMessage
	probe              []sessions.CoordinationRecord
}

func (*settlementBootstrapSession) CoordinationCapabilities() sessions.CoordinationCapabilities {
	return sessions.CoordinationCapabilities{NativeIndexed: true, DurableRevisions: true}
}
func (*settlementBootstrapSession) ReadCoordination(context.Context) (*sessions.CoordinationState, error) {
	return nil, errHotLegacy
}
func (s *settlementBootstrapSession) UpdateCoordination(context.Context, func(*sessions.CoordinationState) error) error {
	s.writes.Add(1)
	return errSettlementBootstrapWrite
}
func (s *settlementBootstrapSession) UpdateRecords(context.Context, func(sessions.CoordinationTx) error) (sessions.CoordinationCommit, error) {
	s.writes.Add(1)
	return sessions.CoordinationCommit{}, errSettlementBootstrapWrite
}
func (s *settlementBootstrapSession) ReadRecords(ctx context.Context, fn func(sessions.CoordinationReader) error) error {
	return s.records.ReadRecords(ctx, func(rd sessions.CoordinationReader) error {
		return fn(settlementBootstrapReader{rd, s})
	})
}

type settlementBootstrapReader struct {
	sessions.CoordinationReader
	owner *settlementBootstrapSession
}

func (rd settlementBootstrapReader) Get(key sessions.RecordKey) (sessions.CoordinationRecord, bool, error) {
	if key == (sessions.RecordKey{Kind: formatKind, ID: formatID}) && rd.owner.format != nil {
		return sessions.CoordinationRecord{Key: key, Payload: rd.owner.format}, true, nil
	}
	return rd.CoordinationReader.Get(key)
}
func (rd settlementBootstrapReader) Query(q sessions.RecordQuery) (sessions.RecordPage, error) {
	if q.Index != "" || q.Limit != 2 {
		rd.owner.candidates.Add(1)
		return sessions.RecordPage{}, errSettlementBootstrapQuery
	}
	if rd.owner.probe != nil {
		return sessions.RecordPage{Records: rd.owner.probe}, nil
	}
	return rd.CoordinationReader.Query(q)
}
func (rd settlementBootstrapReader) QueryCoordinationCandidates(sessions.CoordinationCandidateQuery) (sessions.RecordPage, error) {
	rd.owner.candidates.Add(1)
	return sessions.RecordPage{}, errSettlementBootstrapQuery
}

func settlementBootstrapFixture(t *testing.T, journal bool) (*Runtime, sessions.CoordinationSession, *settlementBootstrapSession) {
	t.Helper()
	r := runtimeTest(t, idleModel(), 1, 1)
	root := r.parent
	if journal {
		if err := root.UpdateCoordination(t.Context(), func(raw *sessions.CoordinationState) error {
			raw.Records["parent_turn"] = map[string]json.RawMessage{r.ID: json.RawMessage(`{"intent":[]}`)}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	handle := &settlementBootstrapSession{CoordinationSession: root, records: root.(sessions.CoordinationRecords)}
	r.parent = handle
	localWaitNotifications(t, r)
	// The local-only provider has no poller for Runtime.Close to join.
	t.Cleanup(func() { r.mu.Lock(); r.notifications = nil; r.mu.Unlock() })
	return r, root, handle
}

func TestSettlementPrebootstrapReadsDoNotInitialize(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"settle", "delivery-return"} {
		for _, journal := range []bool{false, true} {
			name := operation + "/empty"
			if journal {
				name = operation + "/parent-journal"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				r, root, handle := settlementBootstrapFixture(t, journal)
				before, err := root.ReadCoordination(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				var promotes atomic.Int64
				r.config.Promote = func(context.Context) error {
					promotes.Add(1)
					return errors.New("unexpected promotion")
				}
				sub, err := r.Subscribe(t.Context(), []sessions.RevisionScope{{Index: sessions.CoordinationIndexRecords}, ActivityScope()})
				if err != nil {
					t.Fatal(err)
				}
				defer sub.Close()
				r.mu.Lock()
				compatibility := r.notify
				r.mu.Unlock()
				if operation == "settle" {
					err = r.Settle(t.Context())
				} else {
					err = r.repairDeliveryNoticesOnReturn(t.Context(), nil)
				}
				if err != nil {
					t.Fatal(err)
				}
				if promotes.Load() != 0 || handle.writes.Load() != 0 || handle.candidates.Load() != 0 || r.prepared {
					t.Fatalf("prebootstrap read initialized swarm: promotes=%d writes=%d candidate queries=%d prepared=%v", promotes.Load(), handle.writes.Load(), handle.candidates.Load(), r.prepared)
				}
				after, err := root.ReadCoordination(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(before, after) {
					t.Fatalf("read changed durable state: before=%+v after=%+v", before, after)
				}
				select {
				case <-sub.Changes():
					t.Fatal("read published a notification")
				default:
				}
				select {
				case <-compatibility:
					t.Fatal("read published a compatibility wake")
				default:
				}
			})
		}
	}
}

func TestSettlementPrebootstrapRejectsInvalidRoots(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"settle", "delivery-return"} {
		for _, invalid := range []string{"older-format", "newer-format", "malformed-format", "partial-swarm", "foreign-journal"} {
			t.Run(operation+"/"+invalid, func(t *testing.T) {
				t.Parallel()
				r, _, handle := settlementBootstrapFixture(t, false)
				switch invalid {
				case "older-format":
					handle.format = json.RawMessage(`{"version":1}`)
				case "newer-format":
					handle.format = json.RawMessage(`{"version":3}`)
				case "malformed-format":
					handle.format = json.RawMessage(`{"version":"invalid"}`)
				case "partial-swarm":
					handle.probe = []sessions.CoordinationRecord{{Key: sessions.RecordKey{Kind: "member", ID: "member"}, Payload: json.RawMessage(`{}`)}}
				case "foreign-journal":
					handle.probe = []sessions.CoordinationRecord{{Key: sessions.RecordKey{Kind: "parent_turn", ID: "other"}, Payload: json.RawMessage(`{}`)}}
				}
				var promotes atomic.Int64
				r.config.Promote = func(context.Context) error { promotes.Add(1); return errors.New("unexpected promotion") }
				var err error
				if operation == "settle" {
					err = r.Settle(t.Context())
				} else {
					err = r.repairDeliveryNoticesOnReturn(t.Context(), nil)
				}
				if err == nil || invalid != "malformed-format" && !errors.Is(err, ErrUnsupportedFormat) {
					t.Fatalf("invalid root accepted or misclassified: %v", err)
				}
				if promotes.Load() != 0 || handle.writes.Load() != 0 || handle.candidates.Load() != 0 {
					t.Fatal("invalid root was prepared or queried")
				}
			})
		}
	}
}

func TestSettlementPrebootstrapRejectsMalformedJournal(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"settle", "delivery-return"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			r, root, handle := settlementBootstrapFixture(t, false)
			if err := root.UpdateCoordination(t.Context(), func(raw *sessions.CoordinationState) error {
				raw.Records["parent_turn"] = map[string]json.RawMessage{r.ID: json.RawMessage(`{"intent":42}`)}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			raw, err := root.ReadCoordination(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := decodeState(raw); err == nil {
				t.Fatal("fixture was not rejected by the original typed reader")
			}
			var promotes atomic.Int64
			r.config.Promote = func(context.Context) error { promotes.Add(1); return errors.New("unexpected promotion") }
			if operation == "settle" {
				err = r.Settle(t.Context())
			} else {
				err = r.repairDeliveryNoticesOnReturn(t.Context(), nil)
			}
			if err == nil {
				t.Fatal("malformed own journal was accepted")
			}
			if promotes.Load() != 0 || handle.writes.Load() != 0 || handle.candidates.Load() != 0 {
				t.Fatal("invalid journal was prepared or queried")
			}
		})
	}
}

func TestSettlementActivityCompletionDuringRead(t *testing.T) {
	t.Parallel()
	r, root := lifecycleRecordFixture(t, 0, false)
	seedSettlement(t, r, func(s *State) { s.Tasks["selected"].Status = "done"; s.Executions["selected"].Status = "completed" })
	localWaitNotifications(t, r)
	r.active["selected"] = &invocation{}
	b := startWaitReadBarrier(t, r, root, r.Settle)
	b.read(t, 1)
	// Durable completion precedes removal from the live set. Removal while the
	// read returns must not turn an already-idle swarm into a waiting blocker.
	r.mu.Lock()
	delete(r.active, "selected")
	r.mu.Unlock()
	r.publishActivity()
	b.advance(t)
	b.read(t, 2) // reconcile the changed live set with a fresh durable snapshot
	b.advance(t)
	b.result(t, nil)
}

func TestSettlementActivityReservationDuringRead(t *testing.T) {
	t.Parallel()
	r, root := lifecycleRecordFixture(t, 0, false)
	seedSettlement(t, r, func(s *State) { s.Tasks["selected"].Status = "done"; s.Executions["selected"].Status = "completed" })
	localWaitNotifications(t, r)
	b := startWaitReadBarrier(t, r, root, r.Settle)
	b.read(t, 1)
	commitWaitRecord(t, r, root, "task", "selected", &Task{ID: "selected", Run: "run", Owner: "selected", Execution: "selected", Status: "running", Revision: 1})
	commitWaitRecord(t, r, root, "execution", "selected", &Execution{ID: "selected", Run: "run", Member: "selected", Status: "running"})
	r.mu.Lock()
	r.active["selected"] = &invocation{}
	r.mu.Unlock()
	r.publishActivity()
	b.advance(t)
	b.read(t, 2) // do not return idle using the snapshot before reservation
	b.advance(t)
	b.read(t, 3) // replace the newly discovered task dependency subscriptions
	b.cancel()
	b.result(t, context.Canceled)
}

func TestSettlementActivityCompletionAfterSnapshot(t *testing.T) {
	t.Parallel()
	r, root := lifecycleRecordFixture(t, 0, false)
	localWaitNotifications(t, r)
	r.active["selected"] = &invocation{}
	b := startWaitReadBarrier(t, r, root, r.Settle)
	b.read(t, 1)
	b.advance(t)
	b.read(t, 2) // the selected task/execution scopes are now installed
	commitWaitRecord(t, r, root, "task", "selected", &Task{ID: "selected", Run: "run", Owner: "selected", Execution: "selected", Status: "done", Revision: 1})
	commitWaitRecord(t, r, root, "execution", "selected", &Execution{ID: "selected", Run: "run", Member: "selected", Status: "completed"})
	r.mu.Lock()
	delete(r.active, "selected")
	r.mu.Unlock()
	r.publishActivity()
	b.advance(t)
	// A fresh idle sample must not be combined with the old running task facts.
	b.read(t, 3)
	b.advance(t)
	b.result(t, nil)
}
