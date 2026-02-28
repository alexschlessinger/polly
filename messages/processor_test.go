package messages

import (
	"errors"
	"testing"
)

func TestProcessMessagesToEvents_EmitsErrorEvent(t *testing.T) {
	p := NewStreamProcessor()
	msgChan := make(chan ChatMessage, 1)
	events := p.ProcessMessagesToEvents(msgChan)

	msg := ChatMessage{Role: MessageRoleAssistant, Content: "Error: boom"}
	msg.SetError(errors.New("boom"))
	msgChan <- msg
	close(msgChan)

	var got []*StreamEvent
	for ev := range events {
		got = append(got, ev)
	}

	if len(got) != 1 {
		t.Fatalf("expected exactly one event, got %d", len(got))
	}
	if got[0].Type != EventTypeError {
		t.Fatalf("expected EventTypeError, got %q", got[0].Type)
	}
	if got[0].Error == nil || got[0].Error.Error() != "boom" {
		t.Fatalf("expected error 'boom', got %v", got[0].Error)
	}
}

func TestProcessMessagesToEvents_EmitsCompleteForNormalStream(t *testing.T) {
	p := NewStreamProcessor()
	msgChan := make(chan ChatMessage, 1)
	events := p.ProcessMessagesToEvents(msgChan)

	msgChan <- ChatMessage{Role: MessageRoleAssistant, Content: "hello"}
	close(msgChan)

	var types []StreamEventType
	for ev := range events {
		types = append(types, ev.Type)
	}

	if len(types) != 2 {
		t.Fatalf("expected 2 events (content, complete), got %d", len(types))
	}
	if types[0] != EventTypeContent {
		t.Fatalf("expected first event to be content, got %q", types[0])
	}
	if types[1] != EventTypeComplete {
		t.Fatalf("expected second event to be complete, got %q", types[1])
	}
}
