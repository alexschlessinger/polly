package llm

import (
	"context"
	"errors"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

// TestExecuteWithToolsAppendsAssistantOnce: a response carrying several tool
// calls must land in history as one assistant message followed by one result
// per call — not appended again for every call.
func TestExecuteWithToolsAppendsAssistantOnce(t *testing.T) {
	fake := &sequentialLLM{
		responses: []messages.ChatMessage{
			{
				Role: messages.MessageRoleAssistant,
				ToolCalls: []messages.ChatMessageToolCall{
					{ID: "tc1", Name: "noop", Arguments: "{}"},
					{ID: "tc2", Name: "noop", Arguments: "{}"},
				},
				StopReason: messages.StopReasonToolUse,
			},
			{
				Role:       messages.MessageRoleAssistant,
				Content:    "done",
				StopReason: messages.StopReasonEndTurn,
			},
		},
	}
	noop := &tools.Func{
		Name: "noop",
		Run:  func(context.Context, tools.Args) (string, error) { return "ok", nil },
	}
	registry := tools.NewToolRegistry([]tools.Tool{noop})

	builder := NewCompletionBuilder("fake/model").WithUserMessage("go")
	response, err := builder.ExecuteWithTools(context.Background(), fake, registry)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if response == nil || response.Content != "done" {
		t.Fatalf("response = %#v, want final content %q", response, "done")
	}

	var roles []string
	for _, m := range builder.req.Messages {
		roles = append(roles, m.Role)
	}
	want := []string{
		messages.MessageRoleUser,
		messages.MessageRoleAssistant,
		messages.MessageRoleTool,
		messages.MessageRoleTool,
	}
	if len(roles) != len(want) {
		t.Fatalf("history roles = %v, want %v", roles, want)
	}
	for i := range want {
		if roles[i] != want[i] {
			t.Fatalf("history roles = %v, want %v", roles, want)
		}
	}
	if builder.req.Messages[2].ToolCallID != "tc1" || builder.req.Messages[3].ToolCallID != "tc2" {
		t.Fatalf("tool results out of order: %#v", builder.req.Messages[2:])
	}
}

// TestExecuteWithToolsCapsRounds: a model that never stops calling tools must
// hit the round cap instead of looping forever.
func TestExecuteWithToolsCapsRounds(t *testing.T) {
	noop := &tools.Func{
		Name: "noop",
		Run:  func(context.Context, tools.Args) (string, error) { return "ok", nil },
	}
	registry := tools.NewToolRegistry([]tools.Tool{noop})
	fake := &alwaysToolUseLLM{}

	response, err := NewCompletionBuilder("fake/model").
		WithUserMessage("go").
		ExecuteWithTools(context.Background(), fake, registry)

	if !errors.Is(err, ErrMaxIterations) {
		t.Fatalf("err = %v, want ErrMaxIterations", err)
	}
	if response == nil || response.StopReason != messages.StopReasonMaxIterations {
		t.Fatalf("response = %#v, want stop reason %q", response, messages.StopReasonMaxIterations)
	}
	if fake.calls != 250 {
		t.Fatalf("LLM calls = %d, want 250", fake.calls)
	}
}

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
