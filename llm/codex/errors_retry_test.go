package codex

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm/internal/retry"
	"github.com/alexschlessinger/pollytool/llm/openai"
	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestDescribePreservesRetryExhaustionAndCause(t *testing.T) {
	for _, body := range []string{
		`{"detail":"service unavailable"}`,
		`{"detail":{"message":"service unavailable"}}`,
	} {
		t.Run(body, func(t *testing.T) {
			apiErr := &openai.APIError{StatusCode: 503, Message: body}
			err := retry.New(retry.Policy{MaxRetries: 0}).Wait(context.Background(), apiErr, time.Second)
			var original *retry.ExhaustedError
			if !errors.As(err, &original) {
				t.Fatalf("expected retry exhaustion: %v", err)
			}
			wrapped := fmt.Errorf("response stream: %w", err)
			got := describe(wrapped, &callState{})
			if got.Error() != "codex: service unavailable (HTTP 503)" {
				t.Fatalf("friendly description changed: %v", got)
			}
			var exhausted *retry.ExhaustedError
			var cause *openai.APIError
			if !errors.As(got, &exhausted) || exhausted != original || !errors.As(got, &cause) || cause != apiErr {
				t.Fatalf("description lost retry metadata or API cause: %T %v", got, got)
			}
			if !errors.Is(got, wrapped) || !errors.Is(got, original) || !errors.Is(got, apiErr) {
				t.Fatalf("description lost error identity: %v", got)
			}
			var classified interface{ Retryable() bool }
			if !errors.As(got, &classified) || classified.Retryable() {
				t.Fatalf("exhausted error became eligible for nested retries: %v", got)
			}
		})
	}
}

func TestDescribeDetailPreservesAPIClassificationWithoutBudget(t *testing.T) {
	for _, status := range []int{400, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			original := &openai.APIError{StatusCode: status, Message: `{"detail":"request failed"}`}
			got := describe(original, &callState{})
			if got.Error() != fmt.Sprintf("codex: request failed (HTTP %d)", status) || !errors.Is(got, original) {
				t.Fatalf("description lost friendly text or cause: %v", got)
			}
			var classified interface{ Retryable() bool }
			if !errors.As(got, &classified) || classified.Retryable() != original.Retryable() {
				t.Fatalf("description changed classification: %v", got)
			}
		})
	}
}

func TestStreamDetailFailurePreservesSharedBudgetExhaustion(t *testing.T) {
	backend := newFakeBackend(t)
	backend.respond = func(w http.ResponseWriter, _ backendCall, _ int) {
		w.Header().Set("Retry-After-Ms", "0")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"detail":"service unavailable"}`)
	}
	provider := newTestProvider(t, backend, newFakeLogin())
	ctx := retry.WithBudget(context.Background(), retry.New(retry.Policy{MaxRetries: 1, InitialDelay: time.Nanosecond, MaxDelay: time.Nanosecond}))
	request := userRequest("gpt-5.5", "hi")
	core := streaming.NewStreamingCore(ctx, make(chan messages.ChatMessage, 10), newAdapter(request.Model))
	err := provider.stream(ctx, request, core)
	var exhausted *retry.ExhaustedError
	var apiErr *openai.APIError
	if !errors.As(err, &exhausted) || !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("stream description lost exhaustion: %T %v", err, err)
	}
	if exhausted.RetryCount != 1 || exhausted.Attempts != 2 || exhausted.NextRetryAt.IsZero() || backend.count() != 2 {
		t.Fatalf("retry metadata changed: exhausted=%+v requests=%d", exhausted, backend.count())
	}
	if err.Error() != "codex: service unavailable (HTTP 503)" {
		t.Fatalf("backend detail not described: %v", err)
	}
}
