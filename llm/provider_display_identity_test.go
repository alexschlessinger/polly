package llm

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/alexschlessinger/pollytool/llm/anthropic"
	"github.com/alexschlessinger/pollytool/llm/deepseek"
	"github.com/alexschlessinger/pollytool/llm/gemini"
	"github.com/alexschlessinger/pollytool/llm/ollama"
	"github.com/alexschlessinger/pollytool/llm/openai"
	"github.com/alexschlessinger/pollytool/llm/openrouter"
	"github.com/alexschlessinger/pollytool/llm/qwencloud"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestProviderRequestsIgnoreDisplayIdentity(t *testing.T) {
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleSystem, Content: "help"},
		{Role: messages.MessageRoleUser, Content: "work"},
		{Role: messages.MessageRoleAssistant, Reasoning: "reasoning", TextBlocks: []messages.AssistantText{{ID: "provider-text", Phase: messages.PhaseCommentary, Text: "checking"}}, ToolCalls: []messages.ChatMessageToolCall{{ID: "call_protocol", Name: "work", Arguments: `{}`}}},
		{Role: messages.MessageRoleTool, ToolCallID: "call_protocol", ToolName: "work", Content: "result"},
		{Role: messages.MessageRoleUser, Content: "continue"},
	}
	stamped := cloneMessages(history)
	for i := range stamped {
		stamped[i].Metadata = map[string]any{messages.MetadataKeyDisplayID: fmt.Sprintf("host-message-%d", i)}
		for j := range stamped[i].ToolCalls {
			stamped[i].ToolCalls[j].DisplayID = "host-call"
		}
	}
	stamped[3].Metadata[messages.MetadataKeyDisplayID] = "host-call"
	const chatResponse = `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`
	for _, tc := range []struct {
		name, model, response string
		client                func(string) LLM
	}{
		{"openai chat", "custom-model", chatResponse, func(url string) LLM { return openai.NewProvider("test-key", url+"/v1") }},
		{"openai responses", "gpt-5.4", `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}`, func(url string) LLM { return openai.NewResponsesProvider("test-key", url+"/v1") }},
		{"anthropic", "claude-sonnet-4-6", `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`, func(url string) LLM { return anthropic.NewProvider("test-key", url) }},
		{"gemini", "gemini-2.5-flash", `{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`, func(url string) LLM {
			client, err := gemini.NewProvider("test-key", url)
			if err != nil {
				t.Fatal(err)
			}
			return client
		}},
		{"ollama", "llama", `{"message":{"role":"assistant","content":"ok"},"done":true}`, func(url string) LLM { return ollama.NewProvider(url, "") }},
		{"openrouter", "anthropic/claude-sonnet-4-6", chatResponse, func(url string) LLM { return openrouter.NewProvider("test-key", url+"/v1") }},
		{"deepseek", "deepseek-chat", chatResponse, func(url string) LLM { return deepseek.NewProvider("test-key", url) }},
		{"qwencloud", "qwen3.8-plus", chatResponse, func(url string) LLM { return qwencloud.NewProvider("test-key", url) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url, captured := newNativeRequestCaptureServer(t, tc.response)
			client := tc.client(url)
			before := captureNativeCompletionMessages(t, client, tc.model, history, captured)
			after := captureNativeCompletionMessages(t, client, tc.model, stamped, captured)
			if !bytes.Equal(before.body, after.body) {
				t.Fatalf("host identity changed provider request:\nbefore: %s\nafter: %s", before.body, after.body)
			}
			for _, forbidden := range []string{"display_id", "host-message-", "host-call"} {
				if bytes.Contains(after.body, []byte(forbidden)) {
					t.Fatalf("host identity leaked to wire: %s", after.body)
				}
			}
			if !bytes.Contains(after.body, []byte("call_protocol")) {
				t.Fatalf("provider call ID disappeared: %s", after.body)
			}
		})
	}
}
