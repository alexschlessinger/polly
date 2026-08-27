package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm/openai"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestProviderPromptCacheRequestPoliciesAndUsage(t *testing.T) {
	const promptKey = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const sessionID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	t.Run("native OpenAI Responses", func(t *testing.T) {
		serverURL, captured := newNativeRequestCaptureServer(t, `{
			"status":"completed",
			"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],
			"usage":{"input_tokens":20,"output_tokens":3,"input_tokens_details":{"cached_tokens":12,"cache_write_tokens":4}}
		}`)
		client := NewOpenAIClient("test-key", "")
		client.client = openai.NewClient("test-key", serverURL+"/v1")
		got, message := capturePromptCacheRequest(t, client, &CompletionRequest{
			Model: "gpt-5.4", PromptCacheKey: promptKey, CacheSessionID: sessionID,
		}, captured)
		body := decodeCapturedBody(t, got.body)
		if body["prompt_cache_key"] != promptKey {
			t.Fatalf("prompt_cache_key = %#v, want %q; body=%s", body["prompt_cache_key"], promptKey, got.body)
		}
		assertBodyKeysAbsent(t, body, "session_id", "cache_control", "prompt_cache_retention")
		assertNoResponseCacheHeaders(t, got.header)
		assertCacheMetadata(t, message, 12, 4)
	})

	t.Run("direct Anthropic", func(t *testing.T) {
		serverURL, captured := newNativeRequestCaptureServer(t, `{
			"content":[{"type":"text","text":"ok"}],
			"stop_reason":"end_turn",
			"usage":{"input_tokens":5,"output_tokens":2,"cache_creation_input_tokens":7,"cache_read_input_tokens":11}
		}`)
		routeDefaultTransportTo(t, serverURL)
		got, message := capturePromptCacheRequest(t, NewAnthropicClient("test-key"), &CompletionRequest{
			Model: "claude-sonnet-4-6", PromptCacheKey: promptKey, CacheSessionID: sessionID,
		}, captured)
		body := decodeCapturedBody(t, got.body)
		control, ok := body["cache_control"].(map[string]any)
		if !ok || control["type"] != "ephemeral" {
			t.Fatalf("cache_control = %#v, want ephemeral; body=%s", body["cache_control"], got.body)
		}
		if _, exists := control["ttl"]; exists {
			t.Fatalf("default five-minute cache must omit ttl: %#v", control)
		}
		assertBodyKeysAbsent(t, body, "prompt_cache_key", "session_id")
		assertNoResponseCacheHeaders(t, got.header)
		if got := message.GetInputTokens(); got != 23 {
			t.Fatalf("normalized Anthropic input tokens = %d, want 5+7+11=23", got)
		}
		assertCacheMetadata(t, message, 11, 7)
	})

	t.Run("OpenRouter", func(t *testing.T) {
		serverURL, captured := newNativeRequestCaptureServer(t, `{
			"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":20,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":13}}
		}`)
		got, message := capturePromptCacheRequest(t, newOpenRouterClient("test-key", serverURL+"/v1"), &CompletionRequest{
			Model: "anthropic/claude-sonnet-4-6", PromptCacheKey: promptKey, CacheSessionID: sessionID,
		}, captured)
		body := decodeCapturedBody(t, got.body)
		if body["session_id"] != sessionID {
			t.Fatalf("session_id = %#v, want %q; body=%s", body["session_id"], sessionID, got.body)
		}
		assertBodyKeysAbsent(t, body, "prompt_cache_key", "cache_control")
		assertNoResponseCacheHeaders(t, got.header)
		assertCacheMetadata(t, message, 13, 0)
	})

	t.Run("custom OpenAI endpoint", func(t *testing.T) {
		serverURL, captured := newNativeRequestCaptureServer(t, `{
			"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":4,"completion_tokens":1}
		}`)
		got, message := capturePromptCacheRequest(t, NewOpenAIClient("test-key", serverURL+"/v1"), &CompletionRequest{
			Model: "custom-model", PromptCacheKey: promptKey, CacheSessionID: sessionID,
		}, captured)
		body := decodeCapturedBody(t, got.body)
		assertBodyKeysAbsent(t, body, "prompt_cache_key", "session_id", "cache_control")
		assertNoResponseCacheHeaders(t, got.header)
		assertCacheMetadata(t, message, 0, 0)
		if _, exists := message.Metadata[messages.MetadataKeyCacheReadInputTokens]; exists {
			t.Fatalf("custom endpoint omitted cache usage but metadata inferred it: %#v", message.Metadata)
		}
	})
}

