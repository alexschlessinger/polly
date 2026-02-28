package messages

import (
	"encoding/json"
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

func TestProcessMessagesToEvents_ReasoningChunk(t *testing.T) {
	p := NewStreamProcessor()
	msgChan := make(chan ChatMessage, 1)
	events := p.ProcessMessagesToEvents(msgChan)

	msgChan <- ChatMessage{Role: MessageRoleAssistant, Reasoning: "let me think"}
	close(msgChan)

	var types []StreamEventType
	var reasoningContent string
	for ev := range events {
		types = append(types, ev.Type)
		if ev.Type == EventTypeReasoning {
			reasoningContent = ev.Content
		}
	}

	if len(types) < 2 {
		t.Fatalf("expected at least 2 events, got %d", len(types))
	}
	if types[0] != EventTypeReasoning {
		t.Fatalf("expected first event to be reasoning, got %q", types[0])
	}
	if reasoningContent != "let me think" {
		t.Errorf("reasoning content = %q, want %q", reasoningContent, "let me think")
	}
}

func TestProcessMessagesToEvents_ToolCall(t *testing.T) {
	p := NewStreamProcessor()
	msgChan := make(chan ChatMessage, 1)
	events := p.ProcessMessagesToEvents(msgChan)

	msgChan <- ChatMessage{
		Role: MessageRoleAssistant,
		ToolCalls: []ChatMessageToolCall{
			{ID: "tc-1", Name: "search", Arguments: `{"query":"hello"}`},
		},
	}
	close(msgChan)

	var foundToolCall bool
	for ev := range events {
		if ev.Type == EventTypeToolCall {
			foundToolCall = true
			if ev.ToolCall.Name != "search" {
				t.Errorf("tool call name = %q, want %q", ev.ToolCall.Name, "search")
			}
			if ev.ToolCall.Args["query"] != "hello" {
				t.Errorf("tool call args[query] = %v, want %q", ev.ToolCall.Args["query"], "hello")
			}
		}
	}

	if !foundToolCall {
		t.Fatal("expected EventTypeToolCall to be emitted")
	}
}

func TestProcessMessagesToEvents_ToolCallInvalidJSON(t *testing.T) {
	p := NewStreamProcessor()
	msgChan := make(chan ChatMessage, 1)
	events := p.ProcessMessagesToEvents(msgChan)

	msgChan <- ChatMessage{
		Role: MessageRoleAssistant,
		ToolCalls: []ChatMessageToolCall{
			{ID: "tc-1", Name: "search", Arguments: `not valid json`},
		},
	}
	close(msgChan)

	var foundToolCall bool
	var foundComplete bool
	for ev := range events {
		if ev.Type == EventTypeToolCall {
			foundToolCall = true
		}
		if ev.Type == EventTypeComplete {
			foundComplete = true
		}
	}

	if foundToolCall {
		t.Fatal("should not emit EventTypeToolCall for invalid JSON args")
	}
	if !foundComplete {
		t.Fatal("should still emit EventTypeComplete even with invalid tool call JSON")
	}
	// Verify the invalid JSON truly can't parse
	var args map[string]any
	if err := json.Unmarshal([]byte("not valid json"), &args); err == nil {
		t.Fatal("test setup error: expected JSON parse failure")
	}
}

func TestProcessMessagesToEvents_StopReasonForwarded(t *testing.T) {
	p := NewStreamProcessor()
	msgChan := make(chan ChatMessage, 2)
	events := p.ProcessMessagesToEvents(msgChan)

	msgChan <- ChatMessage{Role: MessageRoleAssistant, Content: "hi"}
	msgChan <- ChatMessage{Role: MessageRoleAssistant, StopReason: StopReasonMaxTokens}
	close(msgChan)

	var completeEvent *StreamEvent
	for ev := range events {
		if ev.Type == EventTypeComplete {
			completeEvent = ev
		}
	}

	if completeEvent == nil {
		t.Fatal("expected EventTypeComplete")
	}
	if completeEvent.Message.StopReason != StopReasonMaxTokens {
		t.Errorf("stop reason = %q, want %q", completeEvent.Message.StopReason, StopReasonMaxTokens)
	}
}
