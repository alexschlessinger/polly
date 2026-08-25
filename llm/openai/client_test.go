package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
)

// newTestClient points a client at a test server.
func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return NewClient("test-key", server.URL)
}

func TestNormalizeBaseURL(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", "https://api.openai.com/v1/"},
		{"https://api.deepseek.com", "https://api.deepseek.com/"},
		{"https://openrouter.ai/api/v1", "https://openrouter.ai/api/v1/"},
		{"https://openrouter.ai/api/v1/", "https://openrouter.ai/api/v1/"},
		{" https://host/v1 ", "https://host/v1/"},
	}
	for _, tc := range tests {
		if got := normalizeBaseURL(tc.in); got != tc.want {
			t.Errorf("normalizeBaseURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestChatCompletionGoldenRequest pins the exact JSON sent to
// chat/completions for a request exercising every field polly sets: all four
// message roles (user as a content-part array with an image, assistant with
// tool calls and DeepSeek reasoning replay), tools, structured output, and
// sampling/reasoning knobs. With no SDK underneath, this test is the wire
// contract for every OpenAI-compatible provider.
func TestChatCompletionGoldenRequest(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &gotBody); err != nil {
			t.Errorf("request body is not valid JSON: %v", err)
		}
		w.Write([]byte(`{}`))
	})

	temp := 0.5
	maxTokens := int64(256)
	strict := true
	_, err := client.CreateChatCompletion(context.Background(), &ChatCompletionRequest{
		Model: "gpt-5.4",
		Messages: []ChatMessage{
			{Role: "system", Content: "be brief"},
			{Role: "user", Content: []ChatContentPart{
				{Type: "text", Text: "hi"},
				{Type: "image_url", ImageURL: &ChatImageURL{URL: "data:image/png;base64,AAA"}},
			}},
			{
				Role:             "assistant",
				ReasoningContent: "pondered",
				ToolCalls: []ChatToolCall{{
					ID: "call_1", Type: "function",
					Function: ChatToolCallFunc{Name: "lookup", Arguments: `{"q":"x"}`},
				}},
			},
			{Role: "tool", Content: "y", ToolCallID: "call_1"},
		},
		Temperature:         &temp,
		MaxCompletionTokens: &maxTokens,
		ReasoningEffort:     ReasoningEffortHigh,
		ResponseFormat: &ResponseFormat{
			Type: "json_schema",
			JSONSchema: &JSONSchemaSpec{
				Name:        "response",
				Description: "Structured response",
				Schema:      map[string]any{"type": "object"},
				Strict:      &strict,
			},
		},
		Tools: []ChatTool{{
			Type: "function",
			Function: FunctionDef{
				Name:        "lookup",
				Description: "finds things",
				Parameters:  map[string]any{"type": "object"},
			},
		}},
	})
	if err != nil {
		t.Fatalf("CreateChatCompletion: %v", err)
	}

	if gotPath != "/chat/completions" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("authorization = %q", gotAuth)
	}

	want := map[string]any{
		"model": "gpt-5.4",
		"messages": []any{
			map[string]any{"role": "system", "content": "be brief"},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "hi"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAA"}},
			}},
			map[string]any{"role": "assistant", "reasoning_content": "pondered", "tool_calls": []any{
				map[string]any{"id": "call_1", "type": "function",
					"function": map[string]any{"name": "lookup", "arguments": `{"q":"x"}`}},
			}},
			map[string]any{"role": "tool", "content": "y", "tool_call_id": "call_1"},
		},
		"temperature":           float64(0.5),
		"max_completion_tokens": float64(256),
		"reasoning_effort":      "high",
		"response_format": map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":        "response",
				"description": "Structured response",
				"schema":      map[string]any{"type": "object"},
				"strict":      true,
			},
		},
		"tools": []any{map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "lookup",
				"description": "finds things",
				"parameters":  map[string]any{"type": "object"},
			},
		}},
	}
	if !reflect.DeepEqual(gotBody, want) {
		gotJSON, _ := json.MarshalIndent(gotBody, "", "  ")
		wantJSON, _ := json.MarshalIndent(want, "", "  ")
		t.Errorf("request body mismatch\ngot:\n%s\nwant:\n%s", gotJSON, wantJSON)
	}
}

