package llm

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

type untimedFunc struct{ *tools.Func }

func (untimedFunc) Untimed() bool { return true }

func waitingTool(name string) *tools.Func {
	return &tools.Func{Name: name, Desc: name, Run: func(ctx context.Context, _ tools.Args) (string, error) {
		select {
		case <-time.After(60 * time.Millisecond):
			return "waited", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}}
}

func toolResultText(resp *AgentResponse, name string) string {
	for _, m := range resp.AllMessages {
		if m.Role == messages.MessageRoleTool && m.ToolName == name {
			return m.GetContent()
		}
	}
	return ""
}

func TestUntimedToolsSkipTheToolTimeout(t *testing.T) {
	registry := tools.NewToolRegistry([]tools.Tool{untimedFunc{waitingTool("slow_agent")}, waitingTool("slow")})
	fake := &sequentialLLM{responses: []messages.ChatMessage{
		{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: "1", Name: "slow_agent", Arguments: `{}`}}},
		{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: "2", Name: "slow", Arguments: `{}`}}},
	}}
	agent := NewAgent(fake, registry, AgentConfig{ToolTimeout: 20 * time.Millisecond})
	resp, err := agent.Run(context.Background(), &CompletionRequest{Model: "test/model", Messages: messages.User("go")}, &AgentCallbacks{})
	if err != nil {
		t.Fatal(err)
	}
	if got := toolResultText(resp, "slow_agent"); got != "waited" {
		t.Fatalf("untimed tool result = %q, want it to finish", got)
	}
	if got := toolResultText(resp, "slow"); !strings.Contains(got, "timed out") {
		t.Fatalf("timed tool result = %q, want the timeout", got)
	}
}
