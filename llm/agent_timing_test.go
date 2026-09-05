package llm

import (
	"context"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

// TestProcessEventsRecordsThinkingDuration: the clock runs from the first
// reasoning delta to the first content delta, so a resumed transcript can show
// how long the model thought.
func TestProcessEventsRecordsThinkingDuration(t *testing.T) {
	const thought = 20 * time.Millisecond
	events := make(chan *messages.StreamEvent)
	go func() {
		defer close(events)
		events <- &messages.StreamEvent{Type: messages.EventTypeReasoning, Content: "let me"}
		time.Sleep(thought)
		events <- &messages.StreamEvent{Type: messages.EventTypeReasoning, Content: " think"}
		events <- &messages.StreamEvent{Type: messages.EventTypeContent, Content: "answer"}
		time.Sleep(thought)
		events <- &messages.StreamEvent{Type: messages.EventTypeComplete, Message: &messages.ChatMessage{Role: messages.MessageRoleAssistant, Reasoning: "let me think", Content: "answer"}}
	}()
	response, err := (&Agent{}).processEvents(context.Background(), events, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := response.ThinkingDuration()
	if got < thought || got >= 2*thought {
		t.Fatalf("thinking duration = %v, want about %v (reasoning to first content, not to completion)", got, thought)
	}
}

// TestProcessEventsThinkingRunsToCompletionWithoutContent: a reasoning-only
// response that goes straight to tool calls times its thinking to the end of
// the stream.
func TestProcessEventsThinkingRunsToCompletionWithoutContent(t *testing.T) {
	const thought = 20 * time.Millisecond
	events := make(chan *messages.StreamEvent)
	go func() {
		defer close(events)
		events <- &messages.StreamEvent{Type: messages.EventTypeReasoning, Content: "plan"}
		time.Sleep(thought)
		events <- &messages.StreamEvent{Type: messages.EventTypeComplete, Message: &messages.ChatMessage{
			Role:      messages.MessageRoleAssistant,
			Reasoning: "plan",
			ToolCalls: []messages.ChatMessageToolCall{{ID: "1", Name: "bash", Arguments: `{}`}},
		}}
	}()
	response, err := (&Agent{}).processEvents(context.Background(), events, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := response.ThinkingDuration(); got < thought {
		t.Fatalf("thinking duration = %v, want at least %v", got, thought)
	}
}

// TestProcessEventsLeavesThinkingUnsetWithoutReasoning: no reasoning, no
// metadata, so hydration cannot mistake a plain answer for a timed thought.
func TestProcessEventsLeavesThinkingUnsetWithoutReasoning(t *testing.T) {
	events := make(chan *messages.StreamEvent, 2)
	events <- &messages.StreamEvent{Type: messages.EventTypeContent, Content: "answer"}
	events <- &messages.StreamEvent{Type: messages.EventTypeComplete, Message: &messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "answer"}}
	close(events)
	response, err := (&Agent{}).processEvents(context.Background(), events, nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.ThinkingDuration() != 0 || response.Metadata[messages.MetadataKeyThinkingMillis] != nil {
		t.Fatalf("thinking metadata recorded without reasoning: %#v", response.Metadata)
	}
}

// TestAgentToolMessagesRecordDuration: the tool result carries the same
// wall-clock duration the live OnToolEnd callback saw.
func TestAgentToolMessagesRecordDuration(t *testing.T) {
	const work = 20 * time.Millisecond
	tool := &tools.Func{
		Name: "slow_tool",
		Run: func(context.Context, tools.Args) (string, error) {
			time.Sleep(work)
			return "result", nil
		},
	}
	agent := NewAgent(nil, tools.NewToolRegistry([]tools.Tool{tool}), AgentConfig{})
	var reported time.Duration
	cb := &AgentCallbacks{OnToolEnd: func(_ messages.ChatMessageToolCall, _ string, duration time.Duration, _ error) {
		reported = duration
	}}
	msg, err := agent.executeTool(context.Background(), messages.ChatMessageToolCall{ID: "1", Name: "slow_tool", Arguments: `{}`}, cb)
	if err != nil {
		t.Fatal(err)
	}
	got := msg.ToolDuration()
	if got < work {
		t.Fatalf("tool duration = %v, want at least %v", got, work)
	}
	if reported < got || reported-got >= time.Millisecond {
		t.Fatalf("stored duration %v does not match the live callback's %v (millisecond precision)", got, reported)
	}
}
