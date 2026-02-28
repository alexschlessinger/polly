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
