package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
)

func savedTurnAudits(t *testing.T, session sessions.Session) []messages.TurnAudit {
	t.Helper()
	var audits []messages.TurnAudit
	for _, msg := range testSessionHistory(t, session) {
		value, ok := msg.Metadata[messages.MetadataKeyTurnAudit]
		if !ok {
			continue
		}
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var audit messages.TurnAudit
		if err := json.Unmarshal(data, &audit); err != nil {
			t.Fatal(err)
		}
		audits = append(audits, audit)
	}
	return audits
}

// Exercise the same request, callback and persistence path as executeTurn,
// with a short explicit retry policy and an optional cancellation observer.
func runAuditedTestTurn(t *testing.T, ctx context.Context, state *conversationState, reuse bool, onRetry func(llm.RetryEvent)) error {
	t.Helper()
	config := &Config{}
	ui := newLineTurnUI(config, nil)
	ui.writer, ui.errWriter = io.Discard, io.Discard
	execution := &turnExecution{ctx: ctx, config: config, state: state, settings: &state.settings, turnUI: ui, reuseUser: reuse, audit: newTurnAudit()}
	execution.userMsg = execution.audit.input(messages.ChatMessage{Role: messages.MessageRoleUser, Content: "hello"})
	history, _, err := execution.prepareRequest()
	if err != nil {
		t.Fatal(err)
	}
	req, err := execution.completionRequest(history)
	if err != nil {
		t.Fatal(err)
	}
	req.RetryPolicy = &llm.RetryPolicy{MaxRetries: 1, MaxElapsed: time.Second, InitialDelay: time.Microsecond, MaxDelay: time.Microsecond}
	cb := execution.callbacks()
	if onRetry != nil {
		observe := cb.OnRetry
		cb.OnRetry = func(event llm.RetryEvent) { observe(event); onRetry(event) }
	}
	response, err := state.agent.Run(ctx, req, cb)
	if ctx.Err() != nil {
		err = context.Cause(ctx)
	}
	return execution.settlePersistence(response, err)
}

func TestTurnAuditPersistsStreamRetryAndRequestIdentity(t *testing.T) {
	calls := 0
	state, session := newInterruptedTurnState(t, turnRetryStream(func() []*messages.StreamEvent {
		calls++
		if calls == 1 {
			return []*messages.StreamEvent{{Type: messages.EventTypeError, Error: io.ErrUnexpectedEOF}}
		}
		return []*messages.StreamEvent{{Type: messages.EventTypeComplete, Message: &messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "done", StopReason: messages.StopReasonEndTurn}}}
	}), nil)
	if err := runAuditedTestTurn(t, context.Background(), state, false, nil); err != nil {
		t.Fatal(err)
	}
	history := testSessionHistory(t, session)
	audits := savedTurnAudits(t, session)
	if len(audits) != 1 {
		t.Fatalf("audits=%+v", audits)
	}
	a := audits[0]
	if a.Version != 1 || a.ID == "" || a.ID != history[0].Metadata[messages.MetadataKeyTurnID] || a.InputTurnID != a.ID || a.Status != "completed" || a.StartedAt.IsZero() || a.RunFinishedAt.Before(a.StartedAt) || len(a.Attempts) != 2 || len(a.Retries) != 2 {
		t.Fatalf("audit=%+v", a)
	}
	started, err := time.Parse(time.RFC3339Nano, history[0].Metadata[messages.MetadataKeyTurnStartedAt].(string))
	if err != nil || !started.Equal(a.StartedAt) {
		t.Fatalf("input start=%v err=%v audit start=%v", started, err, a.StartedAt)
	}
	first, last := a.Attempts[0], a.Attempts[1]
	if first.ID == last.ID || first.Error == "" || last.Error != "" || first.FinishedAt == nil || last.FinishedAt == nil || last.ID != history[1].DisplayID() || first.Model != "test/model" || last.Attempt != 1 {
		t.Fatalf("attempts=%+v", a.Attempts)
	}
	if a.Retries[0].Source != "stream" || !a.Retries[0].Waiting || a.Retries[1].Waiting || a.Retries[1].WaitError != "" {
		t.Fatalf("retry transitions=%+v", a.Retries)
	}
}

func TestTurnAuditKeepsFailedInputRetryReusable(t *testing.T) {
	model := &scriptedStreamLLM{failErr: errors.New("request rejected")}
	state, session := newInterruptedTurnState(t, model, nil)
	for i := 0; i < 2; i++ {
		err := runAuditedTestTurn(t, context.Background(), state, i > 0, nil)
		if err == nil || turnProgressSaved(err) {
			t.Fatalf("input-only failure claimed generated progress: %v", err)
		}
	}
	history := testSessionHistory(t, session)
	audits := savedTurnAudits(t, session)
	if len(history) != 3 || len(audits) != 2 || !historyEndsWithEquivalentUserMessage(history, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "hello"}) {
		t.Fatalf("retry duplicated input or lost audits: history=%+v audits=%+v", history, audits)
	}
	if audits[0].ID == audits[1].ID || !audits[1].ReusedInput || audits[1].InputTurnID != audits[0].ID || audits[0].Status != "failed" || len(audits[0].Attempts) != 1 {
		t.Fatalf("retry linkage=%+v", audits)
	}
}

