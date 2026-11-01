package llm

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

func fastRetryPolicy(n int) *RetryPolicy {
	return &RetryPolicy{MaxRetries: n, MaxElapsed: time.Second, InitialDelay: time.Microsecond, MaxDelay: time.Microsecond}
}

type retryStreamFunc func(context.Context, *CompletionRequest) <-chan *messages.StreamEvent

func (f retryStreamFunc) ChatCompletionStream(ctx context.Context, req *CompletionRequest, _ EventStreamProcessor) <-chan *messages.StreamEvent {
	return f(ctx, req)
}

func retryEvents(events ...*messages.StreamEvent) <-chan *messages.StreamEvent {
	ch := make(chan *messages.StreamEvent, len(events))
	for _, event := range events {
		ch <- event
	}
	close(ch)
	return ch
}

func retryAnswer() *messages.StreamEvent {
	return &messages.StreamEvent{Type: messages.EventTypeComplete, Message: &messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "accepted", StopReason: messages.StopReasonEndTurn}}
}

func TestAgentRetryAfterDisplayedOutputRequiresExplicitReset(t *testing.T) {
	for _, kind := range []messages.StreamEventType{messages.EventTypeContent, messages.EventTypeReasoning, messages.EventTypeCommentary} {
		for _, reset := range []bool{false, true} {
			t.Run(string(kind)+map[bool]string{false: "/legacy", true: "/reset"}[reset], func(t *testing.T) {
				calls, aborted := 0, 0
				var order []string
				model := retryStreamFunc(func(_ context.Context, req *CompletionRequest) <-chan *messages.StreamEvent {
					calls++
					for _, m := range req.Messages {
						if strings.Contains(m.Content, "discard") {
							t.Fatal("failed partial response entered model history")
						}
					}
					if calls == 1 {
						return retryEvents(&messages.StreamEvent{Type: kind, Content: "discard", Text: &messages.AssistantText{Text: "discard"}, TextStart: true}, &messages.StreamEvent{Type: messages.EventTypeError, Error: io.ErrUnexpectedEOF})
					}
					order = append(order, "replacement")
					return retryEvents(retryAnswer())
				})
				agent := NewAgent(model, nil, AgentConfig{})
				defer agent.Close()
				cb := &AgentCallbacks{OnContent: func(string) {}, OnReasoning: func(string) {}, OnCommentary: func(messages.AssistantText, bool) {}, OnRetry: func(RetryEvent) {}}
				if reset {
					cb.OnAttemptAborted = func(iteration, attempt int, err error) {
						if iteration != 0 || attempt != 0 || !errors.Is(err, io.ErrUnexpectedEOF) {
							t.Fatalf("abort = %d/%d %v", iteration, attempt, err)
						}
						aborted++
						order = append(order, "abort")
					}
				}
				resp, err := agent.Run(context.Background(), &CompletionRequest{RetryPolicy: fastRetryPolicy(2)}, cb)
				if !reset {
					if !errors.Is(err, io.ErrUnexpectedEOF) || calls != 1 || aborted != 0 {
						t.Fatalf("legacy observer authorized replay: calls=%d aborted=%d err=%v", calls, aborted, err)
					}
					return
				}
				if err != nil || calls != 2 || aborted != 1 || !reflect.DeepEqual(order, []string{"abort", "replacement"}) || len(resp.AllMessages) != 1 || resp.Message.Content != "accepted" {
					t.Fatalf("recovery: calls=%d aborted=%d order=%v response=%+v err=%v", calls, aborted, order, resp, err)
				}
			})
		}
	}
}

