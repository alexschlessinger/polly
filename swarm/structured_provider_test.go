package swarm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
)

// Exercise the actual OpenAI-compatible request encoder and response parser,
// including providers that do not enforce output schemas themselves.
func TestStructuredCompatibleProviderWire(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			http.Error(w, "bad request", 400)
			return
		}
		if _, found := request["response_format"]; found {
			t.Error("wire request constrained investigation")
		}
		found := false
		for _, entry := range request["tools"].([]any) {
			function := entry.(map[string]any)["function"].(map[string]any)
			if function["name"] == completionToolName {
				found = true
				params := function["parameters"].(map[string]any)
				if params["properties"].(map[string]any)["value"].(map[string]any)["type"] != "boolean" {
					t.Error("wire tool lost value schema")
				}
			}
		}
		if !found {
			t.Error("wire completion tool absent")
		}
		message := map[string]any{"role": "assistant", "content": "I have finished"}
		finish := "stop"
		if calls.Add(1) == 2 {
			message = map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "typed", "type": "function", "function": map[string]any{"name": completionToolName, "arguments": `{"value":true}`}}}}
			finish = "tool_calls"
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": message, "finish_reason": finish}}, "usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 2}})
	}))
	defer server.Close()
	r := runtimeTest(t, llm.NewOpenAIClient("fixture", server.URL), 1, 1)
	stream := false
	r.UpdateDefaults(llm.CompletionRequest{Model: "fixture", Stream: &stream}, llm.AgentConfig{MaxIterations: 4}, nil)
	result, err := r.Agent(context.Background(), "", AgentRequest{Task: "return true", ReadOnly: true, Schema: boolResultSchema})
	if err != nil || result.Value != true || calls.Load() != 2 {
		t.Fatalf("result=%+v error=%v requests=%d", result, err, calls.Load())
	}
}
