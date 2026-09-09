package llm

import (
	"context"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

func TestAgentTerminalCallbacksAndCheckpoint(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		stop                         messages.StopReason
		tool, denied, receipt, fails bool
	}{
		{name: "normal", stop: messages.StopReasonEndTurn},
		{name: "unknown", stop: "provider_specific"},
		{name: "truncated", stop: messages.StopReasonMaxTokens},
		{name: "denied", stop: messages.StopReasonToolUse, tool: true, denied: true},
		{name: "response tool", stop: messages.StopReasonToolUse, tool: true, receipt: true},
		{name: "content filter", stop: messages.StopReasonContentFilter, fails: true},
		{name: "malformed", stop: messages.StopReasonError, fails: true},
		{name: "missing calls", stop: messages.StopReasonToolUse, fails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "answer", StopReason: tc.stop}
			if tc.tool {
				msg.ToolCalls = []messages.ChatMessageToolCall{{ID: "call", Name: "work", Arguments: "{}"}}
			}
			registry := tools.NewToolRegistry([]tools.Tool{&tools.Func{Name: "work", Run: func(context.Context, tools.Args) (string, error) {
				if tc.denied {
					t.Error("denied tool executed")
				}
				return "result", nil
			}}})
			defer registry.Close()
			config := AgentConfig{MaxIterations: 2}
			if tc.receipt {
				config.ResponseTool, config.RequireResponseToolSuccess = "work", true
			}
			model := &sequentialLLM{responses: []messages.ChatMessage{msg}}
			agent := NewAgent(model, registry, config)
			defer agent.Close()
			completed, continued, finalCheckpoints, failures := 0, 0, 0, 0
			var finalHistory []messages.ChatMessage
			result, err := agent.Run(context.Background(), &CompletionRequest{}, &AgentCallbacks{
				ApproveToolCalls: func([]messages.ChatMessageToolCall) []bool { return []bool{!tc.denied} },
				ContinueAfterFinal: func(context.Context, *messages.ChatMessage) ([]messages.ChatMessage, error) {
					continued++
					return nil, nil
				},
				OnComplete: func(*messages.ChatMessage) {
					if continued != 1 || finalCheckpoints != 0 {
						t.Error("completion ran outside continuation/checkpoint boundary")
					}
					completed++
				},
				OnError: func(error) { failures++ },
				Checkpoint: func(_ context.Context, cp AgentCheckpoint) error {
					if cp.Final {
						finalCheckpoints++
						finalHistory = cloneMessages(cp.Generated)
					}
					return nil
				},
			})
			if (err != nil) != tc.fails || model.callCount != 1 || finalCheckpoints != 1 {
				t.Fatalf("error=%v calls=%d checkpoints=%d", err, model.callCount, finalCheckpoints)
			}
			wantCompleted, wantFailures := 1, 0
			if tc.fails {
				wantCompleted, wantFailures = 0, 1
			}
			if completed != wantCompleted || continued != wantCompleted || failures != wantFailures {
				t.Fatalf("complete=%d continue=%d errors=%d", completed, continued, failures)
			}
			if result == nil || result.PersistedMessages != len(result.AllMessages) || len(finalHistory) != len(result.AllMessages) {
				t.Fatalf("final checkpoint lost history: %+v", result)
			}
		})
	}
}
