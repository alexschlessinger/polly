package llm

import (
	"context"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

func TestAgentDisableToolsRejectsReturnedCalls(t *testing.T) {
	calls, callbacks := 0, 0
	registry := tools.NewToolRegistry([]tools.Tool{&tools.Func{Name: "effect", Run: func(context.Context, tools.Args) (string, error) { calls++; return "effect happened", nil }}})
	defer registry.Close()
	model := &promptCacheRecordingLLM{responses: []messages.ChatMessage{{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: "a", Name: "effect", Arguments: `{}`}}}}}
	agent := NewAgent(model, registry, AgentConfig{DisableTools: true, MaxIterations: 2})
	defer agent.Close()
	response, err := agent.Run(context.Background(), &CompletionRequest{}, &AgentCallbacks{
		BeforeToolBatch: func(context.Context, []messages.ChatMessageToolCall) error { callbacks++; return nil },
		BeforeToolExecute: func(ctx context.Context, _ messages.ChatMessageToolCall, _ map[string]any) context.Context {
			callbacks++
			return ctx
		},
		ApproveToolCalls: func(calls []messages.ChatMessageToolCall) []bool { callbacks++; return []bool{true} },
	})
	if err == nil || !strings.Contains(err.Error(), "tool execution is disabled") {
		t.Fatalf("Run: %v", err)
	}
	if len(model.requests) != 1 || len(model.requests[0].Tools) != 0 {
		t.Fatal("expected one tool-free model request")
	}
	if calls != 0 || callbacks != 0 {
		t.Fatalf("disabled tool reached execution or approval: calls=%d callbacks=%d", calls, callbacks)
	}
	if response == nil || len(response.AllMessages) < 2 {
		t.Fatal("disabled batch was not retained with matching tool outcomes")
	}
	last := response.AllMessages[len(response.AllMessages)-1]
	if last.Role != messages.MessageRoleTool || last.ToolCallID != "a" {
		t.Fatalf("disabled batch outcome: %+v", last)
	}

	// Direct invocation and even a later always-allowed registration retain
	// the same execution bound; tool availability cannot override it.
	agent.ToolRegistry().Register(&tools.Func{Name: "private_effect", Run: func(context.Context, tools.Args) (string, error) { calls++; return "effect", nil }})
	agent.ToolRegistry().MarkAlwaysAllowed("private_effect")
	if _, err := agent.executeToolCall(context.Background(), messages.ChatMessageToolCall{Name: "private_effect", Arguments: `{}`}, nil); err == nil || calls != 0 {
		t.Fatalf("direct disabled invocation: calls=%d error=%v", calls, err)
	}
}
