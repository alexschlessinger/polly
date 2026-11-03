package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/swarm"
	"github.com/alexschlessinger/pollytool/tools"
)

type compactionRecoveryStream func(*llm.CompletionRequest) []*messages.StreamEvent

func (f compactionRecoveryStream) ChatCompletionStream(_ context.Context, req *llm.CompletionRequest, _ llm.EventStreamProcessor) <-chan *messages.StreamEvent {
	events := f(req)
	ch := make(chan *messages.StreamEvent, len(events))
	for _, event := range events {
		ch <- event
	}
	close(ch)
	return ch
}

// Exercise the production compaction, retry, checkpoint, and turn-persistence
// paths together. A reopen reconstructs every runtime object from SQLite, not
// from the old agent's in-memory transcript or artifact cache.
func TestCompactedSessionRetrySurvivesReopen(t *testing.T) {
	for _, pause := range []bool{false, true} {
		name := "automatic_retry_recovers"
		if pause {
			name = "retry_pause_then_continue"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			const (
				constraint = "Do not change the public API."
				summary    = "COMPACTED-PLAN: " + constraint + " Fix the layout in the attached screenshot."
				receipt    = "SIDE-EFFECT-RECEIPT"
				discarded  = "DISCARDED-PARTIAL"
				toolID     = "accepted-work"
				failedID   = "must-not-run"
				answer     = "Layout fixed without changing the public API."
			)
			root := t.TempDir()
			dbPath, effectPath := filepath.Join(root, "session.db"), filepath.Join(root, "effects.txt")
			image := portablePNGBase64Size(t, 400)
			config := &Config{RetryPolicy: &llm.RetryPolicy{
				MaxRetries: 1, MaxElapsed: time.Minute, InitialDelay: time.Microsecond, MaxDelay: time.Microsecond,
			}}
			failures, firstAnswers := 1, 1
			if pause {
				failures, firstAnswers = 2, 0
			}
			summaries, calls, failed, accepted := 0, 0, 0, 0
			var state *conversationState

			// Give each kind of model call a distinct, exactly representable
			// charge. Failed attempts report usage but must retain no content.
			complete := func(msg messages.ChatMessage, in, out int, cost float64) []*messages.StreamEvent {
				msg.SetTokenUsage(in, out)
				msg.SetReportedCost(cost)
				return []*messages.StreamEvent{
					{Type: messages.EventTypeContent, Content: msg.Content},
					{Type: messages.EventTypeUsage, InputTokens: in, OutputTokens: out, CostUSD: cost, CostReported: true},
					{Type: messages.EventTypeComplete, Message: &msg},
				}
			}
			model := compactionRecoveryStream(func(req *llm.CompletionRequest) []*messages.StreamEvent {
				calls++
				assertRecoveryTranscript(t, req.Messages, discarded, failedID)
				for _, msg := range req.Messages {
					if msg.Role == messages.MessageRoleInternal {
						t.Fatal("internal compaction, usage, or recovery record reached the model")
					}
				}
				last := req.Messages[len(req.Messages)-1]
				if strings.Contains(last.Content, "Summarize the conversation above") {
					summaries++
					if summaries != 1 || !carries(req.Messages, constraint) {
						t.Fatalf("summary request %d lost the original constraint", summaries)
					}
					return complete(messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: summary, StopReason: messages.StopReasonEndTurn}, 101, 11, 1)
				}
				if summaries != 1 || !carries(req.Messages, summary) {
					t.Fatal("continuation did not carry the generated summary")
				}
				images := 0
				for _, msg := range req.Messages {
					for _, part := range msg.Parts {
						if part.Type == "image_base64" {
							images++
							if part.ImageData != image || part.MimeType != "image/png" {
								t.Fatal("referenced image bytes or MIME type changed")
							}
						}
					}
				}
				if images != 1 {
					t.Fatalf("request has %d hydrated images, want 1", images)
				}
				if calls == 2 {
					return complete(messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse,
						ToolCalls: []messages.ChatMessageToolCall{{ID: toolID, Name: "record_work", Arguments: `{}`}},
					}, 202, 22, 2)
				}
				// Inspect both the outgoing request and the real store before
				// failing: the tool receipt must already be checkpointed.
				for _, history := range [][]messages.ChatMessage{req.Messages, testSessionHistory(t, state.session)} {
					results := 0
					for _, msg := range history {
						if msg.Role == messages.MessageRoleTool && msg.ToolCallID == toolID && msg.Content == receipt {
							results++
						}
					}
					if results != 1 {
						t.Fatalf("completed tool receipts = %d, want exactly 1", results)
					}
				}
				if failed < failures {
					failed++
					return []*messages.StreamEvent{
						{Type: messages.EventTypeContent, Content: discarded},
						{Type: messages.EventTypeUsage, InputTokens: 303, OutputTokens: 33, CostUSD: 3, CostReported: true},
						// Even a provisional completion containing an unfinished
						// tool call must be discarded when its stream fails.
						{Type: messages.EventTypeComplete, Message: &messages.ChatMessage{
							Role: messages.MessageRoleAssistant, Content: discarded, StopReason: messages.StopReasonToolUse,
							ToolCalls: []messages.ChatMessageToolCall{{ID: failedID, Name: "record_work", Arguments: `{"unfinished":`}},
						}},
						{Type: messages.EventTypeError, Error: io.ErrUnexpectedEOF},
					}
				}
				accepted++
				return complete(messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: answer, StopReason: messages.StopReasonEndTurn}, 404, 44, 4)
			})
			state = openCompactionRecoveryState(t, dbPath, effectPath, receipt, model)
			attachment, err := externalizeMessageImages(ctx, messages.ChatMessage{
				Role: messages.MessageRoleUser, Content: constraint,
				Parts: []messages.ContentPart{
					{Type: "text", Text: constraint},
					{Type: "image_base64", ImageData: image, MimeType: "image/png", FileName: "layout.png", Reference: "[image #1]"},
				},
			}, state.artifactStore)
			if err != nil {
				t.Fatal(err)
			}
			testAddMessages(t, state.session, []messages.ChatMessage{
				attachment,
				{Role: messages.MessageRoleAssistant, Content: strings.Repeat("Earlier layout discussion. ", 2_000)},
			})
			original := testSessionHistory(t, state.session)
			if len(original[0].Parts) != 2 {
				t.Fatalf("saved attachment parts = %+v", original[0].Parts)
			}
			part := original[0].Parts[1]
			if part.Type != "image_artifact" || part.Artifact == nil || part.Artifact.ID == "" || part.ImageData != "" {
				t.Fatalf("fixture did not persist an artifact-backed image: %+v", part)
			}
			run := func(prompt string) (int, error) {
				// Settled line output is replay-capable; raw streaming is not.
				ui := &lineTurnUI{config: config, writer: io.Discard, errWriter: io.Discard}
				return executeTurnWithUserMessage(ctx, config, state, messages.ChatMessage{
					Role: messages.MessageRoleUser, Content: prompt,
				}, nil, nil, ui, false)
			}
			prompt := "Fix the layout in [image #1]."
			code, err := run(prompt)
			var savedPause *swarm.RetryPauseError
			if pause {
				if code != 1 || !errors.As(err, &savedPause) || !turnProgressSaved(err) || !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("expected a confirmed saved retry pause: code=%d err=%v", code, err)
				}
				if savedPause.Pause.Attempts != 2 || savedPause.Pause.RetryCount != 1 || savedPause.Pause.NextRetryAt.IsZero() {
					t.Fatalf("wrong retry receipt: %+v", savedPause.Pause)
				}
			} else if code != 0 || err != nil {
				t.Fatalf("automatic recovery: code=%d err=%v", code, err)
			}
			if summaries != 1 || failed != failures || calls != 4 || accepted != firstAnswers {
				t.Fatalf("fixture missed compaction or retries: summaries=%d failed=%d calls=%d answers=%d", summaries, failed, calls, accepted)
			}
			history := testSessionHistory(t, state.session)
			assertCompactionRecoveryHistory(t, history, original, summary, answer, discarded, failedID, failures, firstAnswers, pause)
			if got, want := state.spend.total(), (turnCost{usd: float64(3 + 3*failures + 4*accepted), known: true}); got != want {
				t.Fatalf("live cost=%+v want %+v", got, want)
			}
			if err := state.Close(); err != nil {
				t.Fatal(err)
			}
			if err := state.sessionStore.Close(); err != nil {
				t.Fatal(err)
			}

			state = openCompactionRecoveryState(t, dbPath, effectPath, receipt, model)
			reloaded := testSessionHistory(t, state.session)
			if !reflect.DeepEqual(reloaded, history) {
				t.Fatal("reopen changed saved history")
			}
			if calls != 4 {
				t.Fatalf("reopen automatically called the model: calls=%d", calls)
			}
			if pause {
				status := state.swarm.ParentState(nil)
				got, want := status.RetryPause, savedPause.Pause
				if status.Busy || got.Reason != want.Reason || got.Attempts != want.Attempts || got.RetryCount != want.RetryCount || !got.NextRetryAt.Equal(want.NextRetryAt) {
					t.Fatalf("reopen lost the saved pause: %+v, want %+v", status, want)
				}
			}
			if code, err := run("Continue using [image #1]."); code != 0 || err != nil {
				t.Fatalf("explicit continuation: code=%d err=%v", code, err)
			}
			if calls != 5 || summaries != 1 || accepted != firstAnswers+1 {
				t.Fatalf("continuation replayed work or summarized again: calls=%d summaries=%d answers=%d", calls, summaries, accepted)
			}
			history = testSessionHistory(t, state.session)
			assertCompactionRecoveryHistory(t, history, original, summary, answer, discarded, failedID, failures, firstAnswers+1, pause)
			prompts := 0
			for _, msg := range history {
				if msg.Role == messages.MessageRoleUser && msg.Content == prompt {
					prompts++
				}
			}
			if prompts != 1 {
				t.Fatalf("saved original prompt %d times, want 1", prompts)
			}
			if got := state.spend.total(); got != (turnCost{usd: 4, known: true}) {
				t.Fatalf("reopened process charged historical usage again: %+v", got)
			}
			coordination, err := state.swarm.State(ctx)
			if err != nil || coordination.ParentTurns[state.swarm.ID] != nil || state.swarm.ParentState(nil).Busy {
				t.Fatalf("continuation left a pending parent turn: state=%+v err=%v", coordination, err)
			}
			data, err := os.ReadFile(effectPath)
			if err != nil || string(data) != receipt+"\n" {
				t.Fatalf("tool side effect was lost or repeated: %q err=%v", data, err)
			}
		})
	}
}

