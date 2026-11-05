package main

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

func TestThinkingClockStartsOncePerInterval(t *testing.T) {
	m := newReplModel()
	m.appendThinking("first")
	record := m.currentReasoningRecord()
	if record == nil || !record.active || !m.thinkingSegmentOpen || m.thinkingSegmentStart.IsZero() {
		t.Fatal("new reasoning record did not start its clock")
	}
	start := time.Now().Add(-time.Second)
	m.thinkingSegmentStart = start
	m.appendThinking(" second")
	if m.currentReasoningRecord() != record || m.thinkingSegmentStart != start {
		t.Fatal("another delta restarted the running clock")
	}
	m.pauseThinkingClock()
	elapsed := record.elapsed
	m.appendThinking(" third")
	if m.currentReasoningRecord() != record || !record.active || !m.thinkingSegmentOpen || !m.thinkingSegmentStart.After(start) {
		t.Fatal("paused reasoning record did not resume its clock")
	}
	if elapsed < time.Second || record.elapsed != elapsed || string(record.tail) != "first second third" {
		t.Fatalf("resumption changed banked time or text: %+v", record)
	}
	m.finishThinkingSegment()
	m.appendThinking("next")
	if next := m.currentReasoningRecord(); next == nil || next == record || !next.active || len(m.reasoningOrder) != 2 {
		t.Fatal("closed reasoning record was reused instead of starting a new one")
	}
}

func TestTurnHiddenTextPausesThinking(t *testing.T) {
	m := newReplModel()
	m.busy = true
	ui := &gotuiTurnUI{model: m, config: &Config{}}
	execution := &turnExecution{turnUI: ui, config: ui.config, state: &conversationState{}}
	cb := execution.callbacks()
	cb.OnModelRequest(0, 0)
	cb.OnReasoning("first thought")
	record := m.currentReasoningRecord()
	m.thinkingSegmentStart = time.Now().Add(-2 * time.Second)
	cb.OnCommentary(messages.AssistantText{Text: "hidden commentary"}, true)
	if m.thinkingSegmentOpen || record.active || m.state == turnStateThinking || record.elapsed < 2*time.Second {
		t.Fatalf("commentary left a running clock: record=%+v state=%v", record, m.state)
	}
	first := record.elapsed
	cb.OnReasoning("second thought")
	if m.currentReasoningRecord() != record || !record.active {
		t.Fatal("hidden commentary broke the reasoning disclosure instead of pausing it")
	}
	m.thinkingSegmentStart = time.Now().Add(-3 * time.Second)
	cb.OnContent("\n\n") // trimmed answer text still ends the thinking interval
	if m.thinkingSegmentOpen || record.active || record.elapsed < first+3*time.Second {
		t.Fatalf("trimmed content left a running clock: %+v", record)
	}
	second := record.elapsed
	cb.OnReasoning("third thought")
	m.thinkingSegmentStart = time.Now().Add(-time.Second)
	cb.OnIterationUsage(0, 10, 20) // reasoning-only completion, before tools/settlement
	if m.thinkingSegmentOpen || record.active || record.elapsed < second+time.Second {
		t.Fatalf("completed response left a running clock: %+v", record)
	}
	if got := string(record.tail); got != "first thoughtsecond thoughtthird thought" {
		t.Fatalf("reasoning intervals = %q", got)
	}
	if got := strings.Join(transcriptTexts(m), "\n"); strings.Contains(got, "hidden commentary") || m.currentAssistant >= 0 {
		t.Fatalf("hidden text became displayed prose: %q", got)
	}
}

func TestLineHiddenTextPausesThinking(t *testing.T) {
	ui := &lineTurnUI{config: &Config{}, writer: io.Discard, errWriter: io.Discard,
		activity: &lineActivity{details: &lineActivityDetails{}}}
	execution := &turnExecution{turnUI: ui, config: ui.config, state: &conversationState{}}
	cb := execution.callbacks()
	cb.OnReasoning("first thought")
	ui.activity.thinkingSince = time.Now().Add(-2 * time.Second)
	cb.OnCommentary(messages.AssistantText{Text: "hidden commentary"}, true)
	if !ui.activity.thinkingSince.IsZero() || ui.activity.state == turnStateThinking || ui.activity.thought < 2*time.Second {
		t.Fatalf("commentary left a running line clock: %+v", ui.activity)
	}
	first := ui.activity.thought
	cb.OnReasoning("second thought")
	ui.activity.thinkingSince = time.Now().Add(-time.Second)
	cb.OnIterationUsage(0, 10, 20)
	if !ui.activity.thinkingSince.IsZero() || ui.activity.thought < first+time.Second {
		t.Fatalf("completed response left a running line clock: %+v", ui.activity)
	}
	if got := string(ui.activity.details.thought.tail); got != "first thoughtsecond thought" {
		t.Fatalf("line reasoning intervals = %q", got)
	}
}

