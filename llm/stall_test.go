package llm

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
)

type nopAdapter struct{}

func (nopAdapter) ProcessChunk(any, streaming.StreamStateInterface) error                   { return nil }
func (nopAdapter) EnrichFinalMessage(*messages.ChatMessage, streaming.StreamStateInterface) {}
func (nopAdapter) HandleToolCall(any, streaming.StreamStateInterface) error                 { return nil }

func collectStreamEvents(t *testing.T, events <-chan *messages.StreamEvent, within time.Duration) []*messages.StreamEvent {
	t.Helper()
	var got []*messages.StreamEvent
	deadline := time.After(within)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return got
			}
			got = append(got, ev)
		case <-deadline:
			t.Fatalf("stream did not settle within %v; got %d events", within, len(got))
		}
	}
}

// firstStreamError returns the first error event's error. Stream errors cross
// the processor boundary as SetError metadata, which flattens them to their
// message — so the stall contract downstream is the message text, not the
// *streaming.StallError type (that type survives only as the context cause).
func firstStreamError(events []*messages.StreamEvent) error {
	for _, ev := range events {
		if ev.Type == messages.EventTypeError {
			return ev.Error
		}
	}
	return nil
}

func isStallStreamError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "stream stalled")
}

// A hung provider that unwinds silently on cancellation must still fail the
// stream with the stall error — otherwise the processor would fabricate a
// successful completion from the accumulated partial state at channel close.
func TestRunStreamStallCancelsSilentHungProvider(t *testing.T) {
	events := runStream(context.Background(), 40*time.Millisecond, 0, messages.NewStreamProcessor(), nopAdapter{}, func(ctx context.Context, core *streaming.StreamingCore) {
		core.EmitContent("partial ")
		<-ctx.Done() // hung read; no error emitted on the way out
	})
	got := collectStreamEvents(t, events, 5*time.Second)
	err := firstStreamError(got)
	if !isStallStreamError(err) || !strings.Contains(err.Error(), "40ms") {
		t.Fatalf("stream error = %v, want the 40ms stall error (events: %d)", err, len(got))
	}
	for _, ev := range got {
		if ev.Type == messages.EventTypeComplete {
			t.Fatalf("stalled stream fabricated a completion event: %+v", ev.Message)
		}
	}
}

// A provider that surfaces the aborted read as a wrapped context-cancellation
// error must report the stall cause, not the symptom.
func TestRunStreamStallReplacesCanceledReadError(t *testing.T) {
	events := runStream(context.Background(), 40*time.Millisecond, 0, messages.NewStreamProcessor(), nopAdapter{}, func(ctx context.Context, core *streaming.StreamingCore) {
		<-ctx.Done()
		core.EmitError(fmt.Errorf("provider: reading stream: %w", ctx.Err()))
	})
	got := collectStreamEvents(t, events, 5*time.Second)
	err := firstStreamError(got)
	if !isStallStreamError(err) {
		t.Fatalf("stream error = %v, want the stall cause instead of the cancellation symptom", err)
	}
	if strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("stall symptom leaked into the reported error: %v", err)
	}
}

// Steady chunks each land within the stall budget, so a generation much
// longer than the budget must still complete: the watchdog bounds silence,
// not total stream length.
func TestRunStreamStallResetsOnProviderData(t *testing.T) {
	events := runStream(context.Background(), 80*time.Millisecond, 0, messages.NewStreamProcessor(), nopAdapter{}, func(ctx context.Context, core *streaming.StreamingCore) {
		for range 5 {
			time.Sleep(30 * time.Millisecond)
			core.EmitContent("x")
		}
		core.SetStopReason(messages.StopReasonEndTurn)
		core.Complete()
	})
	got := collectStreamEvents(t, events, 5*time.Second)
	if err := firstStreamError(got); err != nil {
		t.Fatalf("healthy slow stream failed: %v", err)
	}
	var complete *messages.ChatMessage
	for _, ev := range got {
		if ev.Type == messages.EventTypeComplete {
			complete = ev.Message
		}
	}
	if complete == nil || complete.Content != "xxxxx" {
		t.Fatalf("completion = %+v, want the accumulated content", complete)
	}
}

// Zero disables the watchdog entirely.
func TestRunStreamZeroStallTimeoutDisablesWatchdog(t *testing.T) {
	events := runStream(context.Background(), 0, 0, messages.NewStreamProcessor(), nopAdapter{}, func(ctx context.Context, core *streaming.StreamingCore) {
		time.Sleep(60 * time.Millisecond) // longer silence than the budgets above
		core.EmitContent("ok")
		core.SetStopReason(messages.StopReasonEndTurn)
		core.Complete()
	})
	got := collectStreamEvents(t, events, 5*time.Second)
	if err := firstStreamError(got); err != nil {
		t.Fatalf("unbounded stream failed: %v", err)
	}
}

// The hard deadline caps total call duration even when data keeps arriving —
// the exact case the stall timer can never catch: an endpoint trickling
// keepalive chunks forever. A silent unwind must still surface the deadline
// error instead of a fabricated completion.
func TestRunStreamDeadlineCapsTricklingStream(t *testing.T) {
	events := runStream(context.Background(), 200*time.Millisecond, 60*time.Millisecond, messages.NewStreamProcessor(), nopAdapter{}, func(ctx context.Context, core *streaming.StreamingCore) {
		for {
			select {
			case <-ctx.Done():
				return // hung endpoint unwinds silently
			case <-time.After(10 * time.Millisecond):
				core.EmitContent("x") // steady trickle keeps resetting the stall timer
			}
		}
	})
	got := collectStreamEvents(t, events, 5*time.Second)
	err := firstStreamError(got)
	if err == nil || !strings.Contains(err.Error(), "deadline") || !strings.Contains(err.Error(), "60ms") {
		t.Fatalf("stream error = %v, want the 60ms deadline error (events: %d)", err, len(got))
	}
	for _, ev := range got {
		if ev.Type == messages.EventTypeComplete {
			t.Fatalf("deadline-capped stream fabricated a completion event: %+v", ev.Message)
		}
	}
}

// A provider that reports the deadline-aborted read as a wrapped cancellation
// must have the deadline cause surfaced, mirroring the stall translation.
func TestRunStreamDeadlineReplacesCanceledReadError(t *testing.T) {
	events := runStream(context.Background(), 0, 40*time.Millisecond, messages.NewStreamProcessor(), nopAdapter{}, func(ctx context.Context, core *streaming.StreamingCore) {
		<-ctx.Done()
		core.EmitError(fmt.Errorf("provider: reading stream: %w", ctx.Err()))
	})
	got := collectStreamEvents(t, events, 5*time.Second)
	err := firstStreamError(got)
	if err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("stream error = %v, want the deadline cause instead of the cancellation symptom", err)
	}
	if strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("deadline symptom leaked into the reported error: %v", err)
	}
}

// User cancellation is not a stall: the cause must never be rewritten into a
// StallError, so callers keep their cancellation semantics.
func TestRunStreamUserCancelIsNotAStall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	events := runStream(ctx, 10*time.Second, 0, messages.NewStreamProcessor(), nopAdapter{}, func(ctx context.Context, core *streaming.StreamingCore) {
		<-ctx.Done()
		core.EmitError(fmt.Errorf("provider: reading stream: %w", ctx.Err()))
	})
	cancel()
	got := collectStreamEvents(t, events, 5*time.Second)
	if err := firstStreamError(got); isStallStreamError(err) {
		t.Fatalf("user cancellation was misreported as a stall: %v", err)
	}
}
