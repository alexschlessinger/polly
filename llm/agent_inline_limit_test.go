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

// A run holds the inline limit to a tenth of its budget, and the pages its
// tools return to that size, so a batch of unread results cannot by itself
// push a request into compaction.
func TestInlineLimitFollowsTheBudget(t *testing.T) {
	full := strings.Repeat("x", 24_000)
	var pageBytes int
	tool := &tools.Func{Name: "large", Run: func(ctx context.Context, _ tools.Args) (string, error) {
		pageBytes = tools.PageBytes(ctx)
		return full, nil
	}}
	model := &recordingSequentialLLM{responses: []messages.ChatMessage{
		{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: "large-call", Name: "large", Arguments: `{}`}}},
		{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonEndTurn, Content: "done"},
	}}
	registry := tools.NewToolRegistry([]tools.Tool{tool})
	defer registry.Close()
	agent := NewAgent(model, registry, AgentConfig{ArtifactStore: newTestArtifactStore()})
	defer agent.Close()
	response, err := agent.Run(context.Background(), &CompletionRequest{Messages: messages.User("run"), MaxContextTokens: 16_000}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if pageBytes != 4*1_600-4 {
		t.Fatalf("page cap = %d, want %d", pageBytes, 4*1_600-4)
	}
	for _, msg := range response.AllMessages {
		if msg.Role == messages.MessageRoleTool && (len(msg.Parts) != 1 || msg.Parts[0].Artifact == nil) {
			t.Fatalf("a 6,000-token result on a 16,000-token budget stayed inline: %d bytes", len(msg.Content))
		}
	}
}

// A host's low inline limit still leaves pages room for two previews.
func TestPagesKeepRoomUnderALowInlineLimit(t *testing.T) {
	if got := tools.PageBytes(pageCap(context.Background(), 10)); got != 4*2*toolPreviewTokenLimit-4 {
		t.Fatalf("page cap under a 10-token limit = %d", got)
	}
}