func TestReasoningKeepsToolAndModelRoundSeparators(t *testing.T) {
	for _, surface := range []string{"tui", "line"} {
		for _, boundary := range []string{"tool", "next response"} {
			t.Run(surface+"/"+boundary, func(t *testing.T) {
				var ui TurnUI
				var text func() string
				if surface == "tui" {
					m := newReplModel()
					m.busy = true
					ui = &gotuiTurnUI{model: m, config: &Config{}}
					text = func() string { return string(m.currentReasoningRecord().tail) }
				} else {
					line := &lineTurnUI{config: &Config{}, writer: io.Discard, errWriter: io.Discard,
						activity: &lineActivity{details: &lineActivityDetails{}}}
					ui = line
					text = func() string { return string(line.activity.details.thought.tail) }
				}
				ui.ShowThinking("first")
				ui.ThinkingEnded() // completion may stop the clock before the boundary
				if boundary == "tool" {
					ui.AppendToolStart([]messages.ChatMessageToolCall{{ID: "probe", Name: "probe", Arguments: `{}`}})
				} else {
					ui.(attemptTurnUI).ModelAttemptStarted(1, 0)
				}
				ui.ThinkingEnded() // hidden text must not clear the pending separator
				ui.ShowThinking("")
				ui.ShowThinking("second")
				ui.ThinkingEnded()
				ui.ShowThinking("third")
				if got := text(); got != "first\nsecondthird" {
					t.Fatalf("reasoning across response boundary = %q", got)
				}
			})
		}
	}
}

func TestInterruptedSharedReasoningPreservesInResponseText(t *testing.T) {
	m := newReplModel()
	m.busy = true
	ui := &gotuiTurnUI{model: m, config: &Config{}}
	ui.ModelAttemptStarted(0, 0)
	ui.ShowThinking("saved")
	ui.ThinkingEnded()
	ui.ModelAttemptStarted(1, 0)
	ui.ShowThinking("first")
	ui.ThinkingEnded()
	ui.ShowThinking("second")
	ui.ModelAttemptAborted(1, 0, context.Canceled)
	if len(m.reasoningOrder) != 2 {
		t.Fatalf("reasoning records = %d, want saved and unsaved", len(m.reasoningOrder))
	}
	failed := m.reasoningRecords.get(m.reasoningOrder[1])
	if !failed.unsaved || string(failed.tail) != "firstsecond" {
		t.Fatalf("failed snapshot inserted a text boundary: %+v", failed)
	}
}