func openCompactionRecoveryState(t *testing.T, dbPath, effectPath, receipt string, model llm.LLM) *conversationState {
	t.Helper()
	store := testOpenDiskStore(t, dbPath, nil)
	session := testAcquireSession(t, store, "compaction-recovery")
	registry := tools.NewToolRegistry([]tools.Tool{&tools.Func{Name: "record_work", Run: func(context.Context, tools.Args) (string, error) {
		file, err := os.OpenFile(effectPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return "", err
		}
		_, writeErr := fmt.Fprintln(file, receipt)
		return receipt, errors.Join(writeErr, file.Close())
	}}})
	state := &conversationState{
		sessionStore: store, session: session, artifactStore: session.ArtifactStore(), toolRegistry: registry,
		settings: Settings{Model: "test/model", SystemPrompt: "You are a test assistant.", MaxTokens: 512, MaxHistoryTokens: 8_000, MaxIterations: 8},
	}
	t.Cleanup(func() { _ = state.Close() })
	state.agent = llm.NewAgent(model, registry, llm.AgentConfig{MaxIterations: 8, ArtifactStore: state.artifactStore})
	var err error
	state.swarm, err = swarm.New(swarm.Config{
		Store: store, Parent: session, Registry: registry, Client: model, OpenTools: tools.NativeOpenTools(registry),
		Root: filepath.Dir(dbPath), Directory: filepath.Join(filepath.Dir(dbPath), "workspaces"), DurableMessages: durableTurnMessages,
	})
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func assertRecoveryTranscript(t *testing.T, history []messages.ChatMessage, discarded, failedID string) {
	t.Helper()
	if carries(history, discarded) {
		t.Fatal("failed partial response entered accepted history")
	}
	calls, results := map[string]int{}, map[string]int{}
	for _, msg := range history {
		for _, call := range msg.ToolCalls {
			if call.ID == failedID || strings.Contains(call.Arguments, "unfinished") {
				t.Fatal("failed tool arguments entered accepted history")
			}
			calls[call.ID]++
			if calls[call.ID] != 1 {
				t.Fatalf("duplicate tool call %q", call.ID)
			}
		}
		if msg.ToolCallID == failedID {
			t.Fatal("failed tool call produced a result")
		}
		if msg.Role == messages.MessageRoleTool {
			results[msg.ToolCallID]++
		}
	}
	if !reflect.DeepEqual(calls, results) {
		t.Fatalf("unpaired tool calls/results: calls=%v results=%v", calls, results)
	}
}

func assertCompactionRecoveryHistory(t *testing.T, history, original []messages.ChatMessage, summary, answer, discarded, failedID string, failures, answers int, paused bool) {
	t.Helper()
	assertRecoveryTranscript(t, history, discarded, failedID)
	if len(history) < len(original) || !reflect.DeepEqual(history[:len(original)], original) {
		t.Fatal("compaction changed original history or its attachment")
	}
	compactions, pauses, results, savedAnswers := 0, 0, 0, 0
	usage := map[float64]int{}
	for _, msg := range history {
		if msg.Role == messages.MessageRoleAssistant && msg.Content == answer && len(msg.ToolCalls) == 0 && msg.StopReason == messages.StopReasonEndTurn {
			savedAnswers++
		}
		if c, ok := msg.Compaction(); ok {
			compactions++
			if c.Summary != summary {
				t.Fatalf("wrong persisted compaction: %+v", c)
			}
		}
		if msg.Metadata[retryPauseMetadataKey] == true {
			pauses++
		}
		if msg.Role == messages.MessageRoleTool {
			results++
		}
		if billed, ok := msg.GetReportedCost(); ok {
			usage[billed]++
			if (billed == 1 || billed == 3) && (!msg.IsUsageRecord() || msg.Content != "" || len(msg.ToolCalls) != 0) {
				t.Fatalf("summary/failed usage retained response content: %+v", msg)
			}
		}
	}
	wantPauses := 0
	if paused {
		wantPauses = 1
	}
	if compactions != 1 || pauses != wantPauses || results != 1 || savedAnswers != answers {
		t.Fatalf("saved compactions=%d pauses=%d results=%d answers=%d, want 1/%d/1/%d", compactions, pauses, results, savedAnswers, wantPauses, answers)
	}
	totals := (&llm.AgentResponse{AllMessages: history}).TokenUsage()
	if usage[1] != 1 || usage[2] != 1 || usage[3] != failures || usage[4] != answers ||
		totals.TotalInput != 303+303*failures+404*answers || totals.TotalOutput != 33+33*failures+44*answers || totals.ReportedCostUSD != float64(3+3*failures+4*answers) {
		t.Fatalf("usage lost or doubled: records=%v totals=%+v (failures=%d answers=%d)", usage, totals, failures, answers)
	}
}