func TestAutomaticCacheUsageProvidersSendNoControls(t *testing.T) {
	const promptKey = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	const sessionID = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"

	t.Run("DeepSeek", func(t *testing.T) {
		serverURL, captured := newNativeRequestCaptureServer(t, `{
			"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":17,"completion_tokens":2,"prompt_cache_hit_tokens":9,"prompt_cache_miss_tokens":8}
		}`)
		got, message := capturePromptCacheRequest(t, NewDeepSeekClient("test-key", serverURL), &CompletionRequest{
			Model: "deepseek-chat", PromptCacheKey: promptKey, CacheSessionID: sessionID,
		}, captured)
		body := decodeCapturedBody(t, got.body)
		assertBodyKeysAbsent(t, body, "prompt_cache_key", "session_id", "cache_control")
		assertNoResponseCacheHeaders(t, got.header)
		assertCacheMetadata(t, message, 9, 0)
	})

	t.Run("Gemini", func(t *testing.T) {
		serverURL, captured := newNativeRequestCaptureServer(t, `{
			"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}],
			"usageMetadata":{"promptTokenCount":17,"candidatesTokenCount":2,"cachedContentTokenCount":9}
		}`)
		routeDefaultTransportTo(t, serverURL)
		client, err := NewGeminiClient("test-key")
		if err != nil {
			t.Fatal(err)
		}
		got, message := capturePromptCacheRequest(t, client, &CompletionRequest{
			Model: "gemini-2.5-flash", PromptCacheKey: promptKey, CacheSessionID: sessionID,
		}, captured)
		body := decodeCapturedBody(t, got.body)
		assertBodyKeysAbsent(t, body, "prompt_cache_key", "session_id", "cache_control", "cachedContent")
		assertNoResponseCacheHeaders(t, got.header)
		assertCacheMetadata(t, message, 9, 0)
	})
}

func TestStreamingProviderCacheUsageParsing(t *testing.T) {
	t.Run("OpenAI Responses", func(t *testing.T) {
		serverURL, captured := newNativeRequestCaptureServer(t, ""+
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n"+
			"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":20,\"output_tokens\":3,\"input_tokens_details\":{\"cached_tokens\":12,\"cache_write_tokens\":4}}}}\n\n"+
			"data: [DONE]\n\n")
		client := NewOpenAIClient("test-key", "")
		client.client = openai.NewClient("test-key", serverURL+"/v1")
		stream := true
		_, message := capturePromptCacheRequest(t, client, &CompletionRequest{
			Model: "gpt-5.4", Stream: &stream,
		}, captured)
		assertCacheMetadata(t, message, 12, 4)
	})

	t.Run("Anthropic", func(t *testing.T) {
		serverURL, captured := newNativeRequestCaptureServer(t, ""+
			"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":5,\"cache_creation_input_tokens\":7,\"cache_read_input_tokens\":11}}}\n\n"+
			"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n"+
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n"+
			"data: {\"type\":\"message_stop\"}\n\n")
		routeDefaultTransportTo(t, serverURL)
		stream := true
		_, message := capturePromptCacheRequest(t, NewAnthropicClient("test-key"), &CompletionRequest{
			Model: "claude-sonnet-4-6", Stream: &stream,
		}, captured)
		if got := message.GetInputTokens(); got != 23 {
			t.Fatalf("streaming normalized Anthropic input tokens = %d, want 23", got)
		}
		assertCacheMetadata(t, message, 11, 7)
	})
}

func capturePromptCacheRequest(t *testing.T, client LLM, req *CompletionRequest, captured <-chan capturedNativeRequest) (capturedNativeRequest, *messages.ChatMessage) {
	t.Helper()
	if req.Stream == nil {
		stream := false
		req.Stream = &stream
	}
	req.Timeout = 5 * time.Second
	if req.MaxTokens == 0 {
		req.MaxTokens = 128
	}
	if len(req.Messages) == 0 {
		req.Messages = messages.User("hello")
	}

	var complete *messages.ChatMessage
	for event := range client.ChatCompletionStream(context.Background(), req, messages.NewStreamProcessor()) {
		if event == nil {
			continue
		}
		switch event.Type {
		case messages.EventTypeError:
			t.Fatalf("completion failed: %v", event.Error)
		case messages.EventTypeComplete:
			complete = event.Message
		}
	}
	if complete == nil {
		t.Fatal("provider did not emit a complete message")
	}

	select {
	case got := <-captured:
		if got.err != nil {
			t.Fatalf("capture request: %v", got.err)
		}
		return got, complete
	case <-time.After(2 * time.Second):
		t.Fatal("provider did not reach capture server")
		return capturedNativeRequest{}, nil
	}
}

func decodeCapturedBody(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatalf("decode captured body %q: %v", data, err)
	}
	return body
}

func assertBodyKeysAbsent(t *testing.T, body map[string]any, keys ...string) {
	t.Helper()
	for _, key := range keys {
		if value, exists := body[key]; exists {
			t.Errorf("unsupported field %q leaked into request with value %#v", key, value)
		}
	}
}

func assertNoResponseCacheHeaders(t *testing.T, header http.Header) {
	t.Helper()
	for _, key := range []string{"Cache-Control", "X-OpenRouter-Cache"} {
		if values, exists := header[http.CanonicalHeaderKey(key)]; exists {
			t.Errorf("response-cache header %s was sent: %q", key, strings.Join(values, ","))
		}
	}
}

func assertCacheMetadata(t *testing.T, message *messages.ChatMessage, read, write int) {
	t.Helper()
	if got := message.GetCacheReadInputTokens(); got != read {
		t.Errorf("cache read tokens = %d, want %d; metadata=%#v", got, read, message.Metadata)
	}
	if got := message.GetCacheWriteInputTokens(); got != write {
		t.Errorf("cache write tokens = %d, want %d; metadata=%#v", got, write, message.Metadata)
	}
}