func TestInterruptedReasoningKeepsSavedPrefixAcrossReload(t *testing.T) {
	for _, failure := range []string{"cancel", "nonretryable"} {
		for _, separate := range []bool{false, true} {
			name := failure + "/coalesced"
			if separate {
				name = failure + "/after prose"
			}
			t.Run(name, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				terminal := errors.New("provider rejected request")
				calls, executed := 0, 0
				model := turnRetryStream(func() []*messages.StreamEvent {
					calls++
					if calls == 1 {
						return []*messages.StreamEvent{
							{Type: messages.EventTypeReasoning, Content: "saved reasoning"},
							{Type: messages.EventTypeComplete, Message: &messages.ChatMessage{
								Role: messages.MessageRoleAssistant, Reasoning: "saved reasoning", StopReason: messages.StopReasonToolUse,
								ToolCalls: []messages.ChatMessageToolCall{{ID: "saved-tool", Name: "probe", Arguments: `{}`}},
							}},
						}
					}
					var events []*messages.StreamEvent
					if separate {
						events = append(events, &messages.StreamEvent{Type: messages.EventTypeContent, Content: "partial answer"})
					}
					events = append(events, &messages.StreamEvent{Type: messages.EventTypeReasoning, Content: "unsaved reasoning"})
					if failure == "nonretryable" {
						events = append(events, &messages.StreamEvent{Type: messages.EventTypeError, Error: terminal})
					}
					return events
				})
				registry := tools.NewToolRegistry([]tools.Tool{&tools.Func{Name: "probe", Run: func(context.Context, tools.Args) (string, error) {
					executed++
					return "saved tool result", nil
				}}})
				defer registry.Close()
				agent := llm.NewAgent(model, registry, llm.AgentConfig{})
				defer agent.Close()
				dbPath := filepath.Join(t.TempDir(), "session.db")
				store := testOpenDiskStore(t, dbPath, nil)
				session := testAcquireSession(t, store, "interrupted-reasoning")
				m := newReplModel()
				m.busy = true
				ui := &gotuiTurnUI{model: m, config: &Config{ShotScript: "test"}}
				state := &conversationState{session: session, artifactStore: session.ArtifactStore(), toolRegistry: registry}
				user := messages.ChatMessage{Role: messages.MessageRoleUser, Content: "run probe"}
				execution := &turnExecution{ctx: ctx, turnUI: ui, config: ui.config, state: state, settings: &Settings{}, userMsg: user}
				cb := execution.callbacks()
				reasoning := cb.OnReasoning
				cb.OnReasoning = func(chunk string) {
					reasoning(chunk)
					if chunk == "unsaved reasoning" && failure == "cancel" {
						m.canceling = true // cleanup must work after display callbacks stop accepting
						cancel()
					}
				}
				response, runErr := agent.Run(ctx, &llm.CompletionRequest{Messages: []messages.ChatMessage{user}}, cb)
				wantErr := terminal
				if failure == "cancel" {
					wantErr = context.Canceled
				}
				if !errors.Is(runErr, wantErr) || calls != 2 || executed != 1 {
					t.Fatalf("run: calls=%d executed=%d err=%v", calls, executed, runErr)
				}
				err := execution.settlePersistence(response, runErr)
				if !errors.Is(err, wantErr) || !turnProgressSaved(err) {
					t.Fatalf("settlement = %v, want failure with saved progress", err)
				}
				m.settleTurnActivity("interrupted", !turnProgressSaved(err))
				if len(m.reasoningOrder) != 2 {
					t.Fatalf("reasoning disclosures = %d, want saved prefix and unsaved suffix", len(m.reasoningOrder))
				}
				accepted := m.reasoningRecords.get(m.reasoningOrder[0])
				failed := m.reasoningRecords.get(m.reasoningOrder[1])
				if accepted.unsaved || accepted.active || string(accepted.tail) != "saved reasoning" {
					t.Fatalf("saved reasoning changed ownership: %+v", accepted)
				}
				if !failed.unsaved || failed.active || !failed.complete || string(failed.tail) != "unsaved reasoning" || failed.elapsed <= 0 {
					t.Fatalf("partial reasoning not retained as unsaved: %+v", failed)
				}
				if transcript := strings.Join(transcriptTexts(m), "\n"); strings.Contains(transcript, "retry") || strings.Count(transcript, "discarded attempt 1") != 1 {
					t.Fatalf("terminal interruption mislabeled or repeated: %q", transcript)
				}
				if err := session.Close(); err != nil {
					t.Fatal(err)
				}
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
				reopenedStore := testOpenDiskStore(t, dbPath, nil)
				reopened := testAcquireSession(t, reopenedStore, "interrupted-reasoning")
				history := testSessionHistory(t, reopened)
				if len(history) != 4 || history[1].Reasoning != "saved reasoning" || history[2].Content != "saved tool result" {
					t.Fatalf("saved work changed after reload: %+v", history)
				}
				for _, message := range history {
					if strings.Contains(message.Reasoning, "unsaved") || strings.Contains(message.Content, "partial answer") {
						t.Fatalf("provisional output persisted: %+v", message)
					}
				}
				assertInterruptedMarker(t, history[3], wantErr.Error())
				reloaded := newReplModel()
				reloaded.hydrateHistory(history, "interrupted-reasoning")
				if len(reloaded.reasoningOrder) != 1 {
					t.Fatalf("reloaded %d reasoning disclosures, want only the saved prefix", len(reloaded.reasoningOrder))
				}
				restored := reloaded.reasoningRecords.get(reloaded.reasoningOrder[0])
				if restored.unsaved || string(restored.tail) != "saved reasoning" || restored.elapsed != history[1].ThinkingDuration() {
					t.Fatalf("restored reasoning differs from persistence: %+v", restored)
				}
			})
		}
	}
}

// Use the real processor: reasoning deltas concatenate within one response,
// even when commentary or answer text arrives between them.
type turnReasoningStream []messages.ChatMessage

