package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
)

type compactTestLLM struct {
	reply func(context.Context, *llm.CompletionRequest) (messages.ChatMessage, error)
}

func (m *compactTestLLM) ChatCompletionStream(ctx context.Context, req *llm.CompletionRequest, processor llm.EventStreamProcessor) <-chan *messages.StreamEvent {
	response, err := m.reply(ctx, req)
	if err != nil {
		events := make(chan *messages.StreamEvent, 1)
		events <- &messages.StreamEvent{Type: messages.EventTypeError, Error: err}
		close(events)
		return events
	}
	input := make(chan messages.ChatMessage, 1)
	input <- response
	close(input)
	return processor.ProcessMessagesToEvents(ctx, input)
}

func compactTestReply() messages.ChatMessage {
	msg := messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "The user requested a change; implementation and tests are complete.", StopReason: messages.StopReasonEndTurn}
	msg.SetTokenUsage(2000, 20)
	return msg
}

func compactTestState(t *testing.T, name string, model llm.LLM) *conversationState {
	t.Helper()
	store := testOpenMemoryStore(t, nil)
	session := testAcquireSession(t, store, name)
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleSystem, Content: "custom persona"},
		{Role: messages.MessageRoleUser, Content: "original-user-detail " + strings.Repeat("requirements ", 500)},
		{Role: messages.MessageRoleAssistant, Content: "original-answer-detail " + strings.Repeat("implemented and verified ", 500)},
	}
	testAddMessages(t, session, history)
	registry := tools.NewToolRegistry(nil)
	agent := llm.NewAgent(model, registry, llm.AgentConfig{CompactionModel: "test/summary"})
	t.Cleanup(func() { _ = agent.Close(); _ = registry.Close() })
	return &conversationState{
		session: session, sessionStore: store, toolRegistry: registry, agent: agent,
		settings: Settings{Model: "test/session", CompactModel: "test/summary", MaxHistoryTokens: 64000, SystemPrompt: "custom persona"},
	}
}

type compactTestUI struct {
	childTurnUI
	notices []string
	used    int
	out     int
}

func (u *compactTestUI) AppendNotice(text string)                                    { u.notices = append(u.notices, text) }
func (u *compactTestUI) RecordContextUsage(used int, _ bool, _ contextBudgetDetails) { u.used = used }
func (u *compactTestUI) RecordTurnTokens(_, out int, _ bool)                         { u.out = out }

func TestCompactCommandRegistryAndValidation(t *testing.T) {
	if defaultReplCommands.busySafeCommand("/compact") || startupSafeCommand("/compact") {
		t.Fatal("compaction must initialize the session and queue behind an active turn")
	}
	if completed, _, ok := defaultReplCommands.complete("/comp", nil); !ok || completed != "/compact" {
		t.Fatalf("completion: %q, %v", completed, ok)
	}
	if detail := strings.Join(defaultReplCommands.helpFor("/compact"), "\n"); !strings.Contains(detail, "usage: /compact") || !strings.Contains(detail, "keep the saved transcript") {
		t.Fatalf("missing compaction help: %q", detail)
	}
	ctx := &replCommandContext{}
	if got := strings.Join(dispatchDefaultCommandForTest(t, "/compact", ctx), "\n"); !strings.Contains(got, "no active session") {
		t.Fatalf("missing-session reply: %q", got)
	}
	if got := strings.Join(dispatchDefaultCommandForTest(t, "/compact extra", ctx), "\n"); got != "usage: /compact" {
		t.Fatalf("arguments accepted: %q", got)
	}
}

