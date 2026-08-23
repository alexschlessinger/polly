package ollama

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	u, _ := url.Parse(server.URL)
	return NewClient(u, nil)
}

// TestChatGoldenRequest pins the exact JSON sent to /api/chat for a request
// exercising every field polly sets: system/user/assistant/tool turns with
// images and tool calls, sampler options, thinking, format, and tools. With
// no SDK underneath, this test is the wire contract.
func TestChatGoldenRequest(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &gotBody); err != nil {
			t.Errorf("request body is not valid JSON: %v", err)
		}
		w.Write([]byte(`{"done":true}`))
	})

	stream := false
	err := client.Chat(context.Background(), &ChatRequest{
		Model: "llama3",
		Messages: []Message{
			{Role: "system", Content: "be brief"},
			{Role: "user", Content: "hi", Images: []ImageData{{1, 2}}},
			{Role: "assistant", ToolCalls: []ToolCall{{
				Function: ToolCallFunction{Name: "lookup", Arguments: map[string]any{"q": "x"}},
			}}},
			{Role: "tool", Content: "y", ToolName: "lookup"},
		},
		Stream:  &stream,
		Format:  json.RawMessage(`"json"`),
		Options: map[string]any{"num_predict": 100, "temperature": 0.5},
		Think:   true,
		Tools: []Tool{{
			Type: "function",
			Function: ToolFunction{
				Name:        "lookup",
				Description: "finds things",
				Parameters: ToolParameters{
					Type:       "object",
					Required:   []string{"q"},
					Properties: map[string]any{"q": map[string]any{"type": "string"}},
				},
			},
		}},
	}, func(ChatResponse) error { return nil })
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	if gotPath != "/api/chat" {
		t.Errorf("path = %q", gotPath)
	}
	want := map[string]any{
		"model": "llama3",
		"messages": []any{
			map[string]any{"role": "system", "content": "be brief"},
			map[string]any{"role": "user", "content": "hi", "images": []any{"AQI="}},
			map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
				map[string]any{"function": map[string]any{
					"index": float64(0), "name": "lookup", "arguments": map[string]any{"q": "x"},
				}},
			}},
			map[string]any{"role": "tool", "content": "y", "tool_name": "lookup"},
		},
		"stream":  false,
		"format":  "json",
		"options": map[string]any{"num_predict": float64(100), "temperature": 0.5},
		"think":   true,
		"tools": []any{map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "lookup",
				"description": "finds things",
				"parameters": map[string]any{
					"type":       "object",
					"required":   []any{"q"},
					"properties": map[string]any{"q": map[string]any{"type": "string"}},
				},
			},
		}},
	}
	if !reflect.DeepEqual(gotBody, want) {
		gotJSON, _ := json.MarshalIndent(gotBody, "", "  ")
		wantJSON, _ := json.MarshalIndent(want, "", "  ")
		t.Errorf("request body mismatch\ngot:\n%s\nwant:\n%s", gotJSON, wantJSON)
	}
}

// TestChatStreaming verifies NDJSON parsing: incremental chunks, thinking,
// tool calls with object arguments, and token counts on the final line.
func TestChatStreaming(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(
			`{"message":{"role":"assistant","thinking":"hmm"},"done":false}` + "\n" +
				`{"message":{"role":"assistant","content":"hi"},"done":false}` + "\n" +
				`{"message":{"role":"assistant","tool_calls":[{"function":{"name":"f","arguments":{"a":1}}}]},"done":false}` + "\n" +
				`{"message":{"role":"assistant"},"done":true,"done_reason":"stop","prompt_eval_count":5,"eval_count":9}` + "\n"))
	})

	var chunks []ChatResponse
	err := client.Chat(context.Background(), &ChatRequest{Model: "m"}, func(resp ChatResponse) error {
		chunks = append(chunks, resp)
		return nil
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	if len(chunks) != 4 {
		t.Fatalf("got %d chunks, want 4", len(chunks))
	}
	if chunks[0].Message.Thinking != "hmm" || chunks[1].Message.Content != "hi" {
		t.Errorf("chunks = %+v", chunks[:2])
	}
	tc := chunks[2].Message.ToolCalls[0].Function
	if tc.Name != "f" || !reflect.DeepEqual(tc.Arguments, map[string]any{"a": float64(1)}) {
		t.Errorf("tool call = %+v", tc)
	}
	final := chunks[3]
	if !final.Done || final.DoneReason != "stop" || final.PromptEvalCount != 5 || final.EvalCount != 9 {
		t.Errorf("final chunk = %+v", final)
	}
}

// TestChatErrors verifies non-2xx envelopes, mid-stream error lines, and
// callback errors stopping the stream.
func TestChatErrors(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"model 'nope' not found"}`))
	})
	err := client.Chat(context.Background(), &ChatRequest{Model: "nope"}, func(ChatResponse) error { return nil })
	var statusErr StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("err = %v, want StatusError", err)
	}
	if statusErr.StatusCode != 404 || statusErr.ErrorMessage != "model 'nope' not found" {
		t.Errorf("statusErr = %+v", statusErr)
	}

	midStream := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(
			`{"message":{"role":"assistant","content":"par"},"done":false}` + "\n" +
				`{"error":"out of memory"}` + "\n"))
	})
	var n int
	err = midStream.Chat(context.Background(), &ChatRequest{Model: "m"}, func(ChatResponse) error {
		n++
		return nil
	})
	if !errors.As(err, &statusErr) || statusErr.ErrorMessage != "out of memory" {
		t.Fatalf("mid-stream err = %v, want StatusError", err)
	}
	if n != 1 {
		t.Errorf("chunks before error = %d, want 1", n)
	}

	stopEarly := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"message":{"role":"assistant","content":"x"},"done":false}` + "\n"))
	})
	sentinel := errors.New("stop")
	if err := stopEarly.Chat(context.Background(), &ChatRequest{Model: "m"}, func(ChatResponse) error {
		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Errorf("callback error = %v, want sentinel", err)
	}
}
