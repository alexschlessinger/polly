package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
)

func TestAgentClampsInheritedBudgetToChildModel(t *testing.T) {
	window := 8000
	model := &metadataRecordingLLM{info: ModelInfo{ModelCapabilities: ModelCapabilities{ContextTokens: &window}}}
	agent := NewAgent(model, nil, AgentConfig{})
	defer agent.Close()
	var history []messages.ChatMessage
	for range 4 {
		history = append(history, messages.ChatMessage{Role: messages.MessageRoleUser, Content: strings.Repeat("history ", 3000)}, messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "done"})
	}
	history = append(history, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "continue"})
	req := &CompletionRequest{Model: "test/smaller-child", MaxContextTokens: 256000, MaxTokens: 1000, Messages: history}
	result, err := agent.Run(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Projection.OmittedExchanges == 0 || result.Projection.RequestEstimatedTokens > 6200 {
		t.Fatalf("inherited history exceeded child window: %+v", result.Projection)
	}
	if len(model.requests) != 1 || req.MaxContextTokens != 256000 {
		t.Fatalf("child budget or caller changed: %+v", model.requests)
	}
}

func TestNativeProviderSchemaUsesToolFallback(t *testing.T) {
	req := &CompletionRequest{Model: "anthropic/claude", ResponseSchema: &Schema{}}
	for _, supported := range []*bool{nil, truth(true), truth(false)} {
		out, _, err := PrepareCapabilities(req, ModelCapabilities{StructuredOutput: truth(false), Tools: supported}, false)
		if supported != nil && !*supported {
			if err == nil {
				t.Fatal("accepted schema without tools or native structured output")
			}
		} else if err != nil || out.ResponseSchema != req.ResponseSchema {
			t.Fatalf("tool schema fallback rejected: %v", err)
		}
	}
}

func TestMultiPassScopesGlobalBaseURLToCompatibleProviders(t *testing.T) {
	for _, provider := range []string{"anthropic", "gemini", "openai", "ollama"} {
		t.Run(provider, func(t *testing.T) {
			m := NewMultiPass(map[string]string{provider: "fixture"})
			spec := m.providers[provider]
			want := "https://compatible.invalid/v1"
			if provider == "anthropic" || provider == "gemini" {
				want = spec.defaultBaseURL
			}
			var discovered, constructed string
			spec.metadata = func(_ context.Context, _ *http.Client, target ModelTarget) (ModelCatalog, error) {
				discovered = target.BaseURL
				return ModelCatalog{Models: []ModelInfo{{ID: "m"}}}, nil
			}
			spec.new = func(_, baseURL string) (LLM, error) {
				constructed = baseURL
				return &recordingLLM{}, nil
			}
			m.providers[provider] = spec
			req := &CompletionRequest{Model: provider + "/m", BaseURL: "https://compatible.invalid/v1"}
			prepared, _, err := Prepare(context.Background(), m, req, false)
			if err != nil {
				t.Fatal(err)
			}
			for range m.ChatCompletionStream(context.Background(), prepared, messages.NewStreamProcessor()) {
			}
			if discovered != want || constructed != want || req.BaseURL != "https://compatible.invalid/v1" {
				t.Fatalf("metadata=%q completion=%q want=%q caller=%q", discovered, constructed, want, req.BaseURL)
			}
		})
	}
}

func TestMetadataSingleModelAcceptsCanonicalAliasResponse(t *testing.T) {
	for _, provider := range []string{"anthropic", "gemini", "openai", "deepseek"} {
		t.Run(provider, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch provider {
				case "gemini":
					fmt.Fprint(w, `{"name":"models/canonical","inputTokenLimit":32000}`)
				case "deepseek":
					fmt.Fprint(w, `{"data":[{"id":"unrelated"}]}`)
				default:
					fmt.Fprint(w, `{"id":"canonical","max_input_tokens":32000}`)
				}
			}))
			defer server.Close()
			m := NewMultiPass(map[string]string{provider: "fixture"})
			info, err := m.GetModelInfo(context.Background(), ModelTarget{Provider: provider, Model: "alias", BaseURL: server.URL})
			if provider == "deepseek" {
				if err == nil || info != nil {
					t.Fatal("list lookup accepted an unrelated model")
				}
			} else if err != nil || info.ID != "canonical" || info.ContextWindow() != 32000 {
				t.Fatalf("alias lookup: %+v %v", info, err)
			}
		})
	}
}