func TestCompactCommandFallbackPersistsSummaryWithoutATurn(t *testing.T) {
	calls := 0
	model := &compactTestLLM{reply: func(_ context.Context, req *llm.CompletionRequest) (messages.ChatMessage, error) {
		calls++
		if req.Model != "test/summary" || len(req.Tools) != 0 || !strings.Contains(projectedRequestText(req.Messages), "original-answer-detail") || strings.Contains(projectedRequestText(req.Messages), "/compact") {
			t.Fatalf("not a pure compaction request: %+v", req)
		}
		return compactTestReply(), nil
	}}
	state := compactTestState(t, "compact-fallback", model)
	before := testSessionHistory(t, state.session)
	settings := state.settings.clone()
	var output, notices bytes.Buffer
	config := &Config{}
	ui := newLineTurnUI(config, nil)
	ui.interactive = true
	ui.writer, ui.errWriter = &output, &notices
	command := newWriterReplCommandContext(config, state, &notices)
	command.compactConversation = func() error { return executeCompaction(context.Background(), config, state, ui) }
	if err := runREPLLoopWithCommands(context.Background(), bufio.NewReader(strings.NewReader("/compact\n/exit\n")), &notices, command, func(string) error {
		t.Fatal("/compact continued the normal agent turn")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || output.Len() != 0 {
		t.Fatalf("calls=%d answer=%q", calls, output.String())
	}
	after := testSessionHistory(t, state.session)
	if len(after) != len(before)+2 || !reflect.DeepEqual(after[:len(before)], before) {
		t.Fatalf("original transcript changed or a user turn was added: %+v", after)
	}
	if c, ok := after[len(after)-1].Compaction(); !ok || c.KeepsTurn || !strings.Contains(c.Summary, "implementation") {
		t.Fatalf("missing full-session summary: %+v", after[len(after)-1])
	}
	view := projectedRequestText(llm.RequestView(after, state.viewTools()))
	if strings.Contains(view, "original-user-detail") || !strings.Contains(view, "implementation") {
		t.Fatalf("future context not compacted: %q", view)
	}
	if !reflect.DeepEqual(state.settings, settings) || !strings.Contains(notices.String(), "saved to this session · original transcript retained") {
		t.Fatalf("settings or save confirmation: %+v, %q", state.settings, notices.String())
	}
	if err := executeCompaction(context.Background(), config, state, &compactTestUI{}); err != nil || calls != 1 {
		t.Fatalf("repeat compaction not a no-op: err=%v calls=%d", err, calls)
	}
}

func TestCompactEmptySessionDoesNotCallModel(t *testing.T) {
	state := compactTestState(t, "compact-empty", &compactTestLLM{reply: func(context.Context, *llm.CompletionRequest) (messages.ChatMessage, error) {
		t.Fatal("empty context called the model")
		return messages.ChatMessage{}, nil
	}})
	if err := state.session.Clear(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := testSessionHistory(t, state.session)
	ui := &compactTestUI{}
	if err := executeCompaction(context.Background(), &Config{}, state, ui); err != nil {
		t.Fatal(err)
	}
	if after := testSessionHistory(t, state.session); !reflect.DeepEqual(before, after) || !strings.Contains(strings.Join(ui.notices, "\n"), "Nothing to compact") {
		t.Fatalf("empty compaction changed history or omitted explanation: history=%+v notices=%v", after, ui.notices)
	}
}

type compactFailSaveSession struct{ sessions.Session }

func (s compactFailSaveSession) AddMessages(context.Context, []messages.ChatMessage) error {
	return errors.New("disk full")
}

func TestCompactionFailureDoesNotChangeContextOrClaimSave(t *testing.T) {
	for _, failure := range []string{"provider", "save", "cancel"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			model := &compactTestLLM{reply: func(context.Context, *llm.CompletionRequest) (messages.ChatMessage, error) {
				if failure == "provider" {
					return messages.ChatMessage{}, errors.New("provider unavailable")
				}
				if failure == "cancel" {
					cancel()
					return messages.ChatMessage{}, context.Canceled
				}
				return compactTestReply(), nil
			}}
			state := compactTestState(t, "compact-failure", model)
			before := testSessionHistory(t, state.session)
			if failure == "save" {
				state.session = compactFailSaveSession{state.session}
			}
			ui := &compactTestUI{}
			if err := executeCompaction(ctx, &Config{}, state, ui); err == nil {
				t.Fatal("failed compaction reported success")
			}
			after := testSessionHistory(t, state.session)
			if !reflect.DeepEqual(before, after) || ui.used != 0 || strings.Contains(strings.Join(ui.notices, "\n"), "saved to this session") {
				t.Fatalf("failed compaction applied or claimed a save: history=%+v, notices=%v, used=%d", after, ui.notices, ui.used)
			}
		})
	}
}

func TestManagedCompactionRunsOffLockAndStaysOnItsTab(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	model := &compactTestLLM{reply: func(ctx context.Context, _ *llm.CompletionRequest) (messages.ChatMessage, error) {
		close(started)
		select {
		case <-release:
			return compactTestReply(), nil
		case <-ctx.Done():
			return messages.ChatMessage{}, ctx.Err()
		}
	}}
	state := compactTestState(t, "compact-origin", model)
	r := newManagedREPL(&Config{}, "-", 0, 0)
	t.Cleanup(func() { _ = r.closeTabs() })
	if err := r.addTab(state); err != nil {
		t.Fatal(err)
	}
	origin := r.visibleTab()
	r.model.mu.Lock()
	r.runCommand("/compact")
	r.model.mu.Unlock()
	if !origin.model.busy || !origin.model.currentTurn.compact {
		t.Fatal("compaction did not enter the cancellable turn lifecycle")
	}
	r.startPendingTurn(context.Background(), func(context.Context, string, TurnUI) error {
		t.Error("manual compaction continued the normal turn")
		return nil
	})
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("compaction did not start")
	}
	// A long model call must not hold the UI lock, and later mutations wait.
	origin.model.mu.Lock()
	origin.model.queue = append(origin.model.queue, queuedREPLInput{text: "/set temp 0.5"})
	origin.model.mu.Unlock()
	other := compactTestState(t, "compact-other", &captureCompletionLLM{})
	if err := r.addTab(other); err != nil {
		t.Fatal(err)
	}
	otherBefore := testSessionHistory(t, other.session)
	close(release)
	select {
	case err := <-origin.turnDone:
		if err != nil {
			t.Fatal(err)
		}
		r.settleTurn(origin, err)
	case <-time.After(5 * time.Second):
		t.Fatal("compaction did not finish")
	}
	r.startQueued(context.Background(), origin, func(context.Context, string, TurnUI) error { return nil })
	if origin.model.busy || origin.state.settings.Temperature != 0.5 || other.settings.Temperature != 0 {
		t.Fatalf("queued command did not serialize on original tab: origin=%+v other=%+v", origin.state.settings, other.settings)
	}
	if after := testSessionHistory(t, other.session); !reflect.DeepEqual(after, otherBefore) {
		t.Fatal("compaction was redirected to the visible tab")
	}
	got := testSessionHistory(t, state.session)
	if _, ok := got[len(got)-1].Compaction(); !ok {
		t.Fatal("origin session was not compacted")
	}
}

