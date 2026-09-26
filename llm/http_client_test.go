package llm_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/llm/anthropic"
	"github.com/alexschlessinger/pollytool/llm/codex"
	"github.com/alexschlessinger/pollytool/llm/deepseek"
	"github.com/alexschlessinger/pollytool/llm/gemini"
	"github.com/alexschlessinger/pollytool/llm/ollama"
	"github.com/alexschlessinger/pollytool/llm/openai"
	"github.com/alexschlessinger/pollytool/llm/openrouter"
	"github.com/alexschlessinger/pollytool/llm/qwencloud"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func jsonResponse(body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}

const chatJSON = `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`
const responseJSON = `{"id":"resp_1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":10,"output_tokens":2}}`
const anthropicJSON = `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":2}}`
const geminiJSON = `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":2}}`
const ollamaJSON = `{"model":"m","message":{"role":"assistant","content":"ok"},"done":true,"done_reason":"stop","prompt_eval_count":10,"eval_count":2}`
const codexSSE = "data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"ok\"}\n\ndata: {\"type\":\"response.completed\",\"response\":" + responseJSON + "}\n\ndata: [DONE]\n\n"

// staticLogin is a sign-in whose token never changes.
type staticLogin struct{}

func (staticLogin) Credential(context.Context) (llm.Credential, error) {
	return llm.Credential{AccessToken: "tok", AccountID: "acct_1"}, nil
}
func (staticLogin) Refresh(context.Context, string) (llm.Credential, error) {
	return llm.Credential{AccessToken: "tok", AccountID: "acct_1"}, nil
}
func (staticLogin) Account() (llm.Account, bool) { return llm.Account{ID: "acct_1"}, true }

func TestHTTPClientReachesDirectAndRoutedProviders(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		direct     func(*http.Client) llm.LLM
	}{
		{"openai", responseJSON, func(c *http.Client) llm.LLM { return openai.NewProvider("key", "", openai.WithHTTPClient(c)) }},
		{"anthropic", anthropicJSON, func(c *http.Client) llm.LLM { return anthropic.NewProvider("key", "", anthropic.WithHTTPClient(c)) }},
		{"gemini", geminiJSON, func(c *http.Client) llm.LLM {
			p, err := gemini.NewProvider("key", "", gemini.WithHTTPClient(c))
			if err != nil {
				t.Fatal(err)
			}
			return p
		}},
		{"ollama", ollamaJSON, func(c *http.Client) llm.LLM {
			return ollama.NewProvider("http://localhost:11434", "key", ollama.WithHTTPClient(c))
		}},
		{"deepseek", chatJSON, func(c *http.Client) llm.LLM { return deepseek.NewProvider("key", "", deepseek.WithHTTPClient(c)) }},
		{"qwencloud", chatJSON, func(c *http.Client) llm.LLM { return qwencloud.NewProvider("key", "", qwencloud.WithHTTPClient(c)) }},
		{"openrouter", chatJSON, func(c *http.Client) llm.LLM { return openrouter.NewProvider("key", "", openrouter.WithHTTPClient(c)) }},
		{"huggingface", chatJSON, nil},
		{"codex", codexSSE, func(c *http.Client) llm.LLM { return codex.NewProvider(staticLogin{}, "", codex.WithHTTPClient(c)) }},
	} {
		for _, routed := range []bool{false, true} {
			if !routed && tc.direct == nil {
				continue
			}
			mode := "direct"
			if routed {
				mode = "router"
			}
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				calls := 0
				transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
					calls++
					if tc.name == "ollama" && req.Header.Get("Authorization") != "Bearer key" {
						t.Error("missing Ollama authentication")
					}
					if tc.name == "codex" && (req.Header.Get("Authorization") != "Bearer tok" || req.Header.Get("chatgpt-account-id") != "acct_1" || req.Host != "chatgpt.com") {
						t.Errorf("codex request not signed for the backend: %s %v", req.URL, req.Header)
					}
					return jsonResponse(tc.body), nil
				})
				client := &http.Client{Transport: transport}
				var provider llm.LLM
				model := "m"
				if routed {
					opts := []llm.ClientOption{llm.WithHTTPClient(client)}
					if tc.name == "codex" {
						opts = append(opts, llm.WithLogin("codex", staticLogin{}))
					}
					provider = llm.NewMultiPass(map[string]string{tc.name: "key"}, opts...)
					model = tc.name + "/m"
				} else {
					provider = tc.direct(client)
				}
				streamMode := llm.Buffered
				got, err := llm.Complete(context.Background(), provider, &llm.CompletionRequest{Model: model, StreamMode: streamMode, Capabilities: &llm.ModelCapabilities{}})
				if err != nil || got == nil || got.Content != "ok" || got.GetInputTokens() != 10 || calls != 1 {
					t.Fatalf("got=%+v err=%v HTTP calls=%d", got, err, calls)
				}
				// In particular, Ollama must not replace the caller's transport with its auth wrapper.
				if _, ok := client.Transport.(roundTripFunc); !ok {
					t.Fatal("caller HTTP client mutated")
				}
			})
		}
	}
}

func TestHTTPClientReachesMetadataAndEmbeddings(t *testing.T) {
	var paths []string
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		paths = append(paths, req.URL.Path)
		switch {
		case strings.HasSuffix(req.URL.Path, "/models"):
			return jsonResponse(`{"data":[{"id":"m"}]}`), nil
		case strings.HasSuffix(req.URL.Path, "/embeddings"):
			return jsonResponse(`{"model":"m","data":[{"embedding":[0.1,0.2]}],"usage":{"total_tokens":3}}`), nil
		case strings.HasSuffix(req.URL.Path, ":batchEmbedContents"):
			return jsonResponse(`{"embeddings":[{"values":[0.1,0.2]}]}`), nil
		default:
			t.Errorf("unexpected request %s", req.URL)
			return jsonResponse(`{}`), nil
		}
	})}
	router := llm.NewMultiPass(map[string]string{"openai": "key"}, llm.WithHTTPClient(client))
	catalog, err := router.ListModels(context.Background(), llm.ModelTarget{Provider: "openai"}, true)
	if err != nil || len(catalog.Models) != 1 {
		t.Fatalf("catalog=%+v err=%v", catalog, err)
	}
	for _, provider := range []string{"openai", "gemini"} {
		got, err := llm.Embed(context.Background(), &llm.EmbeddingRequest{Model: provider + "/m", APIKey: "key", Input: []string{"hi"}}, llm.WithHTTPClient(client))
		if err != nil || len(got.Embeddings) != 1 {
			t.Fatalf("%s embeddings=%+v err=%v", provider, got, err)
		}
	}
	if len(paths) != 3 {
		t.Fatalf("requests=%v", paths)
	}
}