// TestResponsesGoldenRequest pins the Responses API request shape: all four
// input item kinds, flat text.format, reasoning, and an always-present
// strict flag on function tools. User messages must carry no "type" key.
func TestResponsesGoldenRequest(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &gotBody)
		w.Write([]byte(`{}`))
	})

	maxTokens := int64(512)
	strict := false
	toolOutput := "y"
	_, err := client.CreateResponse(context.Background(), &ResponsesRequest{
		Model:        "gpt-5.4",
		Instructions: "be brief",
		Input: []ResponseInputItem{
			{Role: "user", Content: []ResponseInputContent{
				{Type: "input_text", Text: "hi"},
				{Type: "input_image", Detail: "auto", ImageURL: "https://example.com/cat.png"},
			}},
			{Type: "message", Role: "assistant", ID: "msg_1", Status: "completed",
				Content: []ResponseOutputContent{{Type: "output_text", Text: "calling", Annotations: []any{}}}},
			{Type: "function_call", CallID: "call_1", Name: "lookup", Arguments: `{"q":"x"}`, Status: "completed"},
			{Type: "function_call_output", CallID: "call_1", Output: &toolOutput, Status: "completed"},
		},
		MaxOutputTokens: &maxTokens,
		Reasoning:       &ReasoningParam{Effort: ReasoningEffortHigh, Summary: "auto"},
		Text: &TextConfig{Format: &TextFormat{
			Type: "json_schema", Name: "response", Schema: map[string]any{"type": "object"}, Strict: &strict,
		}},
		Tools: []ResponsesTool{{
			Type: "function", Name: "lookup", Parameters: map[string]any{"type": "object"}, Strict: &strict,
		}},
	})
	if err != nil {
		t.Fatalf("CreateResponse: %v", err)
	}

	if gotPath != "/responses" {
		t.Errorf("path = %q", gotPath)
	}
	want := map[string]any{
		"model":        "gpt-5.4",
		"instructions": "be brief",
		"input": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "hi"},
				map[string]any{"type": "input_image", "detail": "auto", "image_url": "https://example.com/cat.png"},
			}},
			map[string]any{"type": "message", "role": "assistant", "id": "msg_1", "status": "completed",
				"content": []any{map[string]any{"type": "output_text", "text": "calling", "annotations": []any{}}}},
			map[string]any{"type": "function_call", "call_id": "call_1", "name": "lookup", "arguments": `{"q":"x"}`, "status": "completed"},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "y", "status": "completed"},
		},
		"max_output_tokens": float64(512),
		"reasoning":         map[string]any{"effort": "high", "summary": "auto"},
		"text": map[string]any{"format": map[string]any{
			"type": "json_schema", "name": "response", "schema": map[string]any{"type": "object"}, "strict": false,
		}},
		"tools": []any{map[string]any{
			"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}, "strict": false,
		}},
	}
	if !reflect.DeepEqual(gotBody, want) {
		gotJSON, _ := json.MarshalIndent(gotBody, "", "  ")
		wantJSON, _ := json.MarshalIndent(want, "", "  ")
		t.Errorf("request body mismatch\ngot:\n%s\nwant:\n%s", gotJSON, wantJSON)
	}
}