func TestManagedCompactionCancellationKeepsHistory(t *testing.T) {
	started := make(chan struct{})
	model := &compactTestLLM{reply: func(ctx context.Context, _ *llm.CompletionRequest) (messages.ChatMessage, error) {
		close(started)
		<-ctx.Done()
		return messages.ChatMessage{}, ctx.Err()
	}}
	state := compactTestState(t, "compact-cancel", model)
	before := testSessionHistory(t, state.session)
	r := newManagedREPL(&Config{}, "-", 0, 0)
	t.Cleanup(func() { _ = r.closeTabs() })
	if err := r.addTab(state); err != nil {
		t.Fatal(err)
	}
	tab := r.visibleTab()
	r.runCommand("/compact")
	r.startPendingTurn(context.Background(), func(context.Context, string, TurnUI) error { return nil })
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("compaction did not start")
	}
	r.cancelTabTurn(tab)
	select {
	case err := <-tab.turnDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation: %v", err)
		}
		r.settleTurn(tab, err)
	case <-time.After(5 * time.Second):
		t.Fatal("compaction did not cancel")
	}
	if got := testSessionHistory(t, state.session); !reflect.DeepEqual(got, before) {
		t.Fatal("canceled compaction changed history")
	}
	if tab.model.busy || tab.model.ed.text() != "/compact" {
		t.Fatalf("cancellation did not settle and restore the action: busy=%v draft=%q", tab.model.busy, tab.model.ed.text())
	}
}

