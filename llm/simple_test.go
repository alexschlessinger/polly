package llm

import (
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
)

// TestSimpleProcessorEmptyCompletionCompletes: a stream that ends with no
// content and no tool calls (a refusal, a content-filter stop) must still
// yield a Complete event carrying the stop reason; only a stream with no
// messages at all stays silent.
func TestSimpleProcessorEmptyCompletionCompletes(t *testing.T) {
	msgChan := make(chan messages.ChatMessage, 1)
	msgChan <- messages.ChatMessage{
		Role:       messages.MessageRoleAssistant,
		StopReason: messages.StopReasonContentFilter,
	}
	close(msgChan)

	var events []*messages.StreamEvent
	for event := range (&SimpleProcessor{}).ProcessMessagesToEvents(msgChan) {
		events = append(events, event)
	}
	if len(events) != 1 || events[0].Type != messages.EventTypeComplete {
		t.Fatalf("events = %#v, want a single Complete", events)
	}
	if got := events[0].Message.StopReason; got != messages.StopReasonContentFilter {
		t.Fatalf("stop reason = %q, want %q", got, messages.StopReasonContentFilter)
	}

	empty := make(chan messages.ChatMessage)
	close(empty)
	for event := range (&SimpleProcessor{}).ProcessMessagesToEvents(empty) {
		t.Fatalf("empty stream produced event %#v", event)
	}
}