// TestStreamChatCompletion verifies the Chat Completions SSE grammar with
// compatible-server quirks: comment keep-alives, tool-call deltas with a
// missing index (tolerated as 0), a usage-only final chunk with empty
// choices, and the [DONE] sentinel. It also checks stream:true and
// stream_options are added while the caller's request stays unmutated.
func TestStreamChatCompletion(t *testing.T) {
	var gotBody map[string]any
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &gotBody)
		w.Write([]byte(
			": OPENROUTER PROCESSING\n\n" +
				"data: {\"choices\":[{\"delta\":{\"content\":\"hel\"}}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"id\":\"call_1\",\"function\":{\"name\":\"f\",\"arguments\":\"{\\\"a\\\":\"}}]}}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"function\":{\"arguments\":\"1}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n" +
				"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":9}}\n\n" +
				"data: [DONE]\n\n"))
	})

	req := &ChatCompletionRequest{Model: "m"}
	var chunks []*ChatCompletionChunk
	for chunk, err := range client.StreamChatCompletion(context.Background(), req) {
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		chunks = append(chunks, chunk)
	}

	if gotBody["stream"] != true {
		t.Errorf("stream = %v, want true", gotBody["stream"])
	}
	if !reflect.DeepEqual(gotBody["stream_options"], map[string]any{"include_usage": true}) {
		t.Errorf("stream_options = %v", gotBody["stream_options"])
	}
	if req.Stream || req.StreamOptions != nil {
		t.Error("caller's request was mutated")
	}

	if len(chunks) != 4 {
		t.Fatalf("got %d chunks, want 4", len(chunks))
	}
	if chunks[0].Choices[0].Delta.Content != "hel" {
		t.Errorf("first delta = %+v", chunks[0].Choices[0].Delta)
	}
	tc := chunks[1].Choices[0].Delta.ToolCalls[0]
	if tc.Index != 0 || tc.ID != "call_1" || tc.Function.Name != "f" {
		t.Errorf("tool call delta = %+v", tc)
	}
	if chunks[2].Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish reason = %q", chunks[2].Choices[0].FinishReason)
	}
	final := chunks[3]
	if len(final.Choices) != 0 || final.Usage == nil || final.Usage.PromptTokens != 5 || final.Usage.CompletionTokens != 9 {
		t.Errorf("usage chunk = %+v", final)
	}
	if chunks[0].Usage != nil {
		t.Error("non-final chunk should have nil usage")
	}
}

// TestStreamChatCompletionMidStreamError verifies that an error envelope
// sent as a data payload — how OpenRouter and other compatible servers
// report mid-stream failures — surfaces as an error instead of being
// silently dropped as an empty chunk.
func TestStreamChatCompletionMidStreamError(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(
			"data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n" +
				"data: {\"error\":{\"message\":\"quota exceeded\",\"code\":402}}\n\n"))
	})

	var texts []string
	var streamErr error
	for chunk, err := range client.StreamChatCompletion(context.Background(), &ChatCompletionRequest{}) {
		if err != nil {
			streamErr = err
			break
		}
		texts = append(texts, chunk.Choices[0].Delta.Content)
	}

	if len(texts) != 1 || texts[0] != "partial" {
		t.Errorf("texts before error = %v", texts)
	}
	var apiErr *APIError
	if !errors.As(streamErr, &apiErr) {
		t.Fatalf("stream error = %v, want *APIError", streamErr)
	}
	if apiErr.Message != "quota exceeded" {
		t.Errorf("apiErr = %+v", apiErr)
	}
}

// TestStreamResponse verifies Responses SSE parsing: event:-framed events,
// unknown types passed through, and error events delivered as ordinary
// events (the adapter decides how to surface them), not stream failures.
func TestStreamResponse(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(
			"event: response.output_text.delta\n" +
				"data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"hi\"}\n\n" +
				"event: response.brand_new_event\n" +
				"data: {\"type\":\"response.brand_new_event\",\"delta\":{\"weird\":\"object\"}}\n\n" +
				"event: response.output_item.done\n" +
				"data: {\"type\":\"response.output_item.done\",\"output_index\":1,\"item\":{\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"f\",\"arguments\":\"{}\"}}\n\n" +
				"event: error\n" +
				"data: {\"type\":\"error\",\"code\":\"server_error\",\"message\":\"boom\"}\n\n" +
				"event: response.completed\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":4}}}\n\n"))
	})

	var events []*ResponseStreamEvent
	for event, err := range client.StreamResponse(context.Background(), &ResponsesRequest{}) {
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		events = append(events, event)
	}

	if len(events) != 5 {
		t.Fatalf("got %d events, want 5", len(events))
	}
	if events[0].Delta != "hi" {
		t.Errorf("delta = %q", events[0].Delta)
	}
	if events[1].Type != "response.brand_new_event" || events[1].Delta != "" {
		t.Errorf("unknown event = %+v (object delta must decode to empty, not fail)", events[1])
	}
	if item := events[2].Item; item == nil || item.CallID != "call_1" || item.Name != "f" {
		t.Errorf("output item = %+v", events[2].Item)
	}
	if events[3].Type != "error" || events[3].Code != "server_error" || events[3].Message != "boom" {
		t.Errorf("error event = %+v", events[3])
	}
	resp := events[4].Response
	if resp == nil || resp.Status != ResponseStatusCompleted || resp.Usage.InputTokens != 3 {
		t.Errorf("completed event = %+v", resp)
	}
}