func TestAgentRetryPreservesCompletedToolsAndFailedUsageOnly(t *testing.T) {
	calls, executed, checkpoints, iterationUsage := 0, 0, 0, 0
	toolReply := func(id string) *messages.StreamEvent {
		return &messages.StreamEvent{Type: messages.EventTypeComplete, Message: &messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: id, Name: "work", Arguments: "{}"}}}}
	}
	model := retryStreamFunc(func(_ context.Context, req *CompletionRequest) <-chan *messages.StreamEvent {
		calls++
		switch calls {
		case 1:
			return retryEvents(toolReply("accepted-tool"))
		case 2:
			return retryEvents(
				&messages.StreamEvent{Type: messages.EventTypeContent, Content: "discard"},
				&messages.StreamEvent{Type: messages.EventTypeUsage, InputTokens: 100, OutputTokens: 8, CacheReadTokens: 40, CostUSD: 0.25, CostReported: true},
				toolReply("must-not-run"),
				&messages.StreamEvent{Type: messages.EventTypeError, Error: io.ErrUnexpectedEOF},
			)
		default:
			var results int
			for _, m := range req.Messages {
				if m.Role == messages.MessageRoleTool {
					results++
				}
				if m.Content == "discard" || len(m.ToolCalls) > 0 && m.ToolCalls[0].ID == "must-not-run" {
					t.Fatal("failed attempt entered request history")
				}
			}
			if results != 1 {
				t.Fatalf("completed tool receipts = %d, want 1", results)
			}
			return retryEvents(retryAnswer())
		}
	})
	registry := tools.NewToolRegistry([]tools.Tool{&tools.Func{Name: "work", Run: func(context.Context, tools.Args) (string, error) { executed++; return "saved result", nil }}})
	defer registry.Close()
	agent := NewAgent(model, registry, AgentConfig{})
	defer agent.Close()
	resp, err := agent.Run(context.Background(), &CompletionRequest{RetryPolicy: fastRetryPolicy(2)}, &AgentCallbacks{
		OnContent: func(string) {}, OnAttemptAborted: func(int, int, error) {},
		Checkpoint: func(_ context.Context, cp AgentCheckpoint) error {
			if cp.Request {
				checkpoints++
			}
			return nil
		},
		OnIterationUsage: func(int, int, int) { iterationUsage++ },
	})
	if err != nil || calls != 3 || executed != 1 || checkpoints != 2 || iterationUsage != 2 {
		t.Fatalf("calls=%d tools=%d checkpoints=%d usage=%d err=%v", calls, executed, checkpoints, iterationUsage, err)
	}
	usage := resp.TokenUsage()
	if usage.TotalInput != 100 || usage.TotalOutput != 8 || usage.CacheRead != 40 || usage.ReportedCostUSD != 0.25 {
		t.Fatalf("failed attempt spend lost or duplicated: %+v", usage)
	}
	var records int
	for _, m := range resp.AllMessages {
		if m.IsUsageRecord() {
			records++
			if m.Content != "" || len(m.ToolCalls) != 0 {
				t.Fatalf("usage leaked response: %+v", m)
			}
		}
	}
	if records != 1 {
		t.Fatalf("usage records = %d", records)
	}
}

func TestAgentRetryExhaustionAndStallClassification(t *testing.T) {
	for _, tc := range []struct {
		name               string
		failure            error
		retries, wantCalls int
		exhausted          bool
	}{
		{"stall recovers within budget", &streaming.StallError{Timeout: time.Second}, 2, 3, true},
		{"hard deadline stays terminal", &streaming.DeadlineError{Deadline: time.Second}, 2, 1, false},
		{"retries disabled", io.ErrUnexpectedEOF, 0, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := &flakyStreamLLM{failures: 20, err: tc.failure}
			agent := NewAgent(model, nil, AgentConfig{})
			defer agent.Close()
			var final error
			_, err := agent.Run(context.Background(), &CompletionRequest{RetryPolicy: fastRetryPolicy(tc.retries)}, &AgentCallbacks{Checkpoint: func(_ context.Context, cp AgentCheckpoint) error {
				if cp.Final {
					final = cp.Err
				}
				return nil
			}})
			var exhausted *RetryExhaustedError
			if !errors.Is(err, tc.failure) || errors.As(err, &exhausted) != tc.exhausted || model.calls != tc.wantCalls || !errors.Is(final, tc.failure) {
				t.Fatalf("calls=%d err=%v checkpoint=%v exhausted=%+v", model.calls, err, final, exhausted)
			}
			if tc.exhausted && (exhausted.Attempts != tc.wantCalls || exhausted.NextRetryAt.IsZero()) {
				t.Fatalf("missing recovery receipt: %+v", exhausted)
			}
		})
	}
}

func TestAgentRetryCancelsFailedProviderBeforeReplacement(t *testing.T) {
	calls := 0
	var previous context.Context
	drained := make(chan struct{})
	model := retryStreamFunc(func(ctx context.Context, _ *CompletionRequest) <-chan *messages.StreamEvent {
		calls++
		if calls > 1 {
			if previous.Err() == nil {
				t.Fatal("previous provider still running")
			}
			return retryEvents(retryAnswer())
		}
		previous = ctx
		ch := make(chan *messages.StreamEvent)
		go func() {
			defer close(ch)
			ch <- &messages.StreamEvent{Type: messages.EventTypeError, Error: io.ErrUnexpectedEOF}
			<-ctx.Done()
			ch <- &messages.StreamEvent{Type: messages.EventTypeContent, Content: "stale"}
			close(drained)
		}()
		return ch
	})
	agent := NewAgent(model, nil, AgentConfig{})
	defer agent.Close()
	_, err := agent.Run(context.Background(), &CompletionRequest{RetryPolicy: fastRetryPolicy(2)}, &AgentCallbacks{OnContent: func(s string) {
		if s == "stale" {
			t.Error("abandoned output forwarded")
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-drained:
	case <-time.After(time.Second):
		t.Fatal("failed producer not drained")
	}
}
