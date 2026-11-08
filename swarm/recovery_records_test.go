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
	"time"

	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/workflow"
)

var errRecoveryCommit = errors.New("injected recovery commit uncertainty")

type recoveryProbe struct {
	sessions.Session
	sessions.CoordinationSession
	records                                                         sessions.CoordinationRecords
	mu                                                              sync.Mutex
	reads, writes, passes, validated, selected, gets, puts, maxPage int
	cold, work                                                      *runtimemetrics.Recorder
	failAt                                                          int
	committed                                                       bool
	override                                                        map[sessions.RecordKey]json.RawMessage
}

func probeRecovery(parent sessions.CoordinationSession) *recoveryProbe {
	return &recoveryProbe{Session: parent.(sessions.Session), CoordinationSession: parent, records: parent.(sessions.CoordinationRecords), cold: runtimemetrics.New(), work: runtimemetrics.New()}
}
func (p *recoveryProbe) ReadCoordination(context.Context) (*sessions.CoordinationState, error) {
	return nil, errHotLegacy
}
func (p *recoveryProbe) UpdateCoordination(context.Context, func(*sessions.CoordinationState) error) error {
	return errHotLegacy
}
func (p *recoveryProbe) UpdateCoordinationCommit(context.Context, func(*sessions.CoordinationState) error) (sessions.CoordinationCommit, error) {
	return sessions.CoordinationCommit{}, errHotLegacy
}
func (p *recoveryProbe) CoordinationCapabilities() sessions.CoordinationCapabilities {
	return sessions.CoordinationCapabilities{NativeIndexed: true, DurableRevisions: true}
}

func (p *recoveryProbe) ReadRecords(ctx context.Context, fn func(sessions.CoordinationReader) error) error {
	p.mu.Lock()
	first := p.reads == 0
	p.reads++
	p.mu.Unlock()
	metric := p.work
	if first {
		metric = p.cold
	}
	return p.records.ReadRecords(runtimemetrics.WithRecorder(ctx, metric), func(rd sessions.CoordinationReader) error { return fn(recoveryProbeReader{rd, p}) })
}
func (p *recoveryProbe) UpdateRecords(ctx context.Context, fn func(sessions.CoordinationTx) error) (sessions.CoordinationCommit, error) {
	p.mu.Lock()
	p.writes++
	fail := p.writes == p.failAt
	p.mu.Unlock()
	commit, err := p.records.UpdateRecords(runtimemetrics.WithRecorder(ctx, p.work), func(tx sessions.CoordinationTx) error {
		if err := fn(recoveryProbeTx{tx, p}); err != nil {
			return err
		}
		if fail && !p.committed {
			return errRecoveryCommit
		}
		return nil
	})
	if err == nil && fail {
		return sessions.CoordinationCommit{}, errRecoveryCommit
	}
	return commit, err
}

type recoveryProbeReader struct {
	sessions.CoordinationReader
	p *recoveryProbe
}

