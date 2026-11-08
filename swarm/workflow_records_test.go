package swarm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/workflow"
)

type workflowRecordCounts struct {
	callbacks, gets, queries, rows, bytes, puts, deletes, broad int
	written                                                     []sessions.RecordKey
}

type workflowRecordSession struct {
	sessions.CoordinationSession
	access          sessions.CoordinationAccess
	mu              sync.Mutex
	counts          workflowRecordCounts
	veto, uncertain error
}

func (s *workflowRecordSession) CoordinationCapabilities() sessions.CoordinationCapabilities {
	return s.access.Capabilities()
}
func (s *workflowRecordSession) ReadRecords(ctx context.Context, fn func(sessions.CoordinationReader) error) error {
	return s.access.ReadRecords(ctx, fn)
}
func (s *workflowRecordSession) UpdateRecords(ctx context.Context, fn func(sessions.CoordinationTx) error) (sessions.CoordinationCommit, error) {
	commit, err := s.access.UpdateRecords(ctx, func(tx sessions.CoordinationTx) error {
		s.mu.Lock()
		s.counts.callbacks++
		s.mu.Unlock()
		if err := fn(&workflowCountingTx{CoordinationTx: tx, session: s}); err != nil {
			return err
		}
		return s.veto
	})
	if err == nil && s.uncertain != nil {
		return sessions.CoordinationCommit{}, s.uncertain
	}
	return commit, err
}
func (s *workflowRecordSession) UpdateCoordination(ctx context.Context, fn func(*sessions.CoordinationState) error) error {
	s.mu.Lock()
	s.counts.broad++
	s.mu.Unlock()
	return s.CoordinationSession.UpdateCoordination(ctx, fn)
}
func (s *workflowRecordSession) ReadCoordination(ctx context.Context) (*sessions.CoordinationState, error) {
	s.mu.Lock()
	s.counts.broad++
	s.mu.Unlock()
	return s.CoordinationSession.ReadCoordination(ctx)
}
func (s *workflowRecordSession) resetCounts() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counts = workflowRecordCounts{}
}
func (s *workflowRecordSession) snapshot() workflowRecordCounts {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.counts
	out.written = append([]sessions.RecordKey(nil), out.written...)
	return out
}

type workflowCountingTx struct {
	sessions.CoordinationTx
	session *workflowRecordSession
}

func (tx *workflowCountingTx) Get(key sessions.RecordKey) (sessions.CoordinationRecord, bool, error) {
	record, found, err := tx.CoordinationTx.Get(key)
	tx.session.mu.Lock()
	defer tx.session.mu.Unlock()
	tx.session.counts.gets++
	if found {
		tx.session.counts.rows++
		tx.session.counts.bytes += len(record.Payload)
	}
	return record, found, err
}
func (tx *workflowCountingTx) Query(query sessions.RecordQuery) (sessions.RecordPage, error) {
	page, err := tx.CoordinationTx.Query(query)
	tx.session.mu.Lock()
	defer tx.session.mu.Unlock()
	tx.session.counts.queries++
	tx.session.counts.rows += len(page.Records)
	for _, record := range page.Records {
		tx.session.counts.bytes += len(record.Payload)
	}
	return page, err
}
func (tx *workflowCountingTx) Put(key sessions.RecordKey, data json.RawMessage) error {
	tx.session.mu.Lock()
	tx.session.counts.puts++
	tx.session.counts.written = append(tx.session.counts.written, key)
	tx.session.mu.Unlock()
	return tx.CoordinationTx.Put(key, data)
}
func (tx *workflowCountingTx) Delete(key sessions.RecordKey) error {
	tx.session.mu.Lock()
	tx.session.counts.deletes++
	tx.session.mu.Unlock()
	return tx.CoordinationTx.Delete(key)
}

