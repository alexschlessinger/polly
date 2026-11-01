package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alexschlessinger/pollytool/llm/internal/contract"
	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestResponsesStreamErrorsRetainAPIClassification(t *testing.T) {
	for _, shape := range []string{"error", "response.failed"} {
		for _, tc := range []struct {
			code      string
			retryable bool
		}{
			{"server_error", true},
			{"rate_limit_exceeded", true},
			{"overloaded_error", true},
			{"insufficient_quota", false},
			{"usage_limit_reached", false},
			{"invalid_api_key", false},
			{"content_policy_violation", false},
			{"unknown_failure", false},
		} {
			t.Run(shape+"/"+tc.code, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if shape == "error" {
						fmt.Fprintf(w, "data: {\"type\":\"error\",\"code\":%q,\"message\":\"request failed\"}\n\n", tc.code)
					} else {
						fmt.Fprintf(w, "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":%q,\"message\":\"request failed\"},\"usage\":{\"input_tokens\":17,\"output_tokens\":3,\"cost\":0.04}}}\n\n", tc.code)
					}
					fmt.Fprint(w, "data: [DONE]\n\n")
				}))
				defer server.Close()
				provider := NewResponsesProvider("test", server.URL)
				var failure error
				var usage *messages.StreamEvent
				for event := range provider.ChatCompletionStream(context.Background(), &contract.CompletionRequest{Model: "m", Capabilities: &contract.ModelCapabilities{}, Messages: messages.User("hello")}, messages.NewStreamProcessor()) {
					switch event.Type {
					case messages.EventTypeError:
						failure = event.Error
					case messages.EventTypeUsage:
						usage = event
					case messages.EventTypeComplete:
						t.Fatal("failed response produced a successful completion")
					}
				}
				var apiErr *APIError
				if !errors.As(failure, &apiErr) || string(apiErr.Code) != tc.code || apiErr.Retryable() != tc.retryable {
					t.Fatalf("typed error lost: %T %v", failure, failure)
				}
				if failure.Error() != tc.code+": request failed" {
					t.Fatalf("error event text changed: %v", failure)
				}
				if shape == "response.failed" && (usage == nil || usage.InputTokens != 17 || usage.OutputTokens != 3 || !usage.CostReported || usage.CostUSD != 0.04) {
					t.Fatalf("failed terminal event lost billed usage: %+v", usage)
				}
			})
		}
	}
}

func TestResponsesFailedEventPreservesMetadataWithoutPromotingTools(t *testing.T) {
	var event ResponseStreamEvent
	if err := json.Unmarshal([]byte(`{
		"type":"response.failed",
		"response":{
			"status":"failed",
			"error":{"code":"server_error","message":"busy"},
			"usage":{"input_tokens":17,"output_tokens":3},
			"output":[
				{"type":"reasoning","id":"r1","encrypted_content":"opaque","summary":[{"type":"summary_text","text":"thought"}]},
				{"type":"function_call","id":"f1","call_id":"c1","name":"dangerous","arguments":"{\"x\":1}"}
			]
		}
	}`), &event); err != nil {
		t.Fatal(err)
	}
	adapter := NewResponsesAdapter("m")
	ch := make(chan messages.ChatMessage, 10)
	core := streaming.NewStreamingCore(context.Background(), ch, adapter)
	if err := core.ProcessChunk(&event); err != nil {
		t.Fatalf("terminal processing skipped usage emission: %v", err)
	}
	core.CompleteStream()
	close(ch)
	var final messages.ChatMessage
	for msg := range ch {
		final = msg
	}
	var apiErr *APIError
	if !final.IsError() || !errors.As(final.GetError(), &apiErr) || apiErr != event.Response.Error {
		t.Fatalf("final message lost typed response error: %+v", final)
	}
	if final.StopReason != messages.StopReasonError {
		t.Fatalf("failed tools promoted to an executable batch: %+v", final)
	}
	if final.GetInputTokens() != 17 || final.GetOutputTokens() != 3 {
		t.Fatalf("failed final message lost usage: %+v", final)
	}
	items, ok := final.Metadata[ResponsesReasoningItemsKey].([]map[string]any)
	if !ok || len(items) != 1 || items[0]["encrypted_content"] != "opaque" || final.Metadata[ResponsesReasoningModelKey] != "m" {
		t.Fatalf("error classification damaged opaque metadata: %+v", final.Metadata)
	}
	order, ok := final.Metadata[ResponsesOutputOrderKey].([]string)
	if !ok || len(order) != 2 || order[0] != "reasoning:r1" || order[1] != "function_call:c1" {
		t.Fatalf("error classification damaged output ordering: %+v", final.Metadata)
	}
}