func (rd recoveryProbeReader) Get(key sessions.RecordKey) (sessions.CoordinationRecord, bool, error) {
	rd.p.mu.Lock()
	if rd.p.reads > 1 { // the first callback is the separately measured cold validation
		rd.p.gets++
	}
	override := rd.p.override[key]
	rd.p.mu.Unlock()
	record, found, err := rd.CoordinationReader.Get(key)
	if err == nil && found && override != nil {
		record.Payload = override
	}
	return record, found, err
}
func (rd recoveryProbeReader) Query(q sessions.RecordQuery) (sessions.RecordPage, error) {
	if q.CountOnly {
		return rd.CoordinationReader.Query(q) // metadata preflight is not a payload scan
	}
	all := (q.Index == "" || q.Index == sessions.CoordinationIndexRecords) && q.Kind == ""
	if all && (q.Limit != recoveryPageLimit || q.MaxBytes != recoveryPageBytes) {
		return sessions.RecordPage{}, fmt.Errorf("unbounded startup query: %+v", q)
	}
	page, err := rd.CoordinationReader.Query(q)
	if err != nil {
		return page, err
	}
	rd.p.mu.Lock()
	defer rd.p.mu.Unlock()
	if all {
		if q.After == "" {
			rd.p.passes++
		}
		if rd.p.passes != 1 {
			return sessions.RecordPage{}, fmt.Errorf("constructor repeated validation scan: %d", rd.p.passes)
		}
		bytes := 0
		for i := range page.Records {
			bytes += len(page.Records[i].Payload)
			if data, ok := rd.p.override[page.Records[i].Key]; ok {
				page.Records[i].Payload = data
			}
		}
		if bytes > recoveryPageBytes && len(page.Records) != 1 {
			return sessions.RecordPage{}, errors.New("unbounded page payload")
		}
		rd.p.maxPage = max(rd.p.maxPage, bytes)
		rd.p.validated += len(page.Records)
	} else {
		rd.p.selected += len(page.Records)
	}
	return page, nil
}
func (rd recoveryProbeReader) QueryCoordinationCandidates(q sessions.CoordinationCandidateQuery) (sessions.RecordPage, error) {
	page, err := sessions.QueryCoordinationCandidates(rd.CoordinationReader, q)
	rd.p.mu.Lock()
	rd.p.selected += len(page.Records)
	rd.p.mu.Unlock()
	return page, err
}

type recoveryProbeTx struct {
	sessions.CoordinationTx
	p *recoveryProbe
}

func (tx recoveryProbeTx) Get(key sessions.RecordKey) (sessions.CoordinationRecord, bool, error) {
	return (recoveryProbeReader{tx.CoordinationTx, tx.p}).Get(key)
}
func (tx recoveryProbeTx) Query(q sessions.RecordQuery) (sessions.RecordPage, error) {
	return (recoveryProbeReader{tx.CoordinationTx, tx.p}).Query(q)
}
func (tx recoveryProbeTx) QueryCoordinationCandidates(q sessions.CoordinationCandidateQuery) (sessions.RecordPage, error) {
	return (recoveryProbeReader{tx.CoordinationTx, tx.p}).QueryCoordinationCandidates(q)
}
func (tx recoveryProbeTx) Put(key sessions.RecordKey, value json.RawMessage) error {
	tx.p.mu.Lock()
	tx.p.puts++
	tx.p.mu.Unlock()
	return tx.CoordinationTx.Put(key, value)
}

