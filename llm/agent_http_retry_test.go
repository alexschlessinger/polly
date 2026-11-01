package llm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/alexschlessinger/pollytool/llm/internal/httpx"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestAgentHTTPAndStreamFailuresShareOneBudget(t *testing.T) {
	var sends atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sends.Add(1) != 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	providerCalls := 0
	model := retryStreamFunc(func(ctx context.Context, _ *CompletionRequest) <-chan *messages.StreamEvent {
		providerCalls++
		retrier := httpx.Retrier{Client: server.Client(), MaxRetries: 20, Prefix: "test", ErrorFromResponse: func(r *http.Response) error { return errors.New(r.Status) }}
		response, err := retrier.Do(ctx, func() (*http.Request, error) {
			return http.NewRequestWithContext(ctx, http.MethodPost, server.URL, nil)
		})
		if err == nil {
			response.Body.Close()
			err = io.ErrUnexpectedEOF
		}
		return retryEvents(&messages.StreamEvent{Type: messages.EventTypeError, Error: err})
	})
	agent := NewAgent(model, nil, AgentConfig{})
	defer agent.Close()
	var waiting, finished []int
	_, err := agent.Run(context.Background(), &CompletionRequest{RetryPolicy: fastRetryPolicy(3)}, &AgentCallbacks{OnRetry: func(e RetryEvent) {
		if e.Waiting {
			waiting = append(waiting, e.RetryCount)
		} else {
			finished = append(finished, e.RetryCount)
		}
	}})
	var exhausted *RetryExhaustedError
	if !errors.As(err, &exhausted) || exhausted.RetryCount != 3 || exhausted.Attempts != 4 || sends.Load() != 4 || providerCalls != 2 {
		t.Fatalf("nested budget multiplied: http=%d provider=%d exhausted=%+v err=%v", sends.Load(), providerCalls, exhausted, err)
	}
	if !reflect.DeepEqual(waiting, []int{1, 2, 3}) || !reflect.DeepEqual(finished, waiting) {
		t.Fatalf("HTTP/stream visibility waiting=%v finished=%v", waiting, finished)
	}
}
