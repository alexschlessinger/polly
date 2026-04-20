package llm

import (
	"context"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
)

type recordingLLM struct {
	onCall func(*CompletionRequest)
}

func (r *recordingLLM) ChatCompletionStream(_ context.Context, req *CompletionRequest, processor EventStreamProcessor) <-chan *messages.StreamEvent {
	if r.onCall != nil {
		r.onCall(req)
	}
	msgs := make(chan messages.ChatMessage)
	close(msgs)
	return processor.ProcessMessagesToEvents(msgs)
}

func TestMultiPass_InvalidModelFormat_EmitsErrorEvent(t *testing.T) {
	m := NewMultiPass(map[string]string{})
	processor := messages.NewStreamProcessor()

	events := m.ChatCompletionStream(context.Background(), &CompletionRequest{
		Model: "gpt-5.4",
	}, processor)

	assertSingleErrorEvent(t, events, "model must include provider prefix")
}

func TestMultiPass_MissingAPIKey_EmitsErrorEvent(t *testing.T) {
	m := NewMultiPass(map[string]string{})
	processor := messages.NewStreamProcessor()

	events := m.ChatCompletionStream(context.Background(), &CompletionRequest{
		Model: "openai/gpt-5.4",
	}, processor)

	assertSingleErrorEvent(t, events, "missing API key for provider 'openai'")
}

func TestMultiPass_UnknownProvider_EmitsErrorEvent(t *testing.T) {
	m := NewMultiPass(map[string]string{})
	processor := messages.NewStreamProcessor()

	events := m.ChatCompletionStream(context.Background(), &CompletionRequest{
		Model:  "unknown/model",
		APIKey: "test-key",
	}, processor)

	assertSingleErrorEvent(t, events, "unknown provider 'unknown'")
}

func TestGetEnvVarNameForProvider_Known(t *testing.T) {
	tests := []struct {
		provider string
		want     string
	}{
		{"openai", "POLLYTOOL_OPENAIKEY"},
		{"anthropic", "POLLYTOOL_ANTHROPICKEY"},
		{"gemini", "POLLYTOOL_GEMINIKEY"},
		{"ollama", "POLLYTOOL_OLLAMAKEY"},
	}

	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			got := getEnvVarNameForProvider(tt.provider)
			if got != tt.want {
				t.Errorf("getEnvVarNameForProvider(%q) = %q, want %q", tt.provider, got, tt.want)
			}
		})
	}
}

func TestGetEnvVarNameForProvider_Unknown(t *testing.T) {
	got := getEnvVarNameForProvider("mycloud")
	want := "POLLYTOOL_MYCLOUDKEY"
	if got != want {
		t.Errorf("getEnvVarNameForProvider(%q) = %q, want %q", "mycloud", got, want)
	}
}

func TestNewMultiPass_SnapshotsAPIKeys(t *testing.T) {
	apiKeys := map[string]string{"openai": "first-key"}

	m := NewMultiPass(apiKeys)

	apiKeys["openai"] = "second-key"
	apiKeys["gemini"] = "gemini-key"

	if got := m.apiKeys["openai"]; got != "first-key" {
		t.Fatalf("snapshotted openai key = %q, want %q", got, "first-key")
	}
	if _, ok := m.apiKeys["gemini"]; ok {
		t.Fatal("expected added provider not to appear in snapshotted apiKeys")
	}
}

func TestNewMultiPass_DoesNotConstructProviders(t *testing.T) {
	var calls int

	m := newMultiPass(map[string]string{"openai": "test-key"}, map[string]providerFactory{
		"openai": func(apiKey, baseURL string) (LLM, error) {
			calls++
			return &recordingLLM{}, nil
		},
	})

	if m == nil {
		t.Fatal("expected non-nil MultiPass")
	}
	if calls != 0 {
		t.Fatalf("provider factory calls = %d, want 0", calls)
	}
}

func TestMultiPass_ClientFor_DoesNotCacheClients(t *testing.T) {
	var calls int

	m := newMultiPass(nil, map[string]providerFactory{
		"openai": func(apiKey, baseURL string) (LLM, error) {
			calls++
			return &recordingLLM{}, nil
		},
	})

	if _, err := m.clientFor("openai", "test-key", ""); err != nil {
		t.Fatalf("first clientFor() error = %v", err)
	}
	if _, err := m.clientFor("openai", "test-key", ""); err != nil {
		t.Fatalf("second clientFor() error = %v", err)
	}
	if calls != 2 {
		t.Fatalf("provider factory calls = %d, want 2", calls)
	}
}

