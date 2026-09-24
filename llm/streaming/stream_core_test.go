package streaming

import (
	"context"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
)

// noopAdapter implements ProviderAdapter with no-op methods
type noopAdapter struct{}

func (n *noopAdapter) ProcessChunk(chunk any, state StreamStateInterface) error                 { return nil }
func (n *noopAdapter) EnrichFinalMessage(msg *messages.ChatMessage, state StreamStateInterface) {}

func newTestStreamingCore() (*StreamingCore, chan messages.ChatMessage) {
	ch := make(chan messages.ChatMessage, 10)
	sc := NewStreamingCore(context.Background(), ch, &noopAdapter{})
	return sc, ch
}

func TestActivityObserverIncludesNonTextDataAndPreservesWatchdog(t *testing.T) {
	observed, watchdog := 0, 0
	ctx, cancel := context.WithCancel(WithActivityObserver(context.Background(), func() { observed++ }))
	defer cancel()
	core := NewStreamingCore(ctx, make(chan messages.ChatMessage, 4), &noopAdapter{})
	core.SetActivityNotifier(func() { watchdog++ })
	if err := core.ProcessChunk("tool argument delta"); err != nil {
		t.Fatal(err)
	}
	core.EmitReasoning("thinking")
	core.EmitContent("answer")
	if observed != 3 || watchdog != 3 {
		t.Fatalf("activity observer=%d watchdog=%d", observed, watchdog)
	}
	cancel()
	if err := core.ProcessChunk("late data"); err != nil {
		t.Fatal(err)
	}
	if observed != 3 {
		t.Fatal("canceled stream still reports live activity")
	}
}

func TestCompleteStreamRefusesStreamWithoutStopReason(t *testing.T) {
	ch := make(chan messages.ChatMessage, 4)
	core := NewStreamingCore(context.Background(), ch, nil)
	core.state.AppendContent("partial")
	core.CompleteStream()
	close(ch)
	var got []messages.ChatMessage
	for msg := range ch {
		got = append(got, msg)
	}
	if len(got) != 1 || !got[0].IsError() {
		t.Fatalf("messages = %+v, want one error", got)
	}
	if got[0].GetError().Error() != ErrStreamEndedEarly.Error() {
		t.Fatalf("error = %v, want ErrStreamEndedEarly", got[0].GetError())
	}
}

func TestCompleteStreamCompletesWithStopReason(t *testing.T) {
	ch := make(chan messages.ChatMessage, 4)
	core := NewStreamingCore(context.Background(), ch, nil)
	core.SetStopReason(messages.StopReasonEndTurn)
	core.CompleteStream()
	close(ch)
	var got []messages.ChatMessage
	for msg := range ch {
		got = append(got, msg)
	}
	if len(got) != 1 || got[0].IsError() || got[0].StopReason != messages.StopReasonEndTurn {
		t.Fatalf("messages = %+v, want one completion", got)
	}
}

func TestCompleteDoesNotRepeatBufferedText(t *testing.T) {
	core, ch := newTestStreamingCore()
	core.EmitContent("first")
	core.EmitReasoning("thought")
	core.EmitContent(" second")
	core.SetStopReason(messages.StopReasonEndTurn)
	core.CompleteStream()
	close(ch)
	var content, reasoning string
	var final messages.ChatMessage
	for msg := range ch {
		content += msg.Content
		reasoning += msg.Reasoning
		final = msg
	}
	if content != "first second" || reasoning != "thought" {
		t.Fatalf("streamed text duplicated or lost: %q / %q", content, reasoning)
	}
	if final.Content != "" || final.Reasoning != "" || final.StopReason != messages.StopReasonEndTurn {
		t.Fatalf("final metadata message changed: %+v", final)
	}
}

// TestCompletePromotesToolCallsToToolUse: providers without a tool-use finish
// reason report a plain stop; the core reads a reply with calls as a tool
// turn, while a terminal reason survives.
func TestCompletePromotesToolCallsToToolUse(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop messages.StopReason
		want messages.StopReason
	}{
		{"end_turn", messages.StopReasonEndTurn, messages.StopReasonToolUse},
		{"max_tokens_survives", messages.StopReasonMaxTokens, messages.StopReasonMaxTokens},
		{"content_filter_survives", messages.StopReasonContentFilter, messages.StopReasonContentFilter},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, ch := newTestStreamingCore()
			core.state.AddToolCall(messages.ChatMessageToolCall{ID: "c", Name: "f", Arguments: "{}"})
			core.SetStopReason(tc.stop)
			core.Complete()
			if got := (<-ch).StopReason; got != tc.want {
				t.Fatalf("stop reason = %q, want %q", got, tc.want)
			}
		})
	}
	core, ch := newTestStreamingCore()
	core.SetStopReason(messages.StopReasonEndTurn)
	core.Complete()
	if got := (<-ch).StopReason; got != messages.StopReasonEndTurn {
		t.Fatalf("stop reason without calls = %q, want end_turn", got)
	}
}

func TestWatchdogErrorStopsWhenConsumerCancels(t *testing.T) {
	provider, cancelProvider := context.WithCancelCause(context.Background())
	cancelProvider(&StallError{})
	consumer, cancelConsumer := context.WithCancel(context.Background())
	defer cancelConsumer()
	core := NewStreamingCore(provider, make(chan messages.ChatMessage), nil)
	core.SetDeliveryContext(consumer)
	done := make(chan struct{})
	go func() { defer close(done); core.EmitError(context.Canceled) }()
	cancelConsumer()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("watchdog error blocked after consumer cancellation")
	}
}

// usageAdapter reports the usage each chunk carries.
type usageAdapter struct{}

func (usageAdapter) ProcessChunk(chunk any, state StreamStateInterface) error {
	usage := chunk.([2]int)
	state.SetTokenUsage(usage[0], usage[1])
	return nil
}
func (usageAdapter) EnrichFinalMessage(*messages.ChatMessage, StreamStateInterface) {}

func TestProcessChunkEmitsChangedUsage(t *testing.T) {
	ch := make(chan messages.ChatMessage, 10)
	core := NewStreamingCore(context.Background(), ch, usageAdapter{})
	for _, chunk := range [][2]int{{0, 0}, {120, 1}, {120, 1}, {120, 9}} {
		if err := core.ProcessChunk(chunk); err != nil {
			t.Fatalf("ProcessChunk: %v", err)
		}
	}
	core.SetReportedCost(0.0021)
	core.SetStopReason(messages.StopReasonEndTurn)
	core.Complete()
	close(ch)
	var got []messages.ChatMessage
	for msg := range ch {
		got = append(got, msg)
	}
	if len(got) != 3 {
		t.Fatalf("messages = %+v, want two usage updates and the completion", got)
	}
	for i, wantOut := range []int{1, 9} {
		msg := got[i]
		if msg.Content != "" || msg.Reasoning != "" || len(msg.ToolCalls) != 0 || msg.StopReason != "" {
			t.Fatalf("usage message %d carries more than usage: %+v", i, msg)
		}
		if msg.GetInputTokens() != 120 || msg.GetOutputTokens() != wantOut {
			t.Fatalf("usage message %d = %+v", i, msg.Metadata)
		}
	}
	if cost, ok := got[2].GetReportedCost(); !ok || cost != 0.0021 {
		t.Fatalf("completion cost = %v, %v", cost, ok)
	}
}
