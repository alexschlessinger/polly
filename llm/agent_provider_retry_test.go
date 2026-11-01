package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm/anthropic"
	"github.com/alexschlessinger/pollytool/llm/openai"
	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/tools"
)

func TestAgentRealProviderPreservesHTTPExhaustion(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error":{"message":"busy","type":"server_error"}}`)
	}))
	defer server.Close()
	agent := NewAgent(openai.NewProvider("key", server.URL), nil, AgentConfig{})
	defer agent.Close()
	_, err := agent.Run(context.Background(), &CompletionRequest{Model: "test-model", RetryPolicy: fastRetryPolicy(2)}, nil)
	var exhausted *RetryExhaustedError
	var apiErr *openai.APIError
	if !errors.As(err, &exhausted) || !errors.As(err, &apiErr) || exhausted.Attempts != 3 || calls.Load() != 3 {
		t.Fatalf("provider pipeline lost recovery cause: calls=%d err=%T %v", calls.Load(), err, err)
	}
}

func TestAgentRealProviderRetriesIncompleteToolsWithoutExecutingThem(t *testing.T) {
	var calls, executed atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"incomplete","type":"function","function":{"name":"work","arguments":"{}"}}]}}]}`+"\n\n")
		// Graceful EOF with parseable arguments is still not a terminal model
		// response. Retrying must neither dispatch nor journal this tool call.
	}))
	defer server.Close()
	registry := tools.NewToolRegistry([]tools.Tool{&tools.Func{Name: "work", Run: func(context.Context, tools.Args) (string, error) { executed.Add(1); return "must not run", nil }}})
	defer registry.Close()
	agent := NewAgent(openai.NewProvider("key", server.URL), registry, AgentConfig{})
	defer agent.Close()
	response, err := agent.Run(context.Background(), &CompletionRequest{Model: "test-model", RetryPolicy: fastRetryPolicy(1)}, nil)
	var exhausted *RetryExhaustedError
	if !errors.As(err, &exhausted) || !errors.Is(err, streaming.ErrStreamEndedEarly) || calls.Load() != 2 || executed.Load() != 0 || len(response.AllMessages) != 0 {
		t.Fatalf("incomplete tools recovered unsafely: calls=%d tools=%d response=%+v err=%v", calls.Load(), executed.Load(), response, err)
	}
}

func TestAgentRealProviderRetriesStallAndOverload(t *testing.T) {
	for _, stall := range []bool{false, true} {
		t.Run(fmt.Sprint(stall), func(t *testing.T) {
			var calls atomic.Int32
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				if stall {
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
					select {
					case <-r.Context().Done():
					case <-release:
					}
					return
				}
				fmt.Fprint(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"busy\"}}\n\n")
			}))
			defer server.Close()
			defer close(release)
			agent := NewAgent(anthropic.NewProvider("key", server.URL), nil, AgentConfig{})
			defer agent.Close()
			_, err := agent.Run(context.Background(), &CompletionRequest{Model: "test-model", RetryPolicy: fastRetryPolicy(1), Timeout: 30 * time.Millisecond}, nil)
			var exhausted *RetryExhaustedError
			if !errors.As(err, &exhausted) || calls.Load() != 2 {
				t.Fatalf("typed provider failure did not recover: calls=%d err=%T %v", calls.Load(), err, err)
			}
			if stall {
				var timeout *streaming.StallError
				if !errors.As(err, &timeout) {
					t.Fatalf("stall cause lost: %v", err)
				}
			} else {
				var overload *anthropic.APIError
				if !errors.As(err, &overload) {
					t.Fatalf("overload cause lost: %v", err)
				}
			}
		})
	}
}
