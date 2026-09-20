package qwencloud

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/alexschlessinger/pollytool/llm/internal/contract"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestModelWireOptions(t *testing.T) {
	cases := []struct {
		model      string
		limitField string
		preserve   bool
	}{
		{"qwen3-coder-plus", "max_tokens", false},
		{"qwen3-coder-plus-2025-09-23", "max_tokens", false},
		{"qwen-plus", "max_tokens", false},
		{"qwen3-max", "max_tokens", false},
		{"qwen3.6-max-preview", "max_tokens", true},
		{"qwen3.7-max", "max_completion_tokens", true},
		{"qwen3.7-max-2026-06-08", "max_completion_tokens", true},
		{"qwen3.7-max-preview", "max_completion_tokens", false},
		{"qwen3.5-plus", "max_completion_tokens", false},
		{"qwen3.6-plus-2026-04-02", "max_completion_tokens", true},
		{"qwen3.7-plus", "max_completion_tokens", true},
		{"qwen3.5-flash", "max_completion_tokens", false},
		{"qwen3.7-flash-2026-07-15", "max_completion_tokens", true},
		{"qwen3.8-max-0902", "max_completion_tokens", true},
		{"qwen3.8-flash", "max_completion_tokens", true},
		{"qwen3.8-omni-flash", "max_tokens", true},
		{"qwen3.8-2.4t-a95b", "max_tokens", false},
		{"deepseek-v3.2-exp", "max_completion_tokens", false},
		{"deepseek-v4-pro-0813", "max_completion_tokens", false},
		{"deepseek-v4.1-flash", "max_completion_tokens", false},
		{"deepseek-r1-0528", "max_completion_tokens", false},
		{"kimi-k2.7-code", "max_tokens", true},
		{"kimi/kimi-k2.7-code-highspeed", "max_tokens", true},
		{"custom-model", "max_tokens", false},
		{"qwen3.8-maximal", "max_tokens", false},
	}
	for _, tc := range cases {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", tc.model, stream), func(t *testing.T) {
				req := &contract.CompletionRequest{Model: tc.model, MaxTokens: 128, ThinkingEffort: contract.EffortDynamic(), Messages: []messages.ChatMessage{
					{Role: messages.MessageRoleAssistant, Content: "answer", Reasoning: "prior reasoning"},
				}}
				body := wireRequest(t, req, stream)
				if body[tc.limitField] != float64(128) {
					t.Fatalf("missing supported token limit %s: %v", tc.limitField, body)
				}
				otherField := "max_tokens"
				if tc.limitField == otherField {
					otherField = "max_completion_tokens"
				}
				if _, ok := body[otherField]; ok {
					t.Fatalf("sent both token limit parameters: %v", body)
				}
				preserve, present := body["preserve_thinking"]
				if present != tc.preserve || (tc.preserve && preserve != true) {
					t.Fatalf("preserve_thinking = %v, present = %v, want %v", preserve, present, tc.preserve)
				}
			})
		}
	}
}

func TestOmitUnusedWireOptions(t *testing.T) {
	for _, model := range []string{"qwen3.8-flash", "qwen3-coder-plus"} {
		for _, history := range [][]messages.ChatMessage{
			nil,
			{{Role: messages.MessageRoleAssistant, Content: "no reasoning"}},
			{{Role: messages.MessageRoleUser, Content: "question", Reasoning: "not assistant reasoning"}},
		} {
			body := wireRequest(t, &contract.CompletionRequest{Model: model, Messages: history}, true)
			for _, key := range []string{"max_tokens", "max_completion_tokens", "preserve_thinking"} {
				if _, ok := body[key]; ok {
					t.Fatalf("%s: unexpected %s without a limit or assistant reasoning: %v", model, key, body)
				}
			}
		}
	}
}

func wireRequest(t *testing.T, req *contract.CompletionRequest, stream bool) map[string]any {
	t.Helper()
	data, err := json.Marshal(buildRequest(req).Streaming(stream))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatal(err)
	}
	return body
}
