package llm

import (
	"context"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
)

func TestMultiPass_InvalidModelFormat_EmitsErrorEvent(t *testing.T) {
	m := NewMultiPass(map[string]string{})
	processor := messages.NewStreamProcessor()

	events := m.ChatCompletionStream(context.Background(), &CompletionRequest{
		Model: "gpt-4.1",
	}, processor)

	assertSingleErrorEvent(t, events, "model must include provider prefix")
}

func TestMultiPass_MissingAPIKey_EmitsErrorEvent(t *testing.T) {
	m := NewMultiPass(map[string]string{})
	processor := messages.NewStreamProcessor()

	events := m.ChatCompletionStream(context.Background(), &CompletionRequest{
		Model: "openai/gpt-4.1",
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