type compactCancelGateUI struct {
	compactTestUI
	cancel context.CancelFunc
}

func (u *compactCancelGateUI) TurnPersistenceAllowed() bool {
	u.cancel()
	return true
}

func TestCompactionCancellationAtSaveGateKeepsOnlyUsage(t *testing.T) {
	model := &compactTestLLM{reply: func(context.Context, *llm.CompletionRequest) (messages.ChatMessage, error) {
		return compactTestReply(), nil
	}}
	state := compactTestState(t, "compact-gate-cancel", model)
	before := testSessionHistory(t, state.session)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ui := &compactCancelGateUI{cancel: cancel}
	if err := executeCompaction(ctx, &Config{}, state, ui); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation at save gate: %v", err)
	}
	after := testSessionHistory(t, state.session)
	if len(after) != len(before)+1 || !reflect.DeepEqual(after[:len(before)], before) || !after[len(after)-1].IsUsageRecord() {
		t.Fatalf("canceled save must keep only billed usage: %+v", after)
	}
	if ui.used != 0 || strings.Contains(strings.Join(ui.notices, "\n"), "saved to this session") {
		t.Fatalf("canceled compaction announced success: used=%d notices=%v", ui.used, ui.notices)
	}
}

type compactBlockingSaveSession struct {
	sessions.Session
	started chan struct{}
	release chan struct{}
}

func (s compactBlockingSaveSession) AddMessages(ctx context.Context, msgs []messages.ChatMessage) error {
	for _, msg := range msgs {
		if _, ok := msg.Compaction(); ok {
			close(s.started)
			<-s.release
			break
		}
	}
	return s.Session.AddMessages(ctx, msgs)
}

func TestDetachedCompactionCannotCoverNewerHistory(t *testing.T) {
	state := compactTestState(t, "compact-detached-save", &compactTestLLM{reply: func(context.Context, *llm.CompletionRequest) (messages.ChatMessage, error) {
		return compactTestReply(), nil
	}})
	base := state.session
	before := testSessionHistory(t, base)
	blocked := compactBlockingSaveSession{Session: base, started: make(chan struct{}), release: make(chan struct{})}
	state.session = blocked
	r := newManagedREPL(&Config{}, "-", 0, 0)
	t.Cleanup(func() { _ = r.closeTabs() })
	if err := r.addTab(state); err != nil {
		t.Fatal(err)
	}
	tab := r.visibleTab()
	r.runCommand("/compact")
	r.startPendingTurn(context.Background(), func(context.Context, string, TurnUI) error { return nil })
	done := tab.turnDone
	select {
	case <-blocked.started:
	case <-time.After(5 * time.Second):
		t.Fatal("summary save did not start")
	}
	r.cancelTabTurn(tab)
	r.abandonCanceledTurn(tab)
	newer := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "NEW user message after detachment"},
		{Role: messages.MessageRoleAssistant, Content: "NEW assistant answer after detachment"},
	}
	testAddMessages(t, base, newer)
	close(blocked.release)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("detached save result: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("detached save did not finish")
	}
	after := testSessionHistory(t, base)
	wantPrefix := append(before, newer...)
	if len(after) != len(wantPrefix)+1 || !reflect.DeepEqual(after[:len(wantPrefix)], wantPrefix) || !after[len(after)-1].IsUsageRecord() {
		t.Fatalf("detached summary changed history; only usage may follow new input: %+v", after)
	}
	view := projectedRequestText(llm.RequestView(after, state.viewTools()))
	if !strings.Contains(view, newer[0].Content) || !strings.Contains(view, newer[1].Content) || !strings.Contains(view, "original-user-detail") {
		t.Fatalf("stale compaction covered unsummarized conversation: %q", view)
	}
}
