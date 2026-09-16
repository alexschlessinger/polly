package llm

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

// TestProcessEventsRecordsThinkingDuration: the clock runs from the first
// reasoning delta to the first content delta, so a resumed transcript can show
// how long the model thought.
func TestProcessEventsRecordsThinkingDuration(t *testing.T) {
	// The receiver stamps the clock with time.Now when it handles an event,
	// and OnReasoning and OnContent fire right after those stamps. The sender
	// starts the thought only once the clock has started and measures its own
	// upper bound once the clock has stopped, so neither bound depends on how
	// promptly a loaded runner schedules the receiver. A clock that stopped at
	// completion instead would exceed the bound by the whole tail.
	const thought = 20 * time.Millisecond
	const tail = 10 * thought
	started := make(chan struct{})
	stopped := make(chan struct{})
	var startOnce, stopOnce sync.Once
	cb := &AgentCallbacks{
		OnReasoning: func(string) { startOnce.Do(func() { close(started) }) },
		OnContent:   func(string) { stopOnce.Do(func() { close(stopped) }) },
	}
	var bound time.Duration
	events := make(chan *messages.StreamEvent)
	go func() {
		defer close(events)
		begin := time.Now()
		events <- &messages.StreamEvent{Type: messages.EventTypeReasoning, Content: "let me"}
		<-started
		time.Sleep(thought)
		events <- &messages.StreamEvent{Type: messages.EventTypeReasoning, Content: " think"}
		events <- &messages.StreamEvent{Type: messages.EventTypeContent, Content: "answer"}
		<-stopped
		bound = time.Since(begin)
		time.Sleep(tail)
		events <- &messages.StreamEvent{Type: messages.EventTypeComplete, Message: &messages.ChatMessage{Role: messages.MessageRoleAssistant, Reasoning: "let me think", Content: "answer"}}
	}()
	response, _, err := (&Agent{}).processEvents(context.Background(), events, cb)
	if err != nil {
		t.Fatal(err)
	}
	got := response.ThinkingDuration()
	if got < thought || got > bound {
		t.Fatalf("thinking duration = %v, want at least %v and at most %v (reasoning to first content, not to completion)", got, thought, bound)
	}
}

// TestProcessEventsThinkingRunsToCompletionWithoutContent: a reasoning-only
// response that goes straight to tool calls times its thinking to the end of
// the stream.
func TestProcessEventsThinkingRunsToCompletionWithoutContent(t *testing.T) {
	const thought = 20 * time.Millisecond
	started := make(chan struct{})
	var once sync.Once
	cb := &AgentCallbacks{OnReasoning: func(string) { once.Do(func() { close(started) }) }}
	events := make(chan *messages.StreamEvent)
	go func() {
		defer close(events)
		events <- &messages.StreamEvent{Type: messages.EventTypeReasoning, Content: "plan"}
		<-started
		time.Sleep(thought)
		events <- &messages.StreamEvent{Type: messages.EventTypeComplete, Message: &messages.ChatMessage{
			Role:      messages.MessageRoleAssistant,
			Reasoning: "plan",
			ToolCalls: []messages.ChatMessageToolCall{{ID: "1", Name: "bash", Arguments: `{}`}},
		}}
	}()
	response, _, err := (&Agent{}).processEvents(context.Background(), events, cb)
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
	response, _, err := (&Agent{}).processEvents(context.Background(), events, nil)
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
	call := messages.ChatMessageToolCall{ID: "1", Name: "slow_tool", Arguments: `{}`}
	msg, err := agent.executeTool(context.Background(), call, agent.resolveTools([]messages.ChatMessageToolCall{call})[0], cb)
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
