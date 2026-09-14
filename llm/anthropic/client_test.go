package anthropic

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
	client := NewClient("test-key")
	client.baseURL = server.URL
	return client
}

// TestCreateMessageGoldenRequest pins the exact JSON sent to the API for a
// request exercising every field polly sets: system and tools at the top
// level, adaptive thinking with output_config, forced tool_choice, and a
// history with image, thinking-replay, tool_use, and tool_result blocks.
// With no SDK underneath, this test is the wire contract.
func TestCreateMessageGoldenRequest(t *testing.T) {
	var gotPath, gotKey, gotVersion, gotContentType string
	var gotBody map[string]any
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		gotContentType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &gotBody); err != nil {
			t.Errorf("request body is not valid JSON: %v", err)
		}
		w.Write([]byte(`{}`))
	})

	temp := 0.5
	isError := false
	_, err := client.CreateMessage(context.Background(), &MessageRequest{
		Model:     "claude-opus-4-7",
		MaxTokens: 100,
		Messages: []MessageParam{
			{Role: "user", Content: []*ContentBlock{
				{Type: "text", Text: "hi"},
				{Type: "image", Source: &ImageSource{Type: "base64", MediaType: "image/png", Data: "AQI="}},
			}},
			{Role: "assistant", Content: []*ContentBlock{
				{Type: "thinking", Thinking: "pondering", Signature: "sig"},
				{Type: "tool_use", ID: "toolu_1", Name: "lookup", Input: json.RawMessage(`{"q":"x"}`)},
			}},
			{Role: "user", Content: []*ContentBlock{
				{Type: "tool_result", ToolUseID: "toolu_1",
					Content: []*ContentBlock{{Type: "text", Text: "y"}}, IsError: &isError},
			}},
		},
		System:       []*ContentBlock{{Type: "text", Text: "be brief"}},
		Temperature:  &temp,
		Thinking:     &ThinkingConfig{Type: ThinkingTypeAdaptive, Display: DisplaySummarized},
		OutputConfig: &OutputConfig{Effort: EffortHigh},
		Tools: []*Tool{{
			Name:        "lookup",
			Description: "finds things",
			InputSchema: InputSchema{Type: "object", Properties: map[string]any{"q": map[string]any{"type": "string"}}, Required: []string{"q"}},
		}},
		ToolChoice: &ToolChoice{Type: "any"},
	})
	if err != nil {
		t.Fatalf("CreateMessage: %v", err)
	}

	if gotPath != "/messages" {
		t.Errorf("path = %q", gotPath)
	}
	if gotKey != "test-key" || gotVersion != "2023-06-01" || gotContentType != "application/json" {
		t.Errorf("headers: key=%q version=%q content-type=%q", gotKey, gotVersion, gotContentType)
	}

	want := map[string]any{
		"model":      "claude-opus-4-7",
		"max_tokens": float64(100),
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "hi"},
				map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "AQI="}},
			}},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "thinking", "thinking": "pondering", "signature": "sig"},
				map[string]any{"type": "tool_use", "id": "toolu_1", "name": "lookup", "input": map[string]any{"q": "x"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_1",
					"content": []any{map[string]any{"type": "text", "text": "y"}}, "is_error": false},
			}},
		},
		"system":        []any{map[string]any{"type": "text", "text": "be brief"}},
		"temperature":   float64(0.5),
		"thinking":      map[string]any{"type": "adaptive", "display": "summarized"},
		"output_config": map[string]any{"effort": "high"},
		"tools": []any{map[string]any{
			"name":        "lookup",
			"description": "finds things",
			"input_schema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"q": map[string]any{"type": "string"}},
				"required":   []any{"q"},
			},
		}},
		"tool_choice": map[string]any{"type": "any"},
	}
	if !reflect.DeepEqual(gotBody, want) {
		gotJSON, _ := json.MarshalIndent(gotBody, "", "  ")
		wantJSON, _ := json.MarshalIndent(want, "", "  ")
		t.Errorf("request body mismatch\ngot:\n%s\nwant:\n%s", gotJSON, wantJSON)
	}
}

