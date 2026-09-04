package streaming

import (
	"context"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
)

// noopAdapter implements ProviderAdapter with no-op methods
type noopAdapter struct{}

func (n *noopAdapter) ProcessChunk(chunk any, state StreamStateInterface) error                 { return nil }
func (n *noopAdapter) EnrichFinalMessage(msg *messages.ChatMessage, state StreamStateInterface) {}
func (n *noopAdapter) HandleToolCall(toolData any, state StreamStateInterface) error            { return nil }

func newTestStreamingCore() (*StreamingCore, chan messages.ChatMessage) {
	ch := make(chan messages.ChatMessage, 10)
	sc := NewStreamingCore(context.Background(), ch, &noopAdapter{})
	return sc, ch
}

func TestHandleStructuredOutput_WithDataKey(t *testing.T) {
	sc, ch := newTestStreamingCore()
	// Simulate a provider that surfaced the structured-output payload via a
	// tool_use stop, which is the realistic incoming state for Anthropic.
	sc.state.SetStopReason(messages.StopReasonToolUse)
	sc.state.AddToolCall(messages.ChatMessageToolCall{
		Name:      "structured_output",
		Arguments: `{"data": {"foo": "bar"}}`,
	})

	ok := sc.HandleStructuredOutput("structured_output")
	if !ok {
		t.Fatal("expected HandleStructuredOutput to return true")
	}

	msg := <-ch
	if msg.Content != `{"foo":"bar"}` {
		t.Errorf("expected content %q, got %q", `{"foo":"bar"}`, msg.Content)
	}
	// Once the structured payload is extracted, the turn is logically over.
	// The agent loop relies on EndTurn here to avoid issuing a follow-up call
	// against a transcript whose last entry is this synthetic assistant msg.
	if msg.StopReason != messages.StopReasonEndTurn {
		t.Errorf("expected stop reason EndTurn, got %v", msg.StopReason)
	}
}

func TestHandleStructuredOutput_NoDataKey(t *testing.T) {
	sc, _ := newTestStreamingCore()
	sc.state.AddToolCall(messages.ChatMessageToolCall{
		Name:      "structured_output",
		Arguments: `{"other": 42}`,
	})

	ok := sc.HandleStructuredOutput("structured_output")
	if ok {
		t.Fatal("expected HandleStructuredOutput to return false when no 'data' key")
	}
}

func TestHandleStructuredOutput_NoMatchingTool(t *testing.T) {
	sc, _ := newTestStreamingCore()
	sc.state.AddToolCall(messages.ChatMessageToolCall{
		Name:      "other_tool",
		Arguments: `{"data": {"x": 1}}`,
	})

	ok := sc.HandleStructuredOutput("structured_output")
	if ok {
		t.Fatal("expected HandleStructuredOutput to return false for non-matching tool")
	}
}

func TestHandleStructuredOutput_InvalidJSON(t *testing.T) {
	sc, _ := newTestStreamingCore()
	sc.state.AddToolCall(messages.ChatMessageToolCall{
		Name:      "structured_output",
		Arguments: `not valid json{`,
	})

	ok := sc.HandleStructuredOutput("structured_output")
	if ok {
		t.Fatal("expected HandleStructuredOutput to return false for invalid JSON")
	}
}

func TestHandleStructuredOutput_EmptyToolCalls(t *testing.T) {
	sc, _ := newTestStreamingCore()

	ok := sc.HandleStructuredOutput("structured_output")
	if ok {
		t.Fatal("expected HandleStructuredOutput to return false with no tool calls")
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