func recoveryFixture(t *testing.T, history int) (*Runtime, sessions.CoordinationSession) {
	t.Helper()
	r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
		t.Error("recovery executed uncertain intent")
		return answer("unexpected")
	}), 1, 4)
	suspendAutoRelease(t, r)
	if err := r.update(t.Context(), func(s *State) error {
		s.Members["m"] = &Member{ID: "m", AgentName: "/root/m", Name: "m", Label: "Member", ReadOnly: true, Execution: "active", Task: "active"}
		s.Executions["active"] = &Execution{ID: "active", Member: "m", Status: "running", Generation: 7, Workflow: "w", Intent: []messages.ChatMessage{{Role: "assistant", Content: "uncertain"}}}
		s.Tasks["active"] = &Task{ID: "active", Owner: "m", Execution: "active", Revision: 1, Status: "running", Requirement: RequirementDelivered}
		s.Executions["finished"] = &Execution{ID: "finished", Member: "m", Status: "completed", Workflow: "w", Result: &AgentResult{Task: "delivery", Session: "m", Execution: "finished", Revision: 1}}
		s.Tasks["delivery"] = &Task{ID: "delivery", Owner: "m", Execution: "finished", Revision: 1, Status: "running", Requirement: RequirementDelivered, Result: json.Number("9007199254740993")}
		s.Tasks["old-delivery"] = &Task{ID: "old-delivery", Status: "done", Delivery: &TaskDelivery{Via: "workflow_step", Ref: "w/old", Execution: "former", Revision: 3}}
		s.Workflows["w"] = &workflow.Report{ID: "w", Name: "Interrupted", Status: "running", Input: json.Number("9007199254740993"), Steps: []workflow.Step{{ID: "w/0", Kind: "exec", Status: "running"}}}
		s.Followups["refresh"] = &FollowupCall{Member: "m", Refresh: true, Phase: "preparing"}
		s.Messages["refresh"] = &Mail{ID: "refresh", From: r.ID, To: "m", Start: true, Posted: time.Unix(1, 0).UTC()}
		s.ParentTurns[r.ID] = &ParentTurn{RetryPause: &RetryPause{Reason: "provider", Attempts: 2}, Intent: []messages.ChatMessage{{Role: "assistant", ToolCalls: []messages.ChatMessageToolCall{{ID: "uncertain-call", Name: "bash", Arguments: `{}`}}}}}
		for i := range history {
			id := fmt.Sprintf("history-%06d", i)
			s.Executions[id] = &Execution{ID: id, Status: "completed", Request: AgentRequest{Input: strings.Repeat("x", 4096)}}
			s.Tasks[id] = &Task{ID: id, Status: "done", Result: strings.Repeat("y", 4096)}
			s.Messages[id] = &Mail{ID: id, To: "unrelated", Delivered: true, Text: strings.Repeat("z", 512), Posted: time.Unix(int64(i+2), 0).UTC()}
			s.Followups[id] = &FollowupCall{Phase: "launched"}
			s.Workflows[id] = &workflow.Report{ID: id, Status: "completed", Acknowledged: true, Steps: []workflow.Step{{ID: id + "/0", Status: "completed"}, {ID: id + "/1", Status: "completed"}}}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	return r, r.parent
}

func assertRecoveryConverged(t *testing.T, old *Runtime, parent sessions.CoordinationSession) {
	t.Helper()
	s, err := old.read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if e := s.Executions["active"]; e.Status != "paused" || e.Generation != 8 || len(e.Intent) != 1 || e.Ended.IsZero() {
		t.Fatalf("execution: %+v", e)
	}
	if f := s.Followups["refresh"]; f.Phase != "interrupted" || s.Messages["refresh"].Start {
		t.Fatalf("followup: %+v", f)
	}
	if s.Workflows["w"].Status != "interrupted" {
		t.Fatal(s.Workflows["w"])
	}
	notices, results := 0, 0
	for _, mail := range s.Messages {
		if mail.Workflow == "w" {
			notices++
			if want := workflowTerminalText(s, s.Workflows["w"]); mail.Text != want {
				t.Fatalf("notice:\n%s\nwant:\n%s", mail.Text, want)
			}
		}
		if mail.Task == "delivery" {
			results++
		}
	}
	if notices != 1 || results != 1 {
		t.Fatalf("duplicate/missing notices: workflow=%d delivery=%d", notices, results)
	}
	if turn := s.ParentTurns[old.ID]; turn == nil || len(turn.Intent) != 0 || turn.RetryPause == nil {
		t.Fatalf("turn: %+v", turn)
	}
	history, err := parent.(sessions.Session).GetHistory(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	receipts := 0
	for _, m := range history {
		if m.Role == messages.MessageRoleTool && m.ToolCallID == "uncertain-call" {
			receipts++
		}
	}
	if receipts != 1 {
		t.Fatalf("parent interrupted receipts=%d", receipts)
	}
}

func TestRecoveryProductionConstructorBoundedValidationAndSelectedWork(t *testing.T) {
	t.Parallel()
	var baseline [6]uint64
	for _, retained := range []int{4, 300} {
		r, parent := recoveryFixture(t, retained)
		probe := probeRecovery(parent)
		config := r.config
		config.Parent = probe
		fresh, err := New(config)
		if err != nil {
			t.Fatal(err)
		}
		if fresh.initializing.Load() {
			t.Fatal("initialization barrier not cleared")
		}
		if err := fresh.Close(); err != nil {
			t.Fatal(err)
		}
		assertRecoveryConverged(t, r, parent)
		metrics := probe.work.Snapshot()
		got := [6]uint64{uint64(probe.gets), uint64(probe.selected), uint64(probe.puts), metrics.Count(runtimemetrics.SQLStatements), metrics.Count(runtimemetrics.RowsRead), metrics.Count(runtimemetrics.PayloadBytesRead)}
		if retained == 4 {
			baseline = got
		} else if !reflect.DeepEqual(got[:5], baseline[:5]) {
			t.Fatalf("selected recovery scaled with completed history: %v vs %v", got, baseline)
		}
		if probe.passes != 1 || probe.validated < retained*7 || probe.maxPage > recoveryPageBytes {
			t.Fatalf("validation passes=%d records=%d max bytes=%d", probe.passes, probe.validated, probe.maxPage)
		}
		t.Logf("history=%d cold rows=%d cold bytes=%d selected work=%v", retained, probe.cold.Snapshot().Count(runtimemetrics.RowsRead), probe.cold.Snapshot().Count(runtimemetrics.PayloadBytesRead), got)
		// Status-only writes preserve opaque JSON numbers exactly.
		if err := parent.(sessions.CoordinationRecords).ReadRecords(t.Context(), func(rd sessions.CoordinationReader) error {
			w, _, err := rd.Get(sessions.RecordKey{Kind: "workflow", ID: "w"})
			if err == nil && !strings.Contains(string(w.Payload), `9007199254740993`) {
				t.Fatal("recovery rounded saved input")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRecoveryInterruptedBatchesConvergeWithoutDuplicateReceipts(t *testing.T) {
	t.Parallel()
	for _, committed := range []bool{false, true} {
		for failAt := 1; failAt <= 6; failAt++ {
			t.Run(fmt.Sprintf("committed=%v/batch=%d", committed, failAt), func(t *testing.T) {
				r, parent := recoveryFixture(t, 0)
				probe := probeRecovery(parent)
				probe.failAt, probe.committed = failAt, committed
				config := r.config
				config.Parent = probe
				if fresh, err := New(config); !errors.Is(err, errRecoveryCommit) || fresh != nil {
					t.Fatalf("failed recovery=%v %v", fresh, err)
				}
				for range 2 {
					config.Parent = probeRecovery(parent)
					fresh, err := New(config)
					if err != nil {
						t.Fatal(err)
					}
					if err := fresh.Close(); err != nil {
						t.Fatal(err)
					}
				}
				assertRecoveryConverged(t, r, parent)
			})
		}
	}
}

func TestRecoveryRejectsLateMalformedPayloadBeforeAnyMutation(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"execution", "workflow", "workflow_step", "member", "context", "format"} {
		t.Run(kind, func(t *testing.T) {
			r, parent := recoveryFixture(t, 80)
			probe := probeRecovery(parent)
			key := sessions.RecordKey{Kind: kind, ID: "w"}
			payload := `{"steps":[{}]}`
			switch kind {
			case "execution":
				key.ID, payload = "history-000079", `{"usage":{"inputTokens":"bad"}}`
			case "workflow_step":
				key.ID, payload = "w/0", `{"started":"invalid"}`
			case "member":
				key.ID, payload = "m", `{"sandboxPresets":["unknown-preset"]}`
			case "context":
				if err := parent.UpdateCoordination(t.Context(), func(raw *sessions.CoordinationState) error {
					raw.Records["context"] = map[string]json.RawMessage{"w": json.RawMessage(`{"id":"w"}`)}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				payload = `{"sandboxPresets":["unknown-preset"]}`
			case "format":
				key.ID, payload = formatID, `{"version":999}`
			}
			probe.override = map[sessions.RecordKey]json.RawMessage{key: json.RawMessage(payload)}
			config := r.config
			config.Parent = probe
			fresh, err := New(config)
			if err == nil {
				fresh.Close()
				t.Fatal("accepted malformed saved payload")
			}
			if probe.writes != 0 {
				t.Fatalf("mutated %d batches before validation failed: %v", probe.writes, err)
			}
			state, err := r.read(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if state.Executions["active"].Status != "running" || len(state.ParentTurns[r.ID].Intent) == 0 {
				t.Fatal("rejection consumed recovery intent")
			}
		})
	}
}