func workflowRecordsRuntime(t *testing.T, legacy bool) (*Runtime, *workflowRecordSession) {
	t.Helper()
	store, err := sessions.OpenStore(sessions.StoreConfig{Mode: sessions.ModeMemory})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	session, err := store.Acquire(t.Context(), "workflow-records", sessions.AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	parent := session.(sessions.CoordinationSession)
	var id string
	if err := parent.UpdateCoordination(t.Context(), func(raw *sessions.CoordinationState) error {
		id = raw.ParentID
		raw.Records = map[string]map[string]json.RawMessage{
			"format": {"swarm": json.RawMessage(`{"version":2}`)},
			"run":    {"run": json.RawMessage(`{"id":"run","status":"running","starts":0,"limit":20}`)},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var handle any = parent
	if legacy {
		handle = repositoryLegacyOnly{parent}
	}
	access, err := sessions.OpenCoordinationAccess(handle)
	if err != nil {
		t.Fatal(err)
	}
	if access.Capabilities().NativeIndexed == legacy {
		t.Fatalf("wrong native capability: %+v", access.Capabilities())
	}
	counted := &workflowRecordSession{CoordinationSession: parent, access: access}
	return &Runtime{ID: id, parent: counted, prepared: true, releaseStopped: true, notify: make(chan struct{})}, counted
}

func workflowRecordsState(t *testing.T, session *workflowRecordSession) *State {
	t.Helper()
	raw, err := session.CoordinationSession.ReadCoordination(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	state, err := decodeState(raw)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func workflowRecordsStep(index int) workflow.Step {
	return workflow.Step{Operation: workflow.Operation{ID: fmt.Sprintf("w/%d", index+1), Kind: "log"}, Status: "running"}
}
func workflowRecordsHeader() workflow.ReportHeader {
	return workflow.ReportHeader{ID: "w", Run: "run", CallID: "launch", Name: "records", Status: "running", Input: map[string]any{"id": "not-a-member", "nested": []any{"execution", "task"}}}
}

func TestWorkflowRecordsMixedFullDelta(t *testing.T) {
	t.Parallel()
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			r, session := workflowRecordsRuntime(t, legacy)
			h := workflowRecordsHeader()
			if err := r.SaveWorkflowDelta(t.Context(), workflow.ReportDelta{Header: h}); err != nil {
				t.Fatal(err)
			}
			steps := map[int]workflow.Step{0: workflowRecordsStep(0), 1: workflowRecordsStep(1), 2: workflowRecordsStep(2)}
			if err := r.SaveWorkflowDelta(t.Context(), workflow.ReportDelta{Header: h, TotalSteps: 3, Steps: steps}); err != nil {
				t.Fatal(err)
			}
			state := workflowRecordsState(t, session)
			full := *state.Workflows["w"]
			session.resetCounts()
			if err := r.SaveWorkflow(t.Context(), full); err != nil {
				t.Fatal(err)
			}
			if counts := session.snapshot(); counts.puts != 0 || counts.deletes != 0 || counts.broad != 0 {
				t.Fatalf("identical full save wrote records: %+v", counts)
			}
			full.Steps[1].Status, full.Steps[1].Value = "completed", map[string]any{"id": "not-a-member", "task": "verbatim"}
			session.resetCounts()
			if err := r.SaveWorkflow(t.Context(), full); err != nil {
				t.Fatal(err)
			}
			if counts := session.snapshot(); !reflect.DeepEqual(counts.written, []sessions.RecordKey{{Kind: "workflow_step", ID: "w/1"}}) {
				t.Fatalf("changed-step writes: %+v", counts)
			}
			full.Steps = full.Steps[:1]
			session.resetCounts()
			if err := r.SaveWorkflow(t.Context(), full); err != nil {
				t.Fatal(err)
			}
			if counts := session.snapshot(); counts.deletes != 2 || counts.puts != 0 {
				t.Fatalf("truncation: %+v", counts)
			}
			if err := r.SaveWorkflowDelta(t.Context(), workflow.ReportDelta{Header: h, TotalSteps: 2, Steps: map[int]workflow.Step{1: steps[1]}}); err != nil {
				t.Fatal(err)
			}
			if err := session.CoordinationSession.UpdateCoordination(t.Context(), func(raw *sessions.CoordinationState) error {
				state, err := decodeState(raw)
				if err != nil {
					return err
				}
				state.Workflows["w"].Acknowledged = true
				return encodeState(raw, state)
			}); err != nil {
				t.Fatal(err)
			}
			h.CallID, h.Run, h.Acknowledged = "forged", "forged", false
			h.Status, h.Output = "completed", map[string]any{"session": "unchanged", "execution": "unchanged"}
			if err := r.SaveWorkflowDelta(t.Context(), workflow.ReportDelta{Header: h, TotalSteps: 2}); err != nil {
				t.Fatal(err)
			}
			if err := r.SaveWorkflowDelta(t.Context(), workflow.ReportDelta{Header: h, TotalSteps: 2}); err != nil {
				t.Fatal(err)
			}
			state = workflowRecordsState(t, session)
			w := state.Workflows["w"]
			if len(w.Steps) != 2 || w.CallID != "launch" || w.Run != "run" || !w.Acknowledged || !reflect.DeepEqual(w.Output, h.Output) || !reflect.DeepEqual(w.Input, h.Input) {
				t.Fatalf("mixed save lost values/bookkeeping: %+v", w)
			}
			if len(state.Messages) != 1 {
				t.Fatalf("terminal notices=%d", len(state.Messages))
			}
			full = *w
			full.Steps = nil
			if err := r.SaveWorkflow(t.Context(), full); err != nil {
				t.Fatal(err)
			}
			if got := workflowRecordsState(t, session).Workflows["w"]; len(got.Steps) != 0 {
				t.Fatal("full save did not delete final steps")
			}
		})
	}
}

func TestWorkflowRecordsDeltaValidation(t *testing.T) {
	t.Parallel()
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			r, session := workflowRecordsRuntime(t, legacy)
			h := workflowRecordsHeader()
			if err := r.SaveWorkflowDelta(t.Context(), workflow.ReportDelta{Header: h, TotalSteps: 1, Steps: map[int]workflow.Step{0: workflowRecordsStep(0)}}); err != nil {
				t.Fatal(err)
			}
			for _, delta := range []workflow.ReportDelta{
				{Header: h, TotalSteps: -1},
				{Header: h, TotalSteps: 0},
				{Header: h, TotalSteps: 1, Steps: map[int]workflow.Step{-1: {}}},
				{Header: h, TotalSteps: 1, Steps: map[int]workflow.Step{1: {}}},
				{Header: h, TotalSteps: 3, Steps: map[int]workflow.Step{2: {}}},
				{Header: h, TotalSteps: 4, Steps: map[int]workflow.Step{0: {}, 2: {}, 3: {}}},
				{Header: h, TotalSteps: 1 << 30},
			} {
				if err := r.SaveWorkflowDelta(t.Context(), delta); !errors.Is(err, sessions.ErrCoordinationRequest) {
					t.Fatalf("delta %+v: %v", delta, err)
				}
			}
			h.Status = "completed"
			if err := r.SaveWorkflowDelta(t.Context(), workflow.ReportDelta{Header: h, TotalSteps: 1, Steps: map[int]workflow.Step{0: {Value: make(chan int)}}}); err == nil {
				t.Fatal("invalid JSON accepted")
			}
			state := workflowRecordsState(t, session)
			if state.Workflows["w"].Status != "running" || len(state.Workflows["w"].Steps) != 1 || len(state.Messages) != 0 {
				t.Fatal("invalid delta partially persisted")
			}
		})
	}
}