func TestTurnAuditPersistsInterruptedRetryWait(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(context.Canceled)
	state, session := newInterruptedTurnState(t, &scriptedStreamLLM{failErr: io.ErrUnexpectedEOF}, nil)
	err := runAuditedTestTurn(t, ctx, state, false, func(event llm.RetryEvent) {
		if event.Waiting {
			cancel(errInteractiveTurnStopped)
		}
	})
	audits := savedTurnAudits(t, session)
	if !errors.Is(err, context.Canceled) || len(audits) != 1 {
		t.Fatalf("err=%v audits=%+v", err, audits)
	}
	a := audits[0]
	if a.Status != "interrupted" || a.InterruptionOrigin != "interactive_stop" || len(a.Attempts) != 1 || len(a.Retries) != 2 || a.Retries[1].WaitError == "" || a.Retries[1].InterruptionOrigin != "interactive_stop" {
		t.Fatalf("interrupted wait audit=%+v", a)
	}
}

func TestTurnAuditCancellationOrigins(t *testing.T) {
	for _, tc := range []struct {
		cause error
		want  string
	}{
		{context.Canceled, "external_context_unknown"},
		{errors.New("custom caller cancellation"), "external_context_unknown"},
		{context.DeadlineExceeded, "context_deadline"},
		{errInteractiveTurnStopped, "interactive_stop"},
		{&shutdownSignal{signal: os.Interrupt}, "process_signal"},
		{sessions.ErrSessionLeaseLost, "session_lease_lost"},
		{sessions.ErrStoreClosed, "session_store_closed"},
	} {
		t.Run(tc.want+"/"+tc.cause.Error(), func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			cancel(tc.cause)
			audit := newTurnAudit().finish(ctx, context.Cause(ctx))
			if audit.Status != "interrupted" || audit.InterruptionOrigin != tc.want || audit.Error != tc.cause.Error() {
				t.Fatalf("audit=%+v", audit)
			}
		})
	}
}

func TestTurnAuditRespectsDetachedPersistenceGate(t *testing.T) {
	state, session := newInterruptedTurnState(t, &scriptedStreamLLM{}, nil)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	r.model.turnID = 2
	execution := &turnExecution{ctx: context.Background(), state: state, audit: newTurnAudit(), inputPersisted: true,
		turnUI: &gotuiTurnUI{repl: r, model: r.model, config: r.config, turnID: 1}}
	if err := execution.settlePersistence(nil, context.Canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("settle=%v", err)
	}
	if history := testSessionHistory(t, session); len(history) != 0 {
		t.Fatalf("detached turn appended audit: %+v", history)
	}
}

func TestTurnAuditRechecksDetachAfterMetadataPersistence(t *testing.T) {
	state, session := newInterruptedTurnState(t, &scriptedStreamLLM{}, nil)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	r.model.turnID = 1
	state.skillRuntime, state.skillSources = &tools.SkillRuntime{}, []string{"test-source"}
	state.session = &auditMetadataSession{Session: session, beforeSet: func() {
		// Model the UI detaching while the metadata write is outstanding,
		// after settlePersistence's initial gate admitted this generation.
		r.model.mu.Lock()
		r.model.turnID++
		r.model.mu.Unlock()
	}}
	execution := &turnExecution{ctx: context.Background(), state: state, audit: newTurnAudit(), inputPersisted: true,
		turnUI: &gotuiTurnUI{repl: r, model: r.model, config: r.config, turnID: 1}}
	if err := execution.settlePersistence(nil, context.Canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("settle=%v", err)
	}
	if history := testSessionHistory(t, session); len(history) != 0 {
		t.Fatalf("turn detached during metadata save appended audit: %+v", history)
	}
}

type auditMetadataSession struct {
	sessions.Session
	beforeSet func()
}

func (s *auditMetadataSession) SetMetadata(ctx context.Context, metadata *sessions.Metadata) error {
	s.beforeSet()
	return s.Session.SetMetadata(ctx, metadata)
}