// TestLegacyThinkingRequest pins the enabled/budget_tokens form and that
// streaming requests carry stream:true while non-streaming ones omit it.
func TestLegacyThinkingRequest(t *testing.T) {
	var bodies []map[string]any
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		json.Unmarshal(body, &m)
		bodies = append(bodies, m)
		w.Write([]byte(`{}`))
	})

	req := &MessageRequest{
		Model:     "claude-sonnet-4-5",
		MaxTokens: 2048,
		Messages:  []MessageParam{{Role: "user", Content: []*ContentBlock{{Type: "text", Text: "hi"}}}},
		Thinking:  &ThinkingConfig{Type: ThinkingTypeEnabled, BudgetTokens: 1024},
	}
	if _, err := client.CreateMessage(context.Background(), req); err != nil {
		t.Fatalf("CreateMessage: %v", err)
	}
	for range client.CreateMessageStream(context.Background(), req) {
	}

	if got := bodies[0]["thinking"]; !reflect.DeepEqual(got, map[string]any{"type": "enabled", "budget_tokens": float64(1024)}) {
		t.Errorf("thinking = %v", got)
	}
	if _, ok := bodies[0]["stream"]; ok {
		t.Error("non-streaming request must not set stream")
	}
	if got := bodies[1]["stream"]; got != true {
		t.Errorf("streaming request stream = %v, want true", got)
	}
	if req.Stream {
		t.Error("caller's request was mutated")
	}
}

// TestCreateMessageStream verifies SSE parsing across the full event
// grammar polly consumes: message_start usage, thinking + signature deltas,
// tool_use with input_json deltas, message_delta stop_reason/usage, and
// ping/comment noise ignored.
func TestCreateMessageStream(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(
			"event: message_start\n" +
				"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"usage\":{\"input_tokens\":7}}}\n\n" +
				": keep-alive\n" +
				"event: ping\ndata: {\"type\":\"ping\"}\n\n" +
				"event: content_block_start\n" +
				"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n" +
				"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"hmm\"}}\n\n" +
				"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"sig\"}}\n\n" +
				"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
				"data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"f\",\"input\":{}}}\n\n" +
				"data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"a\\\":\"}}\n\n" +
				"data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"1}\"}}\n\n" +
				"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":42}}\n\n" +
				"data: {\"type\":\"message_stop\"}\n\n"))
	})

	var events []*StreamEvent
	for event, err := range client.CreateMessageStream(context.Background(), &MessageRequest{}) {
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		events = append(events, event)
	}

	if len(events) != 11 {
		t.Fatalf("got %d events, want 11", len(events))
	}
	if events[0].Type != EventMessageStart || events[0].Message.Usage.InputTokens != 7 {
		t.Errorf("message_start = %+v", events[0])
	}
	if events[2].ContentBlock.Type != "thinking" {
		t.Errorf("thinking block start = %+v", events[2].ContentBlock)
	}
	if events[3].Delta.Thinking != "hmm" || events[4].Delta.Signature != "sig" {
		t.Errorf("thinking deltas = %+v, %+v", events[3].Delta, events[4].Delta)
	}
	if cb := events[6].ContentBlock; cb.Type != "tool_use" || cb.ID != "toolu_1" || cb.Name != "f" {
		t.Errorf("tool_use start = %+v", cb)
	}
	if events[7].Delta.PartialJSON+events[8].Delta.PartialJSON != `{"a":1}` {
		t.Errorf("input_json deltas = %+v, %+v", events[7].Delta, events[8].Delta)
	}
	if d := events[9]; d.Delta.StopReason != StopReasonToolUse || d.Usage.OutputTokens != 42 {
		t.Errorf("message_delta = %+v", d)
	}
}

// TestStreamErrorEvent verifies that a mid-stream error event surfaces as
// *APIError, which is how the API reports failures after streaming begins.
func TestStreamErrorEvent(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(
			"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n" +
				"event: error\n" +
				"data: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"))
	})

	var n int
	var streamErr error
	for _, err := range client.CreateMessageStream(context.Background(), &MessageRequest{}) {
		if err != nil {
			streamErr = err
			break
		}
		n++
	}

	if n != 1 {
		t.Errorf("events before error = %d, want 1", n)
	}
	var apiErr *APIError
	if !errors.As(streamErr, &apiErr) {
		t.Fatalf("stream error = %v, want *APIError", streamErr)
	}
	if apiErr.Type != "overloaded_error" || apiErr.Message != "Overloaded" {
		t.Errorf("apiErr = %+v", apiErr)
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
			w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))
			return
		}
		w.Write([]byte(`{"id":"msg_1","stop_reason":"end_turn"}`))
	})

	resp, err := client.CreateMessage(context.Background(), &MessageRequest{})
	if err != nil {
		t.Fatalf("CreateMessage after retries: %v", err)
	}
	if resp.StopReason != StopReasonEndTurn || calls.Load() != 3 {
		t.Errorf("resp = %+v after %d calls", resp, calls.Load())
	}

	var badCalls atomic.Int32
	bad := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		badCalls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"bad thinking config"}}`))
	})
	_, err = bad.CreateMessage(context.Background(), &MessageRequest{})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.StatusCode != 400 || apiErr.Type != "invalid_request_error" || apiErr.Message != "bad thinking config" {
		t.Errorf("apiErr = %+v", apiErr)
	}
	if badCalls.Load() != 1 {
		t.Errorf("400 was retried: %d calls", badCalls.Load())
	}
}
