package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm/anthropic"
	"github.com/alexschlessinger/pollytool/llm/gemini"
	"github.com/alexschlessinger/pollytool/llm/openai"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestBareModelEstimatesPreservedReasoningConservatively(t *testing.T) {
	opaque := strings.Repeat("x", 60_000)
	for _, tt := range []struct {
		model    string
		metadata map[string]any
	}{
		{"gpt-5.5", map[string]any{
			openai.ResponsesReasoningModelKey: "gpt-5.5",
			openai.ResponsesReasoningItemsKey: []map[string]any{{"id": "rs_1", "encrypted_content": opaque}},
		}},
		{"claude-sonnet-4-6", map[string]any{
			anthropic.ThinkingBlocksKey: []map[string]any{{"type": "thinking", "thinking": opaque, "signature": "signed"}},
		}},
		{"gemini-3-pro", map[string]any{
			gemini.ThoughtSignaturesKey: map[string]string{"c1": base64.StdEncoding.EncodeToString([]byte(opaque))},
		}},
	} {
		t.Run(tt.model, func(t *testing.T) {
			msg := callTool("c1", "fetch", 0)
			msg.Metadata = tt.metadata
			msg.Reasoning = strings.Repeat("plain reasoning is not known to replay ", 10_000)
			for _, reloaded := range []bool{false, true} {
				if reloaded {
					raw, err := json.Marshal(msg)
					if err != nil {
						t.Fatal(err)
					}
					msg = messages.ChatMessage{}
					if err := json.Unmarshal(raw, &msg); err != nil {
						t.Fatal(err)
					}
				}
				for _, baseURL := range []string{"", "https://example.org/v1"} {
					estimate := messageEstimator(&CompletionRequest{Model: tt.model, BaseURL: baseURL})
					want := estimateMessageTokensWith(msg, false)
					if got := estimate(msg); got != want || got < len(opaque)/3 {
						t.Fatalf("reloaded=%v endpoint=%q: estimate=%d want conservative %d", reloaded, baseURL, got, want)
					}
					plain := msg.Clone()
					plain.Metadata = nil
					if got, want := estimate(plain), messages.EstimateMessageTokens(plain)-estimatedStringTokens(plain.Reasoning); got != want {
						t.Fatalf("bare model charged unsupported plain reasoning: estimate=%d want=%d", got, want)
					}
				}
			}
		})
	}
}

// Exercise the actual direct clients, not a router-shaped stub: a bare model
// identifies neither the provider nor whether a custom endpoint uses Responses.
func TestDirectResponsesAgentBudgetsReplayedReasoning(t *testing.T) {
	for _, custom := range []bool{false, true} {
		for _, budget := range []int{2_000, 100_000} {
			t.Run(fmt.Sprintf("custom=%v/budget=%d", custom, budget), func(t *testing.T) {
				captured := make(chan []byte, 4)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/responses") {
						t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					}
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Errorf("read request: %v", err)
					}
					select {
					case captured <- body:
					default:
						t.Error("unexpected additional provider request")
					}
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, `{"id":"resp_1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"a short answer"}]}]}`)
				}))
				defer server.Close()
				target, err := url.Parse(server.URL)
				if err != nil {
					t.Fatal(err)
				}
				transport := server.Client().Transport
				client := &http.Client{Transport: nativeRoundTripper(func(req *http.Request) (*http.Response, error) {
					copy := req.Clone(req.Context())
					copy.URL.Scheme, copy.URL.Host, copy.Host = target.Scheme, target.Host, target.Host
					return transport.RoundTrip(copy)
				})}
				provider := openai.NewProvider("test", "", openai.WithHTTPClient(client))
				baseURL := ""
				if custom {
					baseURL = server.URL
					provider = openai.NewResponsesProvider("test", baseURL, openai.WithHTTPClient(client))
				}
				agent := NewAgent(provider, nil, AgentConfig{DisableTools: true})
				defer agent.Close()
				opaque := strings.Repeat("x", 60_000)
				old := reply("hello", 0)
				old.Metadata = map[string]any{
					openai.ResponsesReasoningModelKey: "gpt-5.5",
					openai.ResponsesReasoningItemsKey: []map[string]any{{"id": "rs_old", "encrypted_content": opaque}},
				}
				history := []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "hello"}, old, {Role: messages.MessageRoleUser, Content: "again"}}
				response, err := agent.Run(context.Background(), &CompletionRequest{
					Model: "gpt-5.5", BaseURL: baseURL, Messages: history, MaxContextTokens: budget, StreamMode: Buffered,
				}, nil)
				if err != nil {
					t.Fatal(err)
				}
				wantCalls := 1
				if budget == 2_000 {
					wantCalls = 2
				}
				if len(captured) != wantCalls {
					t.Fatalf("provider calls=%d want=%d; projection=%+v", len(captured), wantCalls, response.Projection)
				}
				var final []byte
				for len(captured) > 0 {
					final = <-captured
				}
				if budget == 2_000 {
					if strings.Contains(string(final), opaque) || response.Projection.RequestEstimatedTokens > budget {
						t.Fatalf("compacted continuation exceeds budget: projection=%+v", response.Projection)
					}
					compacted := false
					for _, msg := range response.AllMessages {
						if c, ok := msg.Compaction(); ok && c.Summary != "" {
							compacted = true
						}
					}
					if !compacted {
						t.Fatal("large replayed reasoning did not trigger a summary")
					}
				} else if !strings.Contains(string(final), opaque) || response.Projection.RequestEstimatedTokens < len(opaque)/3 {
					t.Fatalf("direct Responses replay not accounted for: projection=%+v", response.Projection)
				}
			})
		}
	}
}
