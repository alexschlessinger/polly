package llm

import (
	"context"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

// A result of about 300 estimated tokens: inline under the default limit,
// stored under a limit of 100.
const inlineLimitFixtureBytes = 1_200

func TestInlineToolResultTokensStoresResultsAtBirth(t *testing.T) {
	full := "HEAD\n" + strings.Repeat("x", inlineLimitFixtureBytes) + "\nTAIL"
	for _, tc := range []struct {
		name   string
		limit  int
		stored bool
	}{
		{name: "default keeps a small result inline", limit: 0},
		{name: "configured limit stores it", limit: 100, stored: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool := &tools.Func{Name: "large", Run: func(context.Context, tools.Args) (string, error) { return full, nil }}
			model := &recordingSequentialLLM{responses: []messages.ChatMessage{
				{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: "large-call", Name: "large", Arguments: `{}`}}},
				{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonEndTurn, Content: "done"},
			}}
			store := newTestArtifactStore()
			registry := tools.NewToolRegistry([]tools.Tool{tool})
			defer registry.Close()
			agent := NewAgent(model, registry, AgentConfig{ArtifactStore: store, InlineToolResultTokens: tc.limit})
			defer agent.Close()
			response, err := agent.Run(context.Background(), &CompletionRequest{Messages: messages.User("run")}, nil)
			if err != nil {
				t.Fatal(err)
			}
			var durable messages.ChatMessage
			for _, msg := range response.AllMessages {
				if msg.Role == messages.MessageRoleTool {
					durable = msg
				}
			}
			if !tc.stored {
				if len(durable.Parts) != 0 || durable.Content != full {
					t.Fatalf("small result was stored: %#v", durable)
				}
				return
			}
			if len(durable.Parts) != 1 || durable.Parts[0].Artifact == nil || durable.Content != artifactBirthPreview(*durable.Parts[0].Artifact, []byte(full)) {
				t.Fatalf("durable result = %#v, want a stored artifact with a preview", durable)
			}
			// The model's second request saw the preview, not the full text.
			if len(model.requests) != 2 {
				t.Fatalf("requests = %d, want 2", len(model.requests))
			}
			for _, msg := range model.requests[1] {
				if msg.Role == messages.MessageRoleTool && strings.Contains(msg.Content, strings.Repeat("x", inlineLimitFixtureBytes)) {
					t.Fatal("the full result reached the model")
				}
			}
		})
	}
}

func TestInlineToolResultTokensAppliesToProjectedHistory(t *testing.T) {
	data := strings.Repeat("x", inlineLimitFixtureBytes)
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "run"},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "call", Name: "tool", Arguments: `{}`}}},
		{Role: messages.MessageRoleTool, ToolCallID: "call", ToolName: "tool", Content: data},
	}
	for _, tc := range []struct {
		name    string
		limit   int
		preview bool
	}{
		{name: "default", limit: 0},
		{name: "configured", limit: 100, preview: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agentTools := builtinProjectionTools(false)
			agentTools.inlineTokens = tc.limit
			projected, stats, err := projectMessagesCached(context.Background(), cloneMessages(history), 0, newTestArtifactStore(), agentTools, nil)
			if err != nil {
				t.Fatal(err)
			}
			toolMessage := projected[len(projected)-1]
			if tc.preview {
				if !strings.Contains(toolMessage.Content, "Head/tail preview follows") || stats.CompactedToolResults != 1 {
					t.Fatalf("configured limit did not store the result: stats=%+v content=%q", stats, toolMessage.Content[:min(120, len(toolMessage.Content))])
				}
			} else if toolMessage.Content != data || stats.CompactedToolResults != 0 {
				t.Fatalf("default limit changed a small result: stats=%+v", stats)
			}
		})
	}
}
