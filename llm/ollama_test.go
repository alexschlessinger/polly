package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
)

// TestOllamaDefaultsToStreaming: a nil CompletionRequest.Stream means
// streaming, per the interface contract; the outgoing Ollama request must say
// stream:true and the stream must still complete with the full content.
func TestOllamaDefaultsToStreaming(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &gotBody)
		w.Write([]byte(
			`{"message":{"role":"assistant","content":"hi"},"done":false}` + "\n" +
				`{"message":{"role":"assistant","content":""},"done":true,"prompt_eval_count":1,"eval_count":2}` + "\n"))
	}))
	t.Cleanup(server.Close)

	client := NewOllamaClient(server.URL, "")
	events := client.ChatCompletionStream(context.Background(), &CompletionRequest{
		Model:     "test-model",
		Messages:  messages.User("hello"),
		MaxTokens: 16,
	}, &SimpleProcessor{})

	var complete *messages.ChatMessage
	for event := range events {
		switch event.Type {
		case messages.EventTypeError:
			t.Fatalf("stream error: %v", event.Error)
		case messages.EventTypeComplete:
			complete = event.Message
		}
	}

	if gotBody["stream"] != true {
		t.Errorf("request stream = %v, want true", gotBody["stream"])
	}
	if complete == nil || complete.Content != "hi" {
		t.Fatalf("complete = %#v, want content %q", complete, "hi")
	}
}

func collectOllamaStream(t *testing.T, server *httptest.Server, req *CompletionRequest) (complete *messages.ChatMessage, reasoning []string) {
	t.Helper()
	client := NewOllamaClient(server.URL, "")
	// The real processor: SimpleProcessor drops reasoning events.
	for event := range client.ChatCompletionStream(context.Background(), req, messages.NewStreamProcessor()) {
		switch event.Type {
		case messages.EventTypeError:
			t.Fatalf("stream error: %v", event.Error)
		case messages.EventTypeReasoning:
			reasoning = append(reasoning, event.Content)
		case messages.EventTypeComplete:
			complete = event.Message
		}
	}
	if complete == nil {
		t.Fatal("stream ended without a complete event")
	}
	return complete, reasoning
}

// TestOllamaThinkingWithoutThinkingChunksKeepsContent: with thinking on, a
// model that answers without ever emitting a thinking field must not be
// silenced. Content seen before any thinking chunk is held and flushed at
// the end.
func TestOllamaThinkingWithoutThinkingChunksKeepsContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(
			`{"message":{"role":"assistant","content":"hi "},"done":false}` + "\n" +
				`{"message":{"role":"assistant","content":"there"},"done":false}` + "\n" +
				`{"message":{"role":"assistant","content":""},"done":true,"prompt_eval_count":1,"eval_count":2}` + "\n"))
	}))
	t.Cleanup(server.Close)

	complete, _ := collectOllamaStream(t, server, &CompletionRequest{
		Model:          "test-model",
		Messages:       messages.User("say hi"),
		MaxTokens:      16,
		ThinkingEffort: EffortLevel(LevelHigh),
	})
	if complete.Content != "hi there" {
		t.Fatalf("content = %q, want %q", complete.Content, "hi there")
	}
}

// TestOllamaThinkingDropsContentRepeatedAfterThinking: content that arrives
// before the thinking chunk is the duplicate some models emit; only what
// follows thinking is the answer.
func TestOllamaThinkingDropsContentRepeatedAfterThinking(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(
			`{"message":{"role":"assistant","content":"early"},"done":false}` + "\n" +
				`{"message":{"role":"assistant","thinking":"hmm"},"done":false}` + "\n" +
				`{"message":{"role":"assistant","content":"answer"},"done":false}` + "\n" +
				`{"message":{"role":"assistant","content":""},"done":true,"prompt_eval_count":1,"eval_count":2}` + "\n"))
	}))
	t.Cleanup(server.Close)

	complete, reasoning := collectOllamaStream(t, server, &CompletionRequest{
		Model:          "test-model",
		Messages:       messages.User("think"),
		MaxTokens:      16,
		ThinkingEffort: EffortLevel(LevelHigh),
	})
	if complete.Content != "answer" {
		t.Fatalf("content = %q, want %q", complete.Content, "answer")
	}
	if len(reasoning) != 1 || reasoning[0] != "hmm" {
		t.Fatalf("reasoning = %q, want [hmm]", reasoning)
	}
}

// TestOllamaStreamedToolCallsAccumulate: Ollama emits each parsed tool call
// in its own chunk. The completed message must carry all of them, numbered
// across the stream.
func TestOllamaStreamedToolCallsAccumulate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(
			`{"message":{"role":"assistant","tool_calls":[{"function":{"name":"a","arguments":{"x":1}}}]},"done":false}` + "\n" +
				`{"message":{"role":"assistant","tool_calls":[{"function":{"name":"b","arguments":{"y":2}}}]},"done":false}` + "\n" +
				`{"message":{"role":"assistant","content":""},"done":true,"prompt_eval_count":1,"eval_count":2}` + "\n"))
	}))
	t.Cleanup(server.Close)

	complete, _ := collectOllamaStream(t, server, &CompletionRequest{
		Model:     "test-model",
		Messages:  messages.User("call both"),
		MaxTokens: 16,
	})
	if complete.StopReason != messages.StopReasonToolUse {
		t.Fatalf("stop reason = %q, want %q", complete.StopReason, messages.StopReasonToolUse)
	}
	if len(complete.ToolCalls) != 2 {
		t.Fatalf("tool calls = %+v, want 2", complete.ToolCalls)
	}
	if complete.ToolCalls[0].Name != "a" || complete.ToolCalls[1].Name != "b" {
		t.Fatalf("tool call order = %q, %q", complete.ToolCalls[0].Name, complete.ToolCalls[1].Name)
	}
	if complete.ToolCalls[0].ID == complete.ToolCalls[1].ID {
		t.Fatalf("synthetic IDs collide: %q", complete.ToolCalls[0].ID)
	}
}

// TestOllamaSendsSchemaAsFormat: a response schema goes to the server as the
// format object itself, so decoding is constrained to it rather than to
// "any JSON".
func TestOllamaSendsSchemaAsFormat(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &gotBody)
		w.Write([]byte(
			`{"message":{"role":"assistant","content":"{\"name\":\"x\"}"},"done":false}` + "\n" +
				`{"message":{"role":"assistant","content":""},"done":true}` + "\n"))
	}))
	t.Cleanup(server.Close)

	schema := &Schema{Raw: map[string]any{
		"type":       "object",
		"required":   []any{"name"},
		"properties": map[string]any{"name": map[string]any{"type": "string"}},
	}}
	complete, _ := collectOllamaStream(t, server, &CompletionRequest{
		Model:          "test-model",
		Messages:       messages.User("name something"),
		MaxTokens:      16,
		ResponseSchema: schema,
	})
	if complete.Content != `{"name":"x"}` {
		t.Fatalf("content = %q", complete.Content)
	}
	format, ok := gotBody["format"].(map[string]any)
	if !ok {
		t.Fatalf("format = %#v, want the schema object", gotBody["format"])
	}
	if format["type"] != "object" {
		t.Fatalf("format type = %v", format["type"])
	}
	props, _ := format["properties"].(map[string]any)
	if _, ok := props["name"]; !ok {
		t.Fatalf("format lacks the schema's properties: %#v", format)
	}
	// The prompt still describes the schema for the model.
	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("no messages sent")
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" || !strings.Contains(first["content"].(string), `"name"`) {
		t.Fatalf("first message = %#v, want a system prompt describing the schema", first)
	}
}
