package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alexschlessinger/pollytool/llm/openai"
	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestCompatibleChatCompletionResponses(t *testing.T) {
	for _, provider := range []string{"openai", "deepseek"} {
		for _, mode := range []string{"stream", "nonstream", "early_eof"} {
			t.Run(provider+"/"+mode, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var req openai.ChatCompletionRequest
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Error(err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					wantReplay := ""
					if provider == "deepseek" {
						wantReplay = "prior reasoning"
					}
					if len(req.Messages) != 2 || req.Messages[1].ReasoningContent != wantReplay {
						t.Errorf("provider reasoning replay: %+v", req.Messages)
					}
					if mode == "early_eof" {
						w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n"))
						return
					}
					if mode == "nonstream" {
						w.Write([]byte(`{"choices":[{"message":{"reasoning_content":"why","content":"answer","tool_calls":[{"id":"call","type":"function","function":{"name":"work","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":20,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":10}}}`))
						return
					}
					w.Write([]byte("data: " + `{"choices":[{"delta":{"reasoning_content":"why","content":"answer","tool_calls":[{"index":0,"id":"call","type":"function","function":{"name":"work","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n" +
						"data: " + `{"choices":[],"usage":{"prompt_tokens":20,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":10}}}` + "\n\n" +
						"data: [DONE]\n\n"))
				}))
				defer server.Close()
				var client LLM = NewOpenAIClient("test", server.URL)
				if provider == "deepseek" {
					client = NewDeepSeekClient("test", server.URL)
				}
				stream := mode != "nonstream"
				req := &CompletionRequest{Model: "test", Stream: &stream, Messages: []messages.ChatMessage{
					{Role: messages.MessageRoleUser, Content: "hi"},
					{Role: messages.MessageRoleAssistant, Content: "prior", Reasoning: "prior reasoning"},
				}}
				var final *messages.ChatMessage
				var streamErr error
				var order []messages.StreamEventType
				for event := range client.ChatCompletionStream(context.Background(), req, messages.NewStreamProcessor()) {
					switch event.Type {
					case messages.EventTypeError:
						streamErr = event.Error
					case messages.EventTypeComplete:
						final = event.Message
					case messages.EventTypeContent, messages.EventTypeReasoning:
						order = append(order, event.Type)
					}
				}
				if mode == "early_eof" {
					if streamErr == nil || streamErr.Error() != streaming.ErrStreamEndedEarly.Error() || final != nil {
						t.Fatalf("premature completion: final=%+v error=%v", final, streamErr)
					}
					return
				}
				if streamErr != nil || final == nil {
					t.Fatalf("final=%+v error=%v", final, streamErr)
				}
				if len(order) != 2 || order[0] != messages.EventTypeReasoning || order[1] != messages.EventTypeContent {
					t.Fatalf("reasoning/content order=%v", order)
				}
				if final.Content != "answer" || final.Reasoning != "why" || final.StopReason != messages.StopReasonToolUse || len(final.ToolCalls) != 1 {
					t.Fatalf("completion lost fields: %+v", final)
				}
				if final.ToolCalls[0] != (messages.ChatMessageToolCall{ID: "call", Name: "work", Arguments: "{}"}) {
					t.Fatalf("tool call=%+v", final.ToolCalls[0])
				}
				if final.GetInputTokens() != 20 || final.GetOutputTokens() != 5 || final.GetCacheReadInputTokens() != 10 {
					t.Fatalf("usage lost: %+v", final.Metadata)
				}
			})
		}
	}
}
