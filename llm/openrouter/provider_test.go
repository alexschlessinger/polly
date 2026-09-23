package openrouter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm/internal/contract"
	"github.com/alexschlessinger/pollytool/messages"
)

// sameJSON compares got (a decoded value or raw JSON) with want structurally,
// since decoded maps re-marshal with sorted keys.
func sameJSON(t *testing.T, got any, want string) {
	t.Helper()
	raw, ok := got.(json.RawMessage)
	if !ok {
		raw, _ = json.Marshal(got)
	}
	var g, w any
	if json.Unmarshal(raw, &g) != nil || json.Unmarshal([]byte(want), &w) != nil || !reflect.DeepEqual(g, w) {
		t.Fatalf("got %s, want %s", raw, want)
	}
}

func complete(t *testing.T, p *Provider, req *contract.CompletionRequest) *messages.ChatMessage {
	t.Helper()
	var final *messages.ChatMessage
	for event := range p.ChatCompletionStream(context.Background(), req, messages.NewStreamProcessor()) {
		if event.Type == messages.EventTypeError {
			t.Fatalf("stream error: %v", event.Error)
		}
		if event.Type == messages.EventTypeComplete {
			final = event.Message
		}
	}
	if final == nil {
		t.Fatal("no final message")
	}
	return final
}

