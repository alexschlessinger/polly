package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alexschlessinger/pollytool/llm/internal/retry"
)

func TestAPIErrorRetryable(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  *APIError
		want bool
	}{
		{"nil", nil, false},
		{"unknown", &APIError{Message: "refused"}, false},
		{"bad request", &APIError{StatusCode: 400}, false},
		{"auth wins over type", &APIError{StatusCode: 401, Type: "rate_limit_error"}, false},
		{"quota", &APIError{StatusCode: 429, Type: "insufficient_quota"}, false},
		{"quota code wins over rate type", &APIError{StatusCode: 429, Type: "rate_limit_error", Code: "insufficient_quota"}, false},
		{"quota stream", &APIError{Code: "insufficient_quota"}, false},
		{"billing", &APIError{StatusCode: 429, Code: "billing_hard_limit_reached"}, false},
		{"gateway refusal", &APIError{StatusCode: 503, Metadata: json.RawMessage(`{"error_type":"context_length_exceeded"}`)}, false},
		{"rate status", &APIError{StatusCode: 429}, true},
		{"overload status", &APIError{StatusCode: 503}, true},
		{"timeout", &APIError{StatusCode: 408}, true},
		{"conflict", &APIError{StatusCode: 409}, true},
		{"rate stream", &APIError{Type: "rate_limit_error"}, true},
		{"overload stream", &APIError{Code: "overloaded_error"}, true},
		{"numeric stream code", &APIError{Code: "429"}, true},
		{"numeric terminal stream code", &APIError{Code: "402"}, false},
		{"gateway overload", &APIError{Metadata: json.RawMessage(`{"error_type":"server_error"}`)}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Retryable(); got != tc.want {
				t.Fatalf("Retryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestInsufficientQuotaDoesNotRetry(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(429)
		fmt.Fprint(w, `{"error":{"message":"buy credits","type":"insufficient_quota","code":"insufficient_quota"}}`)
	}))
	defer server.Close()
	client := NewClient("test", server.URL)
	ctx := retry.WithBudget(context.Background(), retry.New(retry.DefaultPolicy()))
	_, err := client.CreateChatCompletion(ctx, &ChatCompletionRequest{})
	var apiErr *APIError
	var exhausted *retry.ExhaustedError
	if !errors.As(err, &apiErr) || errors.As(err, &exhausted) || calls != 1 {
		t.Fatalf("quota was retried: calls=%d err=%v", calls, err)
	}
}
