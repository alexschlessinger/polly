package ollama

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm/internal/contract"
	"github.com/alexschlessinger/pollytool/llm/internal/retry"
	"github.com/alexschlessinger/pollytool/messages"
)

func ollamaRetryBudget(count int) *retry.Budget {
	return retry.New(retry.Policy{MaxRetries: count, InitialDelay: time.Nanosecond, MaxDelay: time.Nanosecond})
}

func TestStatusErrorRetryable(t *testing.T) {
	for _, status := range []int{0, 200, 400, 401, 402, 403, 404, 408, 409, 422, 429, 500, 502, 503, 504, 600} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			err := StatusError{StatusCode: status}
			want := status == 408 || status == 409 || status == 429 || status >= 500 && status < 600
			if got := err.Retryable(); got != want {
				t.Fatalf("Retryable(%d) = %v, want %v", status, got, want)
			}
		})
	}
}

func TestChatRetryBudgetAndTerminalErrors(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		shared    bool
		wantCalls int32
		exhausted bool
	}{
		{"standalone preserves no retries", 503, false, 1, false},
		{"shared transient exhausts", 503, true, 3, true},
		{"shared rate limit exhausts", 429, true, 3, true},
		{"bad request is terminal", 400, true, 1, false},
		{"authentication is terminal", 401, true, 1, false},
		{"missing model is terminal", 404, true, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Retry-After-Ms", "0")
				w.WriteHeader(tc.status)
				fmt.Fprint(w, `{"error":"unavailable"}`)
			})
			ctx := context.Background()
			budget := ollamaRetryBudget(2)
			if tc.shared {
				ctx = retry.WithBudget(ctx, budget)
			}
			err := client.Chat(ctx, &ChatRequest{Model: "m"}, func(ChatResponse) error {
				t.Error("HTTP failure reached response callback")
				return nil
			})
			var statusErr StatusError
			var exhausted *retry.ExhaustedError
			if !errors.As(err, &statusErr) || statusErr.StatusCode != tc.status || statusErr.ErrorMessage != "unavailable" {
				t.Fatalf("Chat = %v, want status %d", err, tc.status)
			}
			if errors.As(err, &exhausted) != tc.exhausted || calls.Load() != tc.wantCalls {
				t.Fatalf("Chat = %v, calls=%d, want exhausted=%v calls=%d", err, calls.Load(), tc.exhausted, tc.wantCalls)
			}
			if tc.shared && !tc.exhausted {
				for range 2 {
					if err := budget.Wait(ctx, io.ErrUnexpectedEOF, -1); err != nil {
						t.Fatalf("terminal error spent retry budget: %v", err)
					}
				}
			}
		})
	}
}

type ollamaRetryTransport func(*http.Request) (*http.Response, error)

func (fn ollamaRetryTransport) RoundTrip(req *http.Request) (*http.Response, error) { return fn(req) }

func TestChatRetriesTransportFailuresWithFreshRequests(t *testing.T) {
	request := &ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "hello"}}}
	payload, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var previous *http.Request
	calls := 0
	base, _ := url.Parse("https://example.invalid/prefix")
	client := NewClient(base, &http.Client{Transport: ollamaRetryTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		if req == previous {
			t.Error("retry reused its request")
		}
		previous = req
		body, err := io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil || string(body) != string(payload) {
			t.Errorf("attempt %d body=%q err=%v, want %q", calls, body, err, payload)
		}
		if req.Method != http.MethodPost || req.URL.Path != "/prefix/api/chat" || req.Header.Get("Content-Type") != "application/json" {
			t.Errorf("attempt %d lost request metadata: %+v", calls, req)
		}
		if calls < 3 {
			return nil, io.ErrUnexpectedEOF
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"message":{"content":"done"},"done":true}`))}, nil
	})})
	ctx := retry.WithBudget(context.Background(), ollamaRetryBudget(2))
	chunks := 0
	err = client.Chat(ctx, request, func(response ChatResponse) error {
		chunks++
		if !response.Done || response.Message.Content != "done" {
			t.Errorf("response = %+v", response)
		}
		return nil
	})
	if err != nil || calls != 3 || chunks != 1 {
		t.Fatalf("Chat = %v, calls=%d chunks=%d", err, calls, chunks)
	}
}

func TestChatCancellationDuringSharedRetryWait(t *testing.T) {
	var calls atomic.Int32
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(503)
		fmt.Fprint(w, `{"error":"busy"}`)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = retry.WithBudget(ctx, retry.New(retry.DefaultPolicy()))
	var events []retry.RetryEvent
	ctx = retry.WithObserver(ctx, func(event retry.RetryEvent) {
		events = append(events, event)
		if event.Waiting {
			cancel()
		}
	})
	err := client.Chat(ctx, &ChatRequest{Model: "m"}, func(ChatResponse) error { return nil })
	if !errors.Is(err, context.Canceled) || calls.Load() != 1 {
		t.Fatalf("Chat = %v, calls=%d, want cancellation after one call", err, calls.Load())
	}
	if len(events) != 2 || !events[0].Waiting || events[1].Waiting {
		t.Fatalf("retry did not report wait start/end: %+v", events)
	}
}

func TestProviderSharesHTTPBudgetAndPreservesAuthAndBody(t *testing.T) {
	var calls atomic.Int32
	var firstBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		if call == 1 {
			firstBody = string(body)
		} else if string(body) != firstBody {
			t.Errorf("attempt %d body changed: %q, want %q", call, body, firstBody)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("attempt %d lost headers: %v", call, r.Header)
		}
		if call < 3 {
			w.Header().Set("Retry-After-Ms", "0")
			w.WriteHeader(503)
			fmt.Fprint(w, `{"error":"busy"}`)
			return
		}
		fmt.Fprint(w, "{\"message\":{\"role\":\"assistant\",\"content\":\"done\"},\"done\":false}\n{\"done\":true}\n")
	}))
	defer server.Close()
	provider := NewProvider(server.URL, "test-key")
	ctx := retry.WithBudget(context.Background(), ollamaRetryBudget(2))
	var complete *messages.ChatMessage
	for event := range provider.ChatCompletionStream(ctx, &contract.CompletionRequest{Model: "m", Messages: messages.User("hello")}, messages.NewStreamProcessor()) {
		if event.Type == messages.EventTypeError {
			t.Fatalf("stream error: %v", event.Error)
		}
		if event.Type == messages.EventTypeComplete {
			complete = event.Message
		}
	}
	if calls.Load() != 3 || complete == nil || complete.Content != "done" {
		t.Fatalf("calls=%d complete=%+v", calls.Load(), complete)
	}
}
