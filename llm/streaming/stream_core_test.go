package streaming

import (
	"context"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
)

// noopAdapter implements ProviderAdapter with no-op methods
type noopAdapter struct{}

func (n *noopAdapter) ProcessChunk(chunk any, state StreamStateInterface) error { return nil }
func (n *noopAdapter) EnrichFinalMessage(msg *messages.ChatMessage, state StreamStateInterface) {}
func (n *noopAdapter) HandleToolCall(toolData any, state StreamStateInterface) error { return nil }

func newTestStreamingCore() (*StreamingCore, chan messages.ChatMessage) {
	ch := make(chan messages.ChatMessage, 10)
	sc := NewStreamingCore(context.Background(), ch, &noopAdapter{})
	return sc, ch
}

func TestHandleStructuredOutput_WithDataKey(t *testing.T) {
	sc, ch := newTestStreamingCore()
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