func TestMultiPass_UsesDefaultProviderConfig(t *testing.T) {
	var gotAPIKey string
	var gotBaseURL string
	var gotReq *CompletionRequest

	m := newMultiPass(map[string]string{"openai": "default-key"}, map[string]providerFactory{
		"openai": func(apiKey, baseURL string) (LLM, error) {
			gotAPIKey = apiKey
			gotBaseURL = baseURL
			return &recordingLLM{
				onCall: func(req *CompletionRequest) {
					gotReq = req
				},
			}, nil
		},
	})

	processor := messages.NewStreamProcessor()
	events := m.ChatCompletionStream(context.Background(), &CompletionRequest{
		Model: "openai/gpt-5.4",
	}, processor)
	drainEvents(events)

	if gotAPIKey != "default-key" {
		t.Fatalf("factory apiKey = %q, want %q", gotAPIKey, "default-key")
	}
	if gotBaseURL != "" {
		t.Fatalf("factory baseURL = %q, want empty", gotBaseURL)
	}
	if gotReq == nil {
		t.Fatal("expected provider to receive request")
	}
	if gotReq.APIKey != "default-key" {
		t.Fatalf("request apiKey = %q, want %q", gotReq.APIKey, "default-key")
	}
	if gotReq.Model != "gpt-5.4" {
		t.Fatalf("request model = %q, want %q", gotReq.Model, "gpt-5.4")
	}
}

func TestMultiPass_OpenAIBaseURLAllowsMissingAPIKey(t *testing.T) {
	var gotAPIKey string
	var gotBaseURL string
	var gotReq *CompletionRequest

	m := newMultiPass(nil, map[string]providerFactory{
		"openai": func(apiKey, baseURL string) (LLM, error) {
			gotAPIKey = apiKey
			gotBaseURL = baseURL
			return &recordingLLM{
				onCall: func(req *CompletionRequest) {
					gotReq = req
				},
			}, nil
		},
	})

	processor := messages.NewStreamProcessor()
	events := m.ChatCompletionStream(context.Background(), &CompletionRequest{
		Model:   "openai/gpt-5.4",
		BaseURL: "http://example.test/v1",
	}, processor)
	drainEvents(events)

	if gotAPIKey != "" {
		t.Fatalf("factory apiKey = %q, want empty", gotAPIKey)
	}
	if gotBaseURL != "http://example.test/v1" {
		t.Fatalf("factory baseURL = %q, want %q", gotBaseURL, "http://example.test/v1")
	}
	if gotReq == nil {
		t.Fatal("expected provider to receive request")
	}
	if gotReq.APIKey != "" {
		t.Fatalf("request apiKey = %q, want empty", gotReq.APIKey)
	}
}

func TestMultiPass_ClientFor_DefaultsOllamaBaseURL(t *testing.T) {
	var gotAPIKey string
	var gotBaseURL string

	m := newMultiPass(nil, map[string]providerFactory{
		"ollama": func(apiKey, baseURL string) (LLM, error) {
			gotAPIKey = apiKey
			gotBaseURL = baseURL
			return &recordingLLM{}, nil
		},
	})

	if _, err := m.clientFor("ollama", "ollama-key", ""); err != nil {
		t.Fatalf("clientFor() error = %v", err)
	}

	if gotAPIKey != "ollama-key" {
		t.Fatalf("factory apiKey = %q, want %q", gotAPIKey, "ollama-key")
	}
	if gotBaseURL != "http://localhost:11434" {
		t.Fatalf("factory baseURL = %q, want %q", gotBaseURL, "http://localhost:11434")
	}
}

func drainEvents(events <-chan *messages.StreamEvent) {
	for range events {
	}
}

func assertSingleErrorEvent(t *testing.T, events <-chan *messages.StreamEvent, wantSubstring string) {
	t.Helper()

	var got []*messages.StreamEvent
	for ev := range events {
		got = append(got, ev)
	}

	if len(got) != 1 {
		t.Fatalf("expected exactly 1 event, got %d", len(got))
	}
	if got[0].Type != messages.EventTypeError {
		t.Fatalf("expected EventTypeError, got %q", got[0].Type)
	}
	if got[0].Error == nil {
		t.Fatal("expected non-nil error")
	}
	if !strings.Contains(got[0].Error.Error(), wantSubstring) {
		t.Fatalf("expected error to contain %q, got %q", wantSubstring, got[0].Error.Error())
	}
}
