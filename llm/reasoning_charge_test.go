package llm

import (
	"context"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
)

// Plain reasoning text is charged only where the provider replays it. A
// provider that never sends it back is charged nothing for it, in the
// projection's estimate and in the floor a host sizes a budget by.
func TestReasoningIsChargedOnlyWhereReplayed(t *testing.T) {
	reasoning := strings.Repeat("deep thought ", 2_000)
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleSystem, Content: "You are terse."},
		{Role: messages.MessageRoleUser, Content: "think"},
		{Role: messages.MessageRoleAssistant, Reasoning: reasoning, ToolCalls: []messages.ChatMessageToolCall{{ID: "c1", Name: "fetch", Arguments: `{}`}}},
		{Role: messages.MessageRoleTool, ToolCallID: "c1", ToolName: "fetch", Content: "fetched"},
	}
	estimate := func(model string) int {
		t.Helper()
		_, stats, err := projectCompletionRequest(context.Background(), &CompletionRequest{Model: model, Messages: history}, nil, projectionTools{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return stats.EstimatedTokens
	}
	plain := estimatedStringTokens(reasoning)
	replayed, unsent := estimate("deepseek/deepseek-v4.1-flash"), estimate("openai/gpt-5.5")
	if replayed-unsent != plain {
		t.Fatalf("estimates %d replayed, %d unsent: want the reasoning, %d, between them", replayed, unsent, plain)
	}
	floor := func(model string) int {
		return ContextFloorTokens(&CompletionRequest{Model: model, Messages: history})
	}
	if replayed, unsent := floor("deepseek/deepseek-v4.1-flash"), floor("openai/gpt-5.5"); replayed-unsent != plain {
		t.Fatalf("floors %d replayed, %d unsent: want the reasoning, %d, between them", replayed, unsent, plain)
	}
	// One run's cache follows the model its requests go to.
	state := &runState{shape: newRequestShapeCache(history), projection: &projectionCache{}}
	for _, model := range []string{"deepseek/deepseek-v4.1-flash", "openai/gpt-5.5", "deepseek/deepseek-v4.1-flash"} {
		if got := func() int {
			_, stats, err := projectCompletionRequest(context.Background(), &CompletionRequest{Model: model, Messages: history}, nil, projectionTools{}, state)
			if err != nil {
				t.Fatal(err)
			}
			return stats.EstimatedTokens
		}(); got != estimate(model) {
			t.Fatalf("%s estimated at %d through a shared cache, %d alone", model, got, estimate(model))
		}
	}
}