func (chunks turnReasoningStream) ChatCompletionStream(ctx context.Context, _ *llm.CompletionRequest, processor llm.EventStreamProcessor) <-chan *messages.StreamEvent {
	input := make(chan messages.ChatMessage, len(chunks))
	for _, chunk := range chunks {
		input <- chunk
	}
	close(input)
	return processor.ProcessMessagesToEvents(ctx, input)
}

func commentaryReasoningStream() turnReasoningStream {
	return turnReasoningStream{
		{Reasoning: "first"},
		{TextBlocks: []messages.AssistantText{{ID: "update", Phase: messages.PhaseCommentary, Text: "hidden update"}}},
		{Reasoning: "second"},
		{TextBlocks: []messages.AssistantText{{ID: "answer", Phase: messages.PhaseFinalAnswer, Text: "answer"}}, StopReason: messages.StopReasonEndTurn},
	}
}

func TestLineReasoningTextMatchesStreamProcessor(t *testing.T) {
	agent := llm.NewAgent(commentaryReasoningStream(), nil, llm.AgentConfig{})
	defer agent.Close()
	ui := &lineTurnUI{config: &Config{}, writer: io.Discard, errWriter: io.Discard,
		activity: &lineActivity{details: &lineActivityDetails{}}}
	execution := &turnExecution{turnUI: ui, config: ui.config, state: &conversationState{}}
	cb := execution.callbacks()
	cb.BeforeFirstRequest, cb.OnRequestProjection = nil, nil
	response, err := agent.Run(context.Background(), &llm.CompletionRequest{}, cb)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(ui.activity.details.thought.tail); got != "firstsecond" || got != response.Message.Reasoning {
		t.Fatalf("line reasoning = %q, stored reasoning = %q", got, response.Message.Reasoning)
	}
	if !ui.activity.thinkingSince.IsZero() {
		t.Fatal("completion left the line clock running")
	}
}

func TestTurnResumedReasoningTimingMatchesReload(t *testing.T) {
	model := commentaryReasoningStream()
	agent := llm.NewAgent(model, nil, llm.AgentConfig{})
	defer agent.Close()
	session := newTurnPersistenceTestSession(t)
	m := newReplModel()
	m.busy = true
	ui := &gotuiTurnUI{model: m, config: &Config{}}
	user := messages.ChatMessage{Role: messages.MessageRoleUser, Content: "answer"}
	execution := &turnExecution{ctx: context.Background(), turnUI: ui, config: ui.config,
		state: &conversationState{session: session}, settings: &Settings{}, userMsg: user}
	cb := execution.callbacks()
	reasoning, commentary := cb.OnReasoning, cb.OnCommentary
	cb.OnReasoning = func(chunk string) {
		reasoning(chunk)
		time.Sleep(20 * time.Millisecond)
	}
	var gap time.Duration
	cb.OnCommentary = func(text messages.AssistantText, start bool) {
		commentary(text, start)
		begin := time.Now()
		time.Sleep(200 * time.Millisecond)
		gap = time.Since(begin)
	}
	response, err := agent.Run(context.Background(), &llm.CompletionRequest{Messages: []messages.ChatMessage{user}}, cb)
	if err != nil {
		t.Fatal(err)
	}
	if err := execution.settlePersistence(response, nil); err != nil {
		t.Fatal(err)
	}
	m.settleTurnActivity("done", false)
	live := m.reasoningRecords.get(m.reasoningOrder[0])
	history := testSessionHistory(t, session)
	reloaded := newReplModel()
	reloaded.hydrateHistory(history, "timing")
	restored := reloaded.reasoningRecords.get(reloaded.reasoningOrder[0])
	if live.elapsed < 40*time.Millisecond || restored.elapsed < 40*time.Millisecond {
		t.Fatalf("later interval omitted: live=%v restored=%v", live.elapsed, restored.elapsed)
	}
	difference := live.elapsed - restored.elapsed
	if difference < 0 {
		difference = -difference
	}
	if difference >= gap/2 {
		t.Fatalf("hidden commentary counted as reasoning: live=%v restored=%v gap=%v", live.elapsed, restored.elapsed, gap)
	}
	if string(live.tail) != "firstsecond" || string(live.tail) != string(restored.tail) || restored.elapsed != response.Message.ThinkingDuration() {
		t.Fatalf("live/restored reasoning mismatch: live=%+v restored=%+v message=%+v", live, restored, response.Message)
	}
}
