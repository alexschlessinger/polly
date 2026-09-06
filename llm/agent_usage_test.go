package llm

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

func TestAgentRequestProjectionAndUsageLifecycle(t *testing.T) {
	first := messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: "1", Name: "noop", Arguments: `{}`}}}
	first.SetTokenUsage(100, 20)
	last := messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonEndTurn, Content: "done"}
	last.SetTokenUsage(80, 10)
	model := &sequentialLLM{responses: []messages.ChatMessage{first, last}}
	var events []string
	registry := tools.NewToolRegistry([]tools.Tool{&tools.Func{Name: "noop", Run: func(context.Context, tools.Args) (string, error) { return "ok", nil }}})
	defer registry.Close()
	agent := NewAgent(model, registry, AgentConfig{})
	defer agent.Close()
	_, err := agent.Run(context.Background(), &CompletionRequest{Messages: messages.User("hi")}, &AgentCallbacks{
		BeforeFirstRequest: func(ProjectionStats) error { events = append(events, "persist"); return nil },
		OnRequestProjection: func(iteration int, stats ProjectionStats) {
			if model.callCount != iteration || stats.RequestEstimatedTokens <= 0 {
				t.Fatalf("projection arrived after request or has no estimate: %+v", stats)
			}
			events = append(events, fmt.Sprintf("project %d", iteration))
		},
		OnIterationUsage: func(iteration, in, out int) {
			if model.callCount != iteration+1 {
				t.Fatal("usage arrived before provider completion")
			}
			events = append(events, fmt.Sprintf("usage %d %d/%d", iteration, in, out))
		},
		OnToolStart: func([]messages.ChatMessageToolCall) { events = append(events, "tools") },
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"persist", "project 0", "usage 0 100/20", "tools", "project 1", "usage 1 80/10"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events=%v want=%v", events, want)
	}
	// Iteration identities are local to each run and callbacks stay optional.
	_, err = agent.Run(context.Background(), &CompletionRequest{Messages: messages.User("again")}, &AgentCallbacks{OnRequestProjection: func(iteration int, _ ProjectionStats) {
		if iteration != 0 {
			t.Fatalf("next run starts at %d", iteration)
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAgentPersistenceVetoSuppressesProjectionAndUsage(t *testing.T) {
	agent := NewAgent(&sequentialLLM{}, nil, AgentConfig{})
	defer agent.Close()
	veto := errors.New("store failed")
	_, err := agent.Run(context.Background(), &CompletionRequest{Messages: messages.User("hi")}, &AgentCallbacks{
		BeforeFirstRequest:  func(ProjectionStats) error { return veto },
		OnRequestProjection: func(int, ProjectionStats) { t.Fatal("projection passed failed persistence gate") },
		OnIterationUsage:    func(int, int, int) { t.Fatal("usage without a request") },
	})
	if !errors.Is(err, veto) {
		t.Fatal(err)
	}
}