func TestRuntimeStorageScaleWorkflowRecords(t *testing.T) {
	t.Parallel()
	var baseline workflowRecordCounts
	var appendBytes int
	for _, history := range []int{16, 1024} {
		t.Run(fmt.Sprint(history), func(t *testing.T) {
			r, session := workflowRecordsRuntime(t, false)
			h := workflowRecordsHeader()
			steps := make(map[int]workflow.Step, history)
			for i := range history {
				steps[i] = workflowRecordsStep(i)
			}
			// Even the first indexed historical payload may be arbitrarily large.
			// Counting steps must not fetch it just to discover the partition size.
			first := steps[0]
			first.Value = strings.Repeat("old", history*128)
			steps[0] = first
			if err := r.SaveWorkflowDelta(t.Context(), workflow.ReportDelta{Header: h, TotalSteps: history, Steps: steps}); err != nil {
				t.Fatal(err)
			}
			if _, err := session.access.UpdateRecords(t.Context(), func(tx sessions.CoordinationTx) error {
				for i := range history {
					id := fmt.Sprintf("unrelated-%04d", i)
					for _, kind := range []string{"member", "task", "execution", "workflow", "mail"} {
						data, _ := json.Marshal(map[string]any{"id": id, "status": "completed", "text": strings.Repeat("x", 128)})
						if err := tx.Put(sessions.RecordKey{Kind: kind, ID: id}, data); err != nil {
							return err
						}
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			session.resetCounts()
			step := steps[1]
			step.Status, step.Value = "completed", "kept verbatim"
			if err := r.SaveWorkflowDelta(t.Context(), workflow.ReportDelta{Header: h, TotalSteps: history, Steps: map[int]workflow.Step{1: step}}); err != nil {
				t.Fatal(err)
			}
			counts := session.snapshot()
			if counts.callbacks != 1 || counts.broad != 0 || counts.puts != 1 || counts.gets != 3 || counts.queries != 1 || counts.rows != 3 {
				t.Fatalf("unbounded targeted work: %+v", counts)
			}
			if history == 16 {
				baseline = counts
			} else if !reflect.DeepEqual(counts, baseline) {
				t.Fatalf("history changed touched records: small=%+v large=%+v", baseline, counts)
			}
			t.Logf("history=%d unrelated_records=%d facade returned rows=%d bytes=%d gets=%d queries=%d puts=%d (native; excludes SQL index work and cold preparation)", history, history*5, counts.rows, counts.bytes, counts.gets, counts.queries, counts.puts)
			session.resetCounts()
			if err := r.SaveWorkflowDelta(t.Context(), workflow.ReportDelta{Header: h, TotalSteps: history + 1, Steps: map[int]workflow.Step{history: workflowRecordsStep(history)}}); err != nil {
				t.Fatal(err)
			}
			appended := session.snapshot()
			if appended.callbacks != 1 || appended.broad != 0 || appended.gets != 3 || appended.queries != 1 || appended.rows != 2 || appended.puts != 1 {
				t.Fatalf("append read historical payloads: %+v", appended)
			}
			if history == 16 {
				appendBytes = appended.bytes
			} else if appended.bytes != appendBytes {
				t.Fatalf("append read bytes grew with history: small=%d large=%d", appendBytes, appended.bytes)
			}
			t.Logf("append history=%d facade returned rows=%d bytes=%d gets=%d queries=%d puts=%d", history, appended.rows, appended.bytes, appended.gets, appended.queries, appended.puts)
			session.resetCounts()
			h.Status = "completed"
			if err := r.SaveWorkflowDelta(t.Context(), workflow.ReportDelta{Header: h, TotalSteps: history + 1}); err != nil {
				t.Fatal(err)
			}
			if counts := session.snapshot(); counts.gets > 3 || counts.rows > 3 || counts.puts != 2 || counts.queries != 2 {
				t.Fatalf("header-only terminal save read steps/history: %+v", counts)
			}
		})
	}
}

func TestWorkflowRecordsVetoAndUncertainCommit(t *testing.T) {
	t.Parallel()
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			r, session := workflowRecordsRuntime(t, legacy)
			h := workflowRecordsHeader()
			if err := r.SaveWorkflowDelta(t.Context(), workflow.ReportDelta{Header: h}); err != nil {
				t.Fatal(err)
			}
			h.Status = "completed"
			sentinel := errors.New("injected persistence error")
			session.veto = sentinel
			session.resetCounts()
			notify := r.notify
			if err := r.SaveWorkflowDelta(t.Context(), workflow.ReportDelta{Header: h}); !errors.Is(err, sentinel) {
				t.Fatal(err)
			}
			if session.snapshot().callbacks != 1 || r.notify != notify {
				t.Fatal("veto replayed or notified")
			}
			if state := workflowRecordsState(t, session); state.Workflows["w"].Status != "running" || len(state.Messages) != 0 {
				t.Fatal("veto persisted")
			}
			session.veto, session.uncertain = nil, sentinel
			session.resetCounts()
			if err := r.SaveWorkflowDelta(t.Context(), workflow.ReportDelta{Header: h}); !errors.Is(err, sentinel) {
				t.Fatal(err)
			}
			if session.snapshot().callbacks != 1 || r.notify != notify {
				t.Fatal("uncertain save replayed or notified")
			}
			if state := workflowRecordsState(t, session); state.Workflows["w"].Status != "completed" || len(state.Messages) != 1 {
				t.Fatal("uncertain commit missing atomic terminal state/mail")
			}
			session.uncertain = nil
			if err := r.SaveWorkflowDelta(t.Context(), workflow.ReportDelta{Header: h}); err != nil {
				t.Fatal(err)
			}
			if len(workflowRecordsState(t, session).Messages) != 1 {
				t.Fatal("explicit recovery duplicated terminal mail")
			}
		})
	}
}