func TestTurnAuditSequenceFenceRejectsInterveningTurn(t *testing.T) {
	state, session := newInterruptedTurnState(t, &scriptedStreamLLM{}, nil)
	newInput := messages.ChatMessage{Role: messages.MessageRoleUser, Content: "newer turn"}
	state.session = &auditSequenceSession{Session: session, CoordinationSession: session.(sessions.CoordinationSession), beforeUpdate: func() error {
		return session.AddMessage(context.Background(), newInput)
	}}
	execution := &turnExecution{ctx: context.Background(), state: state, audit: newTurnAudit(), inputPersisted: true, turnUI: newLineTurnUI(&Config{}, nil)}
	if err := execution.settlePersistence(nil, errors.New("old failure")); err == nil || !strings.Contains(err.Error(), "checkpoint sequence changed") {
		t.Fatalf("intervening append was not fenced: %v", err)
	}
	if history := testSessionHistory(t, session); len(history) != 1 || !equivalentUserMessage(history[0], newInput) {
		t.Fatalf("old audit followed newer input: %+v", history)
	}
}

type auditSequenceSession struct {
	sessions.Session
	sessions.CoordinationSession
	beforeUpdate func() error
}

func (s *auditSequenceSession) UpdateCoordination(ctx context.Context, update func(*sessions.CoordinationState) error) error {
	if err := s.beforeUpdate(); err != nil {
		return err
	}
	return s.CoordinationSession.UpdateCoordination(ctx, update)
}

func TestRootTurnAuditRecordsCancellationDuringTool(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(context.Canceled)
	model := &scriptedStreamLLM{responses: []messages.ChatMessage{{
		Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse,
		ToolCalls: []messages.ChatMessageToolCall{{ID: "probe-1", Name: "probe", Arguments: `{}`}},
	}}}
	probe := &tools.Func{Name: "probe", Run: func(context.Context, tools.Args) (string, error) {
		cancel(errInteractiveTurnStopped)
		return "completed before cancellation", nil
	}}
	state, session := newInterruptedTurnState(t, model, probe)
	config := &Config{}
	ui := newLineTurnUI(config, nil)
	ui.writer, ui.errWriter = io.Discard, io.Discard
	_, err := executeTurnWithUserMessage(ctx, config, state, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "probe"}, nil, nil, ui, false)
	audits := savedTurnAudits(t, session)
	if !errors.Is(err, errInteractiveTurnStopped) || !turnProgressSaved(err) || len(audits) != 1 || audits[0].InterruptionOrigin != "interactive_stop" || audits[0].Status != "interrupted" || len(audits[0].Attempts) != 1 || audits[0].Attempts[0].Error != "" {
		t.Fatalf("tool cancellation err=%v audit=%+v", err, audits)
	}
	history := testSessionHistory(t, session)
	if len(history) != 4 || history[2].Content != "completed before cancellation" || history[1].ToolCalls[0].DisplayID == "" || history[1].ToolCalls[0].DisplayID != history[2].DisplayID() {
		t.Fatalf("completed tool exchange changed: %+v", history)
	}
}

func TestTurnAuditDoesNotRenderOrReachProvider(t *testing.T) {
	user := messages.ChatMessage{Role: messages.MessageRoleUser, Content: "hello"}
	answer := messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "answer", StopReason: messages.StopReasonEndTurn}
	marker := messages.ChatMessage{Role: messages.MessageRoleInternal, Metadata: map[string]any{messages.MetadataKeyTurnAudit: newTurnAudit().finish(context.Background(), nil)}}
	for _, history := range [][]messages.ChatMessage{{user}, {user, answer}} {
		before, after := newReplModel(), newReplModel()
		before.hydrateHistory(history, "test")
		withAudit := append(append([]messages.ChatMessage(nil), history...), marker)
		after.hydrateHistory(withAudit, "test")
		if before.fullTranscript() != after.fullTranscript() || before.ed.text() != after.ed.text() || before.restoreDraftNext != after.restoreDraftNext {
			t.Fatalf("audit changed hydrated display or draft: before=%q after=%q", before.fullTranscript(), after.fullTranscript())
		}
	}
	seen := false
	agent := llm.NewAgent(auditInspectLLM(func(req *llm.CompletionRequest) {
		seen = true
		for _, msg := range req.Messages {
			if msg.Role == messages.MessageRoleInternal || msg.Metadata[messages.MetadataKeyTurnAudit] != nil {
				t.Fatalf("audit leaked to provider: %+v", msg)
			}
		}
	}), nil, llm.AgentConfig{})
	defer agent.Close()
	if _, err := agent.Run(context.Background(), &llm.CompletionRequest{Messages: []messages.ChatMessage{user, marker}}, nil); err != nil || !seen {
		t.Fatalf("provider request err=%v seen=%v", err, seen)
	}
}

type auditInspectLLM func(*llm.CompletionRequest)

func (f auditInspectLLM) ChatCompletionStream(_ context.Context, req *llm.CompletionRequest, _ llm.EventStreamProcessor) <-chan *messages.StreamEvent {
	f(req)
	events := make(chan *messages.StreamEvent, 1)
	events <- &messages.StreamEvent{Type: messages.EventTypeComplete, Message: &messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "done", StopReason: messages.StopReasonEndTurn}}
	close(events)
	return events
}