// TestErrorEnvelopeAndRetry verifies non-2xx handling: retryable statuses
// are retried (honoring Retry-After-Ms) before succeeding, while 4xx client
// errors fail fast with the parsed envelope.
func TestErrorEnvelopeAndRetry(t *testing.T) {
	var calls atomic.Int32
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 {
			w.Header().Set("Retry-After-Ms", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"message":"slow down","type":"rate_limit_error"}}`))
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	})

	resp, err := client.CreateChatCompletion(context.Background(), &ChatCompletionRequest{})
	if err != nil {
		t.Fatalf("CreateChatCompletion after retries: %v", err)
	}
	if resp.Choices[0].Message.Content != "ok" || calls.Load() != 3 {
		t.Errorf("resp = %+v after %d calls", resp, calls.Load())
	}

	var badCalls atomic.Int32
	bad := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		badCalls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":"unknown model","type":"invalid_request_error","code":"model_not_found"}}`))
	})
	_, err = bad.CreateChatCompletion(context.Background(), &ChatCompletionRequest{})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.StatusCode != 400 || apiErr.Type != "invalid_request_error" || apiErr.Code != "model_not_found" {
		t.Errorf("apiErr = %+v", apiErr)
	}
	if badCalls.Load() != 1 {
		t.Errorf("400 was retried: %d calls", badCalls.Load())
	}
}

// TestKeylessRequestOmitsAuthorization matches the official SDK: no
// Authorization header at all when the key is empty (keyless compatible
// servers reject malformed empty bearers).
func TestKeylessRequestOmitsAuthorization(t *testing.T) {
	var sawAuthHeader bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, sawAuthHeader = r.Header["Authorization"]
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)

	client := NewClient("", server.URL)
	if _, err := client.CreateChatCompletion(context.Background(), &ChatCompletionRequest{}); err != nil {
		t.Fatalf("CreateChatCompletion: %v", err)
	}
	if sawAuthHeader {
		t.Error("keyless request must not send an Authorization header")
	}
}

// TestCreateEmbeddingsGoldenRequest pins the embeddings body and response
// decoding.
func TestCreateEmbeddingsGoldenRequest(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &gotBody)
		w.Write([]byte(`{"model":"text-embedding-3-large","data":[{"embedding":[0.1,0.2]}],"usage":{"prompt_tokens":2,"total_tokens":2}}`))
	})

	dim := int64(64)
	resp, err := client.CreateEmbeddings(context.Background(), &EmbeddingRequest{
		Model:      "text-embedding-3-large",
		Input:      []string{"a", "b"},
		Dimensions: &dim,
	})
	if err != nil {
		t.Fatalf("CreateEmbeddings: %v", err)
	}

	if gotPath != "/embeddings" {
		t.Errorf("path = %q", gotPath)
	}
	want := map[string]any{
		"model":      "text-embedding-3-large",
		"input":      []any{"a", "b"},
		"dimensions": float64(64),
	}
	if !reflect.DeepEqual(gotBody, want) {
		t.Errorf("body = %v, want %v", gotBody, want)
	}
	if len(resp.Data) != 1 || resp.Data[0].Embedding[1] != 0.2 || resp.Usage.TotalTokens != 2 {
		t.Errorf("resp = %+v", resp)
	}
}
