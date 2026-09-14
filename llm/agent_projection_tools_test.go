package llm

import (
	"context"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

func TestAgentProjectionTracksTranscriptTool(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		remove, disable, unsupported bool
	}{
		{name: "available"},
		{name: "removed", remove: true},
		{name: "disabled", disable: true},
		{name: "unsupported", unsupported: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry := tools.NewToolRegistry(nil)
			defer registry.Close()
			want := !tc.remove && !tc.disable && !tc.unsupported
			client := ownershipLLM(func(_ context.Context, req *CompletionRequest) messages.ChatMessage {
				advertised, recommended := false, false
				for _, tool := range req.Tools {
					if tool.GetName() == "read_transcript" {
						advertised = true
					}
				}
				for _, msg := range req.Messages {
					if strings.Contains(msg.Content, "call read_transcript") {
						recommended = true
					}
				}
				if advertised != want || recommended != want {
					t.Errorf("read_transcript: advertised=%v recommended=%v, want %v", advertised, recommended, want)
				}
				return messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "done", StopReason: messages.StopReasonEndTurn}
			})
			agent := NewAgent(client, registry, AgentConfig{DisableTools: tc.disable})
			defer agent.Close()
			if tc.disable {
				// Even an explicitly registered reader must not be advertised
				// when the caller has disabled tool use for this agent.
				agent.ToolRegistry().Register(&readTranscriptTool{})
			}
			if tc.remove {
				agent.ToolRegistry().Remove("read_transcript")
			}
			req := &CompletionRequest{
				MaxContextTokens: 2000,
				Messages: []messages.ChatMessage{
					{Role: messages.MessageRoleUser, Content: strings.Repeat("old history ", 2000)},
					{Role: messages.MessageRoleAssistant, Content: "old answer"},
					{Role: messages.MessageRoleUser, Content: "new question"},
				},
			}
			if tc.unsupported {
				req.Capabilities = &ModelCapabilities{Tools: truth(false)}
			}
			resp, err := agent.Run(context.Background(), req, nil)
			if err != nil {
				t.Fatal(err)
			}
			if resp.Projection.OmittedExchanges != 1 {
				t.Fatalf("omitted exchanges = %d, want 1", resp.Projection.OmittedExchanges)
			}
		})
	}
}

func TestUnsupportedToolsPreserveTranscriptRecallRendering(t *testing.T) {
	registry := tools.NewToolRegistry(nil)
	defer registry.Close()
	client := ownershipLLM(func(context.Context, *CompletionRequest) messages.ChatMessage {
		return messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "done", StopReason: messages.StopReasonEndTurn}
	})
	agent := NewAgent(client, registry, AgentConfig{})
	defer agent.Close()
	_, err := agent.Run(context.Background(), &CompletionRequest{
		Capabilities: &ModelCapabilities{Tools: truth(false)},
		Messages: []messages.ChatMessage{
			{Role: messages.MessageRoleUser, Content: "old question"},
			{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "recall", Name: "read_transcript", Arguments: `{}`}}},
			{Role: messages.MessageRoleTool, ToolName: "read_transcript", ToolCallID: "recall", Content: "nested recalled transcript"},
			{Role: messages.MessageRoleAssistant, Content: "old answer"},
			{Role: messages.MessageRoleUser, Content: "new question"},
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	reader, ok := agent.ToolRegistry().Get("read_transcript")
	if !ok {
		t.Fatal("request adaptation removed the durable transcript reader")
	}
	out, err := reader.Execute(context.Background(), nil)
	if err != nil || strings.Contains(out, "nested recalled transcript") || !strings.Contains(out, "read_transcript result not rendered") || !strings.Contains(out, "old question") {
		t.Fatalf("durable recall rendering changed: %q, %v", out, err)
	}
}
