package llm

import (
	"context"
	"testing"

	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
)

func newStructuredOutputCore() (*streaming.StreamingCore, chan messages.ChatMessage) {
	ch := make(chan messages.ChatMessage, 10)
	return streaming.NewStreamingCore(context.Background(), ch, nopAdapter{}), ch
}

func TestCompleteStructuredOutput_WithDataKey(t *testing.T) {
	sc, ch := newStructuredOutputCore()
	// Simulate a provider that surfaced the structured-output payload via a
	// tool_use stop, which is the realistic incoming state for Anthropic.
	sc.GetState().SetStopReason(messages.StopReasonToolUse)
	sc.GetState().AddToolCall(messages.ChatMessageToolCall{
		Name:      structuredOutputToolName,
		Arguments: `{"data": {"foo": "bar"}}`,
	})

	ok := completeStructuredOutput(sc)
	if !ok {
		t.Fatal("expected completeStructuredOutput to return true")
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

func TestCompleteStructuredOutput_NoDataKey(t *testing.T) {
	sc, _ := newStructuredOutputCore()
	sc.GetState().AddToolCall(messages.ChatMessageToolCall{
		Name:      structuredOutputToolName,
		Arguments: `{"other": 42}`,
	})

	ok := completeStructuredOutput(sc)
	if ok {
		t.Fatal("expected completeStructuredOutput to return false when no 'data' key")
	}
}

func TestCompleteStructuredOutput_NoMatchingTool(t *testing.T) {
	sc, _ := newStructuredOutputCore()
	sc.GetState().AddToolCall(messages.ChatMessageToolCall{
		Name:      "other_tool",
		Arguments: `{"data": {"x": 1}}`,
	})

	ok := completeStructuredOutput(sc)
	if ok {
		t.Fatal("expected completeStructuredOutput to return false for non-matching tool")
	}
}

func TestCompleteStructuredOutput_InvalidJSON(t *testing.T) {
	sc, _ := newStructuredOutputCore()
	sc.GetState().AddToolCall(messages.ChatMessageToolCall{
		Name:      structuredOutputToolName,
		Arguments: `not valid json{`,
	})

	ok := completeStructuredOutput(sc)
	if ok {
		t.Fatal("expected completeStructuredOutput to return false for invalid JSON")
	}
}

func TestCompleteStructuredOutput_EmptyToolCalls(t *testing.T) {
	sc, _ := newStructuredOutputCore()

	ok := completeStructuredOutput(sc)
	if ok {
		t.Fatal("expected completeStructuredOutput to return false with no tool calls")
	}
}
