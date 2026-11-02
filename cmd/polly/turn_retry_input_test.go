package main

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	ui "github.com/metaspartan/gotui/v5"
)

func TestCanceledUsageOnlyTurnReusesUnchangedComposerInput(t *testing.T) {
	for _, attached := range []bool{false, true} {
		name := "text"
		if attached {
			name = "attachment"
		}
		t.Run(name, func(t *testing.T) {
			calls := 0
			state, session := newInterruptedTurnState(t, turnRetryStream(func() []*messages.StreamEvent {
				calls++
				if calls == 1 {
					return []*messages.StreamEvent{
						{Type: messages.EventTypeUsage, InputTokens: 80, OutputTokens: 7},
						{Type: messages.EventTypeError, Error: context.Canceled},
					}
				}
				return []*messages.StreamEvent{{Type: messages.EventTypeComplete, Message: &messages.ChatMessage{
					Role: messages.MessageRoleAssistant, Content: "accepted", StopReason: messages.StopReasonEndTurn,
				}}}
			}), nil)
			defer state.agent.Close()
			r := newManagedREPL(&Config{}, "ctx", 0, 0)
			r.state, r.model.artifactStore = state, state.artifactStore
			prompt, path := "inspect this", ""
			if attached {
				path = filepath.Join(t.TempDir(), "retry.png")
				writeImageFixture(t, path, 8, 8)
				prompt += " " + r.model.registerAttachment(path, "retry.png")
			}
			r.model.ed.setText(prompt)
			r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})
			awaitReferencePreparation(t, r)
			prepared, ok := r.takePending()
			if !ok {
				t.Fatal("initial composer input was not accepted")
			}
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(context.Canceled)
			turnUI := &gotuiTurnUI{repl: r, model: r.model, config: r.config, state: state,
				turnID: r.model.turnID, turn: prepared, persistence: r.model.currentPersistence}
			execution := &turnExecution{ctx: ctx, config: r.config, state: state, settings: &state.settings, turnUI: turnUI, audit: newTurnAudit()}
			execution.userMsg = execution.audit.input(prepared.userMessage)
			history, _, err := execution.prepareRequest()
			if err != nil {
				t.Fatal(err)
			}
			req, err := execution.completionRequest(history)
			if err != nil {
				t.Fatal(err)
			}
			callbacks := execution.callbacks()
			observe := callbacks.OnUsageProgress
			callbacks.OnUsageProgress = func(usage llm.UsageUpdate) {
				observe(usage)
				cancel(errInteractiveTurnStopped)
			}
			response, err := state.agent.Run(ctx, req, callbacks)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("run error=%v", err)
			}
			execution.recordUsage(response)
			err = execution.settlePersistence(response, context.Cause(ctx))
			turnUI.CompleteTurn(turnCompletion{Err: err, ProgressSaved: turnProgressSaved(err)})
			r.endTurn(err)
			history = testSessionHistory(t, session)
			if len(history) != 3 || !history[1].IsUsageRecord() || history[1].GetInputTokens() != 80 || history[1].GetOutputTokens() != 7 {
				t.Fatalf("usage-only interruption was not preserved: %+v", history)
			}
			assertInterruptedMarker(t, history[2], context.Canceled.Error())
			firstAudit := savedTurnAudits(t, session)
			if len(firstAudit) != 1 || firstAudit[0].InterruptionOrigin != "interactive_stop" {
				t.Fatalf("cancellation audit=%+v", firstAudit)
			}
			if r.model.ed.text() != prompt {
				t.Fatalf("composer did not restore input: %q", r.model.ed.text())
			}
			if attached {
				mutateAttachmentSource(t, path, "delete")
			}
			r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})
			awaitReferencePreparation(t, r)
			retried, ok := r.takePending()
			if !ok || !r.model.restoreDraftNext || !reflect.DeepEqual(retried.userMessage, prepared.userMessage) {
				t.Fatalf("unchanged retry lost its payload or reuse flag: turn=%+v reuse=%v", retried, r.model.restoreDraftNext)
			}
			projected, err := prepareSessionImageRequest(history, retried.userMessage, true)
			if err != nil || len(projected) != len(history) {
				t.Fatalf("retry duplicated request input: messages=%d want=%d err=%v", len(projected), len(history), err)
			}
			if _, err := executeTurnWithUserMessage(context.Background(), r.config, state, retried.userMessage, nil, nil, &collectingTurnUI{}, true); err != nil {
				t.Fatal(err)
			}
			audits := savedTurnAudits(t, session)
			if len(audits) != 2 || !audits[1].ReusedInput || audits[1].InputTurnID != audits[0].ID {
				t.Fatalf("reused input audit linkage=%+v", audits)
			}
			after := testSessionHistory(t, session)
			if len(after) != 5 || !reflect.DeepEqual(after[:len(history)], history) || after[3].Role != messages.MessageRoleAssistant {
				t.Fatalf("retry duplicated input or changed accounting/audit suffix: %+v", after)
			}
		})
	}
}

func TestInputReuseStopsAtAcceptedProgress(t *testing.T) {
	user := messages.ChatMessage{Role: messages.MessageRoleUser, Content: "try this"}
	usage := turnUsageTestMessage(80, 7).UsageRecord()
	marker := interruptedTurnMarker(context.Canceled)
	for _, tc := range []struct {
		name     string
		progress []messages.ChatMessage
	}{
		{"assistant answer", []messages.ChatMessage{{Role: messages.MessageRoleAssistant, Content: "partial"}}},
		{"assistant reasoning", []messages.ChatMessage{{Role: messages.MessageRoleAssistant, Reasoning: "thinking"}}},
		{"tool exchange", []messages.ChatMessage{
			{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "call", Name: "probe"}}},
			{Role: messages.MessageRoleTool, ToolCallID: "call", Content: "done"},
		}},
		{"denied tool display", []messages.ChatMessage{{Role: messages.MessageRoleInternal, Metadata: map[string]any{
			messages.MetadataKeyTurnStatus: messages.TurnStatusToolDenied, messages.MetadataKeyDisplayToolCalls: "[]",
		}}}},
		{"interrupted display", []messages.ChatMessage{{Role: messages.MessageRoleInternal, Metadata: map[string]any{
			messages.MetadataKeyTurnStatus: messages.TurnStatusInterrupted, messages.MetadataKeyDisplayReasoning: "thinking",
		}}}},
		{"compaction", []messages.ChatMessage{(messages.Compaction{Summary: "prior work"}).Message()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			history := append([]messages.ChatMessage{user}, tc.progress...)
			history = append(history, usage, marker)
			if historyEndsWithEquivalentUserMessage(history, user) {
				t.Fatal("accepted progress was skipped to reuse earlier input")
			}
			projected, err := prepareSessionImageRequest(history, user, true)
			if err != nil || len(projected) != len(history)+1 {
				t.Fatalf("input after progress was not appended: %d messages, err=%v", len(projected), err)
			}
			session := newTurnPersistenceTestSession(t)
			if err := session.AddMessages(context.Background(), history); err != nil {
				t.Fatal(err)
			}
			if err := persistUserMessageForTurn(context.Background(), session, user, true); err != nil {
				t.Fatal(err)
			}
			if after := testSessionHistory(t, session); len(after) != len(history)+1 {
				t.Fatalf("new input after progress was not persisted: %+v", after)
			}
		})
	}
}