func TestChatRequestCarriesGatewayExtensions(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/v1/chat/completions" || json.Unmarshal(raw, &body) != nil {
			t.Errorf("request %s: %s", r.URL.Path, raw)
		}
		fmt.Fprint(w, `{"id":"gen-1","model":"org/m","provider":"Upstream","choices":[{"message":{"content":"ok","reasoning_details":[{"type":"reasoning.text","text":"why"}]},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()
	p := NewProvider("key", server.URL+"/v1")
	streamMode := contract.Buffered
	first := complete(t, p, &contract.CompletionRequest{
		Model: "org/m", StreamMode: streamMode, ModelHost: "host/route", CacheSessionID: "sess",
		ThinkingEffort: contract.EffortLevel(contract.LevelHigh), Capabilities: &contract.ModelCapabilities{},
		Messages: messages.User("hi"),
	})
	if body["session_id"] != "sess" || body["reasoning_effort"] != nil {
		t.Fatalf("extensions: %v", body)
	}
	if reasoning, _ := body["reasoning"].(map[string]any); reasoning["effort"] != "high" {
		t.Fatalf("reasoning: %v", body["reasoning"])
	}
	if provider, _ := body["provider"].(map[string]any); provider["allow_fallbacks"] != false || fmt.Sprint(provider["only"]) != "[host/route]" {
		t.Fatalf("routing: %v", body["provider"])
	}
	meta, _ := first.Metadata[MetadataKey].(map[string]any)
	if meta["endpoint"] != Endpoint(server.URL+"/v1") || meta["requested_model"] != "org/m" || meta["provider"] != "Upstream" {
		t.Fatalf("attribution: %v", meta)
	}

	history := append(messages.User("hi"), *first)
	history = append(history, messages.User("more")...)
	complete(t, p, &contract.CompletionRequest{Model: "org/m", StreamMode: streamMode, Messages: history, Capabilities: &contract.ModelCapabilities{}})
	msgs, _ := body["messages"].([]any)
	assistant, _ := msgs[1].(map[string]any)
	sameJSON(t, assistant["reasoning_details"], `[{"type":"reasoning.text","text":"why"}]`)
	if assistant["reasoning"] != nil {
		t.Fatalf("plain reasoning sent alongside details: %v", assistant["reasoning"])
	}
}

func responsesStream(events ...string) string {
	var b strings.Builder
	for _, event := range events {
		fmt.Fprintf(&b, "data: %s\n\n", event)
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func TestResponsesRoundTripReplaysReasoningItemsVerbatim(t *testing.T) {
	item := `{"type":"reasoning","id":"rs_1","status":"completed","summary":[],"content":[{"type":"reasoning_text","text":"because"}],"signature":"sig-1","format":"anthropic-claude-v1"}`
	for _, streamMode := range []contract.StreamMode{contract.Streaming, contract.Buffered} {
		streamed := streamMode == contract.Streaming
		t.Run(fmt.Sprint("stream=", streamed), func(t *testing.T) {
			var bodies []map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				var body map[string]any
				if r.URL.Path != "/v1/responses" || json.Unmarshal(raw, &body) != nil {
					t.Errorf("request %s: %s", r.URL.Path, raw)
				}
				bodies = append(bodies, body)
				response := `{"id":"resp_1","model":"org/m","status":"completed","output":[` + item + `,{"type":"message","id":"msg_1","status":"completed","content":[{"type":"output_text","text":"answer"}]}],"usage":{"input_tokens":3,"output_tokens":2}}`
				if !streamed {
					fmt.Fprint(w, response)
					return
				}
				fmt.Fprint(w, responsesStream(
					`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}`,
					`{"type":"response.reasoning_text.delta","output_index":0,"delta":"because"}`,
					`{"type":"response.output_item.done","output_index":0,"item":`+item+`}`,
					`{"type":"response.output_text.delta","output_index":1,"delta":"answer"}`,
					`{"type":"response.completed","response":`+response+`}`,
				))
			}))
			defer server.Close()
			p := NewProvider("key", server.URL+"/v1", WithAPI(ResponsesAPI))
			req := &contract.CompletionRequest{
				Model: "org/m", StreamMode: streamMode, ModelHost: "host/route", CacheSessionID: "sess",
				ThinkingEffort: contract.EffortLevel(contract.LevelHigh), Capabilities: &contract.ModelCapabilities{},
				Messages: messages.User("why?"),
			}
			first := complete(t, p, req)
			if first.Content != "answer" || first.Reasoning != "because" || first.StopReason != messages.StopReasonEndTurn {
				t.Fatalf("reply: %+v", first)
			}
			body := bodies[0]
			if body["store"] != false || body["session_id"] != "sess" || body["model"] != "org/m" {
				t.Fatalf("request: %v", body)
			}
			if reasoning, _ := body["reasoning"].(map[string]any); reasoning["effort"] != "high" || reasoning["summary"] != "auto" {
				t.Fatalf("reasoning: %v", body["reasoning"])
			}
			if provider, _ := body["provider"].(map[string]any); fmt.Sprint(provider["only"]) != "[host/route]" {
				t.Fatalf("routing: %v", body["provider"])
			}
			meta, _ := first.Metadata[MetadataKey].(map[string]any)
			if meta["response_id"] != "resp_1" || meta["model"] != "org/m" || first.Metadata["openai_reasoning_items"] != nil {
				t.Fatalf("attribution: %v", first.Metadata)
			}
			// The reply survives a session reload as generic JSON.
			raw, _ := json.Marshal(first)
			var loaded messages.ChatMessage
			if err := json.Unmarshal(raw, &loaded); err != nil {
				t.Fatal(err)
			}
			sameJSON(t, ResponsesReplay(loaded, Endpoint(server.URL+"/v1"), "org/m"), "["+item+"]")
			if plain, details := ChatReplay(loaded, Endpoint(server.URL+"/v1"), "org/m"); plain != "" || details != nil {
				t.Fatalf("responses reply offered to chat replay: %q %s", plain, details)
			}
			_, structured := Replay(loaded, Endpoint(server.URL+"/v1"), "org/m")
			sameJSON(t, structured, "["+item+"]")

			history := append(messages.User("why?"), loaded)
			history = append(history, messages.User("and?")...)
			complete(t, p, &contract.CompletionRequest{Model: "org/m", StreamMode: streamMode, Messages: history, Capabilities: &contract.ModelCapabilities{}})
			input, _ := bodies[1]["input"].([]any)
			if len(input) != 4 {
				t.Fatalf("input items: %v", input)
			}
			sameJSON(t, input[1], item)
			if assistant, _ := input[2].(map[string]any); assistant["role"] != "assistant" {
				t.Fatalf("assistant turn after its reasoning: %v", input[2])
			}
			if _, present := bodies[1]["reasoning"]; present {
				t.Fatalf("reasoning control sent without a preference: %v", bodies[1]["reasoning"])
			}
		})
	}
}

func TestResponsesReplayIsScopedToGatewayAndModel(t *testing.T) {
	msg := messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonEndTurn, Metadata: map[string]any{
		MetadataKey: map[string]any{"endpoint": Endpoint(""), "requested_model": "m", "reasoning_items": []any{map[string]any{"type": "reasoning", "id": "rs"}}},
	}}
	if ResponsesReplay(msg, Endpoint(""), "m") == nil {
		t.Fatal("own reply not replayed")
	}
	if ResponsesReplay(msg, Endpoint("https://other.example/v1"), "m") != nil || ResponsesReplay(msg, Endpoint(""), "other") != nil {
		t.Fatal("replayed to a different gateway or model")
	}
	msg.Metadata[MetadataKey].(map[string]any)["incomplete"] = true
	if ResponsesReplay(msg, Endpoint(""), "m") != nil {
		t.Fatal("incomplete reply replayed")
	}
}

func TestChatStreamReportsBilledCost(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, responsesStream(
			`{"id":"gen-1","choices":[{"index":0,"delta":{"content":"ok"}}]}`,
			`{"id":"gen-1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`{"id":"gen-1","choices":[],"usage":{"prompt_tokens":194,"completion_tokens":2,"total_tokens":196,"cost":0.00042}}`,
		))
	}))
	defer server.Close()
	p := NewProvider("key", server.URL+"/v1")
	final := complete(t, p, &contract.CompletionRequest{
		Model: "org/m", StreamMode: contract.Streaming, Capabilities: &contract.ModelCapabilities{},
		Messages: messages.User("hi"),
	})
	if cost, ok := final.GetReportedCost(); !ok || cost != 0.00042 {
		t.Fatalf("reported cost = %v, %v; want 0.00042", cost, ok)
	}
	if final.GetInputTokens() != 194 || final.GetOutputTokens() != 2 {
		t.Fatalf("usage = %v", final.Metadata)
	}
}
