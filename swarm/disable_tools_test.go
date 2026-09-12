package swarm

import (
	"context"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestMemberRetainsInheritedDisableTools(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		name := "inherit"
		if explicit {
			name = "requested"
		}
		t.Run(name, func(t *testing.T) {
			calls := 0
			model := modelFunc(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
				calls++
				if len(req.Tools) != 0 {
					t.Error("child exposed tools disabled by parent")
				}
				return messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: "read", Name: "read_file", Arguments: `{"path":"missing"}`}}}
			})
			r := runtimeTest(t, model, 1, 1)
			if _, err := r.config.Registry.LoadToolAuto("read_file"); err != nil {
				t.Fatal(err)
			}
			r.UpdateDefaults(r.config.Request, llm.AgentConfig{DisableTools: true, MaxIterations: 2}, nil)
			req := AgentRequest{Label: "Test agent", Task: "inspect", ReadOnly: true}
			if explicit {
				req.Tools = []string{"read_file"}
			}
			_, err := r.Agent(context.Background(), "", req)
			if err == nil || !strings.Contains(err.Error(), "tool execution is disabled") || calls != 1 {
				t.Fatalf("disabled child: calls=%d error=%v", calls, err)
			}
		})
	}
}
