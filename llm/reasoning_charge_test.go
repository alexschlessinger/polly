package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm/anthropic"
	"github.com/alexschlessinger/pollytool/llm/gemini"
	"github.com/alexschlessinger/pollytool/llm/openai"
	"github.com/alexschlessinger/pollytool/llm/openrouter"
	"github.com/alexschlessinger/pollytool/messages"
)

// Plain reasoning text is charged only where the provider replays it. A
// provider that never sends it back is charged nothing for it.
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
		_, stats, err := projectCompletionRequest(context.Background(), &CompletionRequest{Model: model, Messages: history}, nil, nil)
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
}

func TestReasoningEstimatesFollowReplayFences(t *testing.T) {
	opaque := strings.Repeat("encrypted", 8_000)
	openAIState := func(model string, valid bool) map[string]any {
		item := map[string]any{"encrypted_content": opaque}
		if valid {
			item["id"] = "rs_1"
		}
		return map[string]any{
			openai.ResponsesReasoningModelKey: model,
			openai.ResponsesReasoningItemsKey: []map[string]any{item},
		}
	}
	thinking := map[string]any{anthropic.ThinkingBlocksKey: []map[string]any{{"type": "thinking", "thinking": opaque, "signature": "signed"}}}
	signatures := map[string]any{gemini.ThoughtSignaturesKey: map[string]string{"c1": base64.StdEncoding.EncodeToString([]byte(opaque))}}
	for _, tt := range []struct {
		name, model, baseURL string
		metadata             map[string]any
		charged              bool
	}{
		{"same OpenAI model", "openai/gpt-5.5", "", openAIState("gpt-5.5", true), true},
		{"changed OpenAI model", "openai/gpt-5.5", "", openAIState("gpt-5.4", true), false},
		{"unattributed OpenAI state", "openai/gpt-5.5", "", openAIState("", true), false},
		{"incomplete OpenAI item", "openai/gpt-5.5", "", openAIState("gpt-5.5", false), false},
		{"custom Chat endpoint", "openai/gpt-5.5", "https://example.org/v1", openAIState("gpt-5.5", true), false},
		{"blank native endpoint", "openai/gpt-5.5", "  ", openAIState("gpt-5.5", true), true},
		{"same Codex model", "codex/gpt-5.5", "", openAIState("codex/gpt-5.5", true), true},
		{"changed Codex model", "codex/gpt-5.5", "", openAIState("codex/gpt-5.4", true), false},
		{"OpenAI to Codex", "codex/gpt-5.5", "", openAIState("gpt-5.5", true), false},
		{"Codex to OpenAI", "openai/gpt-5.5", "", openAIState("codex/gpt-5.5", true), false},
		{"signed thinking", "anthropic/claude-sonnet-4-6", "", thinking, true},
		{"unsigned thinking", "anthropic/claude-sonnet-4-6", "", map[string]any{anthropic.ThinkingBlocksKey: []map[string]any{{"type": "thinking", "thinking": opaque}}}, false},
		{"redacted thinking", "anthropic/claude-sonnet-4-6", "", map[string]any{anthropic.ThinkingBlocksKey: []map[string]any{{"type": "redacted_thinking", "data": opaque}}}, true},
		{"foreign thinking", "openai/gpt-5.5", "", thinking, false},
		{"foreign encrypted items", "anthropic/claude-sonnet-4-6", "", openAIState("gpt-5.5", true), false},
		{"matching tool signature", "gemini/gemini-3-pro", "", signatures, true},
		{"unmatched tool signature", "gemini/gemini-3-pro", "", map[string]any{gemini.ThoughtSignaturesKey: map[string]string{"other": opaque}}, false},
		{"invalid tool signature", "gemini/gemini-3-pro", "", map[string]any{gemini.ThoughtSignaturesKey: map[string]string{"c1": opaque + "!"}}, false},
		{"foreign tool signature", "openai/gpt-5.5", "", signatures, false},
		{"plain-only provider", "deepseek/deepseek-v4.1-flash", "", thinking, false},
		{"no-replay provider", "ollama/model", "", thinking, false},
		{"unknown provider", "custom/model", "", thinking, false},
		{"foreign gateway state", "openai/gpt-5.5", "", map[string]any{openrouter.MetadataKey: map[string]any{"reasoning_details": opaque}}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			msg := callTool("c1", "fetch", 0)
			msg.Metadata = tt.metadata
			estimate := messageEstimator(&CompletionRequest{Model: tt.model, BaseURL: tt.baseURL})
			for _, reloaded := range []bool{false, true} {
				if reloaded {
					raw, err := json.Marshal(msg)
					if err != nil {
						t.Fatal(err)
					}
					msg = messages.ChatMessage{}
					if err := json.Unmarshal(raw, &msg); err != nil {
						t.Fatal(err)
					}
				}
				plain := msg.Clone()
				plain.Metadata = nil
				charged := estimate(msg) - estimate(plain)
				if tt.charged && charged < len(opaque)/4 || !tt.charged && charged != 0 {
					t.Fatalf("reloaded=%v: charged %d reasoning tokens, want replay=%v", reloaded, charged, tt.charged)
				}
				for _, role := range []string{messages.MessageRoleUser, messages.MessageRoleTool} {
					other, plainOther := msg, plain
					other.Role, plainOther.Role = role, role
					if estimate(other) != estimate(plainOther) {
						t.Fatalf("charged unreplayed reasoning on %s message", role)
					}
				}
			}
		})
	}
}

func TestUnreplayedReasoningDoesNotTriggerPaidCompaction(t *testing.T) {
	for _, model := range []string{"openai/gpt-5.5", "anthropic/claude-sonnet-4-6", "deepseek/deepseek-v4.1-flash"} {
		t.Run(model, func(t *testing.T) {
			calls := 0
			client := &compactionLLM{reply: func(req *CompletionRequest, _ int) (messages.ChatMessage, error) {
				calls++
				if isSummaryRequest(req) {
					t.Error("unreplayed metadata triggered a paid summary")
				}
				return reply("done", 0), nil
			}}
			agent := NewAgent(client, nil, AgentConfig{DisableTools: true})
			defer agent.Close()
			old := reply("hello", 0)
			old.Metadata = map[string]any{
				openai.ResponsesReasoningModelKey: "gpt-5.4",
				openai.ResponsesReasoningItemsKey: []map[string]any{{"id": "rs_old", "encrypted_content": strings.Repeat("x", 60_000)}},
			}
			history := []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "hello"}, old, {Role: messages.MessageRoleUser, Content: "again"}}
			response, err := agent.Run(context.Background(), &CompletionRequest{Model: model, Messages: history, MaxContextTokens: 2_000}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 || response.Projection.RequestEstimatedTokens >= 100 {
				t.Fatalf("calls=%d projection=%+v", calls, response.Projection)
			}
		})
	}
}
