package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm/qwencloud"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestQwenCloudRoutingAndThinking(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, effort := range []ThinkingEffort{EffortOff(), EffortDynamic(), EffortLevel(LevelHigh), EffortBudget(12000)} {
			t.Run(fmt.Sprintf("stream=%v/%s", stream, effort), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/compatible-mode/v1/models" {
						fmt.Fprint(w, `{"data":[{"id":"qwen3.8-max"}]}`)
						return
					}
					if r.URL.Path != "/compatible-mode/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer fixture" {
						t.Errorf("bad route/auth: %s", r.URL)
					}
					var body struct {
						Model               string `json:"model"`
						EnableThinking      *bool  `json:"enable_thinking"`
						ThinkingBudget      int    `json:"thinking_budget"`
						ReasoningEffort     string `json:"reasoning_effort"`
						PreserveThinking    bool   `json:"preserve_thinking"`
						MaxCompletionTokens int    `json:"max_completion_tokens"`
						MaxTokens           *int   `json:"max_tokens"`
						Stream              bool   `json:"stream"`
						StreamOptions       *struct {
							IncludeUsage bool `json:"include_usage"`
						} `json:"stream_options"`
						Messages []struct {
							Content   any    `json:"content"`
							Reasoning string `json:"reasoning_content"`
						} `json:"messages"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
						return
					}
					budget, _ := effort.AsBudget()
					if body.Model != "qwen3.8-max" || body.EnableThinking == nil || *body.EnableThinking != effort.IsEnabled() || body.ThinkingBudget != budget || body.ReasoningEffort != "" || body.Stream != stream {
						t.Errorf("bad request: %+v", body)
					}
					if !body.PreserveThinking || body.MaxCompletionTokens != 128 || body.MaxTokens != nil {
						t.Errorf("lost model options: %+v", body)
					}
					if stream && (body.StreamOptions == nil || !body.StreamOptions.IncludeUsage) {
						t.Error("missing streamed usage request")
					}
					if !stream && body.StreamOptions != nil {
						t.Error("unexpected stream options")
					}
					if len(body.Messages) != 2 || body.Messages[0].Reasoning != "" || body.Messages[1].Reasoning != "prior reasoning" || body.Messages[1].Content != "prior answer" {
						t.Errorf("bad replay: %+v", body.Messages)
					}
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"think\"}}]}\n\n")
						fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"answer\"},\"finish_reason\":\"stop\"}]}\n\n")
						fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}\n\ndata: [DONE]\n\n")
					} else {
						fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"answer","reasoning_content":"think"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
					}
				}))
				defer server.Close()
				history := []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "question", Reasoning: "ignore"}, {Role: messages.MessageRoleAssistant, Content: "prior answer", Reasoning: "prior reasoning"}}
				req := &CompletionRequest{Model: "qwencloud/qwen3.8-max", BaseURL: server.URL + "/compatible-mode/v1", Stream: &stream, ThinkingEffort: effort, MaxTokens: 128, Messages: history}
				final, err := routerCompletion(context.Background(), NewMultiPass(map[string]string{"qwencloud": "fixture"}), req)
				if err != nil {
					t.Fatal(err)
				}
				if final.GetContent() != "answer" || final.Reasoning != "think" {
					t.Fatalf("bad completion: %+v", final)
				}
				if final.GetInputTokens() != 10 || final.GetOutputTokens() != 5 {
					t.Fatalf("lost usage: %+v", final.Metadata)
				}
				if req.Model != "qwencloud/qwen3.8-max" || req.Messages[1].Reasoning != "prior reasoning" {
					t.Fatal("mutated caller request")
				}
			})
		}
	}
}

func TestQwenCloudConfiguration(t *testing.T) {
	spec := defaultProviders()["qwencloud"]
	if spec.defaultBaseURL != qwencloud.DefaultBaseURL || !ProviderRequiresKey("qwencloud/m", "") || ProviderKeyEnvVar("qwencloud") != "POLLYTOOL_QWENCLOUDKEY" {
		t.Fatal("bad provider configuration")
	}
	t.Setenv("POLLYTOOL_QWENCLOUDKEY", "")
	_, err := routerCompletion(context.Background(), NewMultiPass(nil), &CompletionRequest{Model: "qwencloud/m"})
	if err == nil || !strings.Contains(err.Error(), "POLLYTOOL_QWENCLOUDKEY") {
		t.Fatalf("missing key error: %v", err)
	}
}

func TestQwenCloudToolRoundTrip(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body struct {
			PreserveThinking bool `json:"preserve_thinking"`
			Messages         []struct {
				Role       string `json:"role"`
				Reasoning  string `json:"reasoning_content"`
				ToolCallID string `json:"tool_call_id"`
				ToolCalls  []struct {
					ID string `json:"id"`
				} `json:"tool_calls"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if calls == 1 {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","reasoning_content":"need weather","tool_calls":[{"id":"weather-1","type":"function","function":{"name":"weather","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		if !body.PreserveThinking || len(body.Messages) != 3 || body.Messages[1].Reasoning != "need weather" || len(body.Messages[1].ToolCalls) != 1 || body.Messages[1].ToolCalls[0].ID != "weather-1" || body.Messages[2].ToolCallID != "weather-1" {
			t.Errorf("lost tool history: %+v", body.Messages)
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"sunny"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()
	stream := false
	client := NewMultiPass(map[string]string{"qwencloud": "fixture"})
	req := &CompletionRequest{Model: "qwencloud/qwen3.8-flash", BaseURL: server.URL, Stream: &stream, Capabilities: &ModelCapabilities{}, ThinkingEffort: EffortDynamic(), Messages: []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "weather?"}}}
	first, err := routerCompletion(context.Background(), client, req)
	if err != nil {
		t.Fatal(err)
	}
	if first.StopReason != messages.StopReasonToolUse || len(first.ToolCalls) != 1 || first.ToolCalls[0].Name != "weather" || first.ToolCalls[0].Arguments != "{}" {
		t.Fatalf("bad tool call: %+v", first)
	}
	// Persist/reload the assistant turn before the next request, as sessions do.
	raw, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	var loaded messages.ChatMessage
	if err := json.Unmarshal(raw, &loaded); err != nil {
		t.Fatal(err)
	}
	req.Messages = append(req.Messages, loaded, messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: "weather-1", Content: "sunny"})
	final, err := routerCompletion(context.Background(), client, req)
	if err != nil {
		t.Fatal(err)
	}
	if final.GetContent() != "sunny" || calls != 2 {
		t.Fatalf("bad final: %+v (%d calls)", final, calls)
	}
}
