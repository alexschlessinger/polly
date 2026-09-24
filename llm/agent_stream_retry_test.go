package llm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"syscall"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

// flakyStreamLLM fails its first failures streams with err, optionally after
// emitting one content delta, then answers normally.
type flakyStreamLLM struct {
	failures  int
	err       error
	emitFirst bool
	calls     int
}

func (f *flakyStreamLLM) ChatCompletionStream(_ context.Context, _ *CompletionRequest, _ EventStreamProcessor) <-chan *messages.StreamEvent {
	f.calls++
	failing := f.calls <= f.failures
	events := make(chan *messages.StreamEvent, 3)
	if failing {
		if f.emitFirst {
			events <- &messages.StreamEvent{Type: messages.EventTypeContent, Content: "partial"}
		}
		events <- &messages.StreamEvent{Type: messages.EventTypeError, Error: f.err}
		close(events)
		return events
	}
	events <- &messages.StreamEvent{Type: messages.EventTypeComplete, Message: &messages.ChatMessage{
		Role:       messages.MessageRoleAssistant,
		Content:    "done",
		StopReason: messages.StopReasonEndTurn,
	}}
	close(events)
	return events
}

// A provider stream that dies before showing anything is re-sent; one that
// already handed deltas to the caller, or failed for a reason the transport
// did not cause, is not.
func TestAgentRetriesTransientStreamFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		failures  int
		err       error
		emitFirst bool
		withCB    bool
		wantCalls int
		wantErr   bool
	}{
		{name: "reset before any delta is re-sent", failures: 1, err: syscall.ECONNRESET, withCB: true, wantCalls: 2},
		{name: "no callbacks means nothing was shown", failures: 1, err: syscall.ECONNRESET, emitFirst: true, wantCalls: 2},
		{name: "reset after a shown delta stands", failures: 1, err: syscall.ECONNRESET, emitFirst: true, withCB: true, wantCalls: 1, wantErr: true},
		{name: "provider refusal is not transient", failures: 1, err: errors.New("model refused the request"), withCB: true, wantCalls: 1, wantErr: true},
		{name: "budget is bounded", failures: 9, err: syscall.ECONNRESET, withCB: true, wantCalls: streamRetries + 1, wantErr: true},
		// The request layer retries a request it could not send and reports
		// the last failure as a *url.Error; sending it again would multiply
		// its budget.
		{name: "a request the transport gave up on stands", failures: 1, err: &url.Error{Op: "Post", URL: "https://api.example", Err: syscall.ECONNRESET}, withCB: true, wantCalls: 1, wantErr: true},
		{name: "a DNS failure stands", failures: 1, err: &url.Error{Op: "Post", URL: "https://api.example", Err: &net.DNSError{Err: "no such host", IsNotFound: true}}, withCB: true, wantCalls: 1, wantErr: true},
		// HTTP/2 resets the stream, not the connection, when a body dies.
		{name: "an HTTP/2 stream reset is re-sent", failures: 1, err: fmt.Errorf("anthropic: reading stream: %w", errors.New("stream error: stream ID 7; INTERNAL_ERROR; received from peer")), withCB: true, wantCalls: 2},
		{name: "a GOAWAY is re-sent", failures: 1, err: errors.New("http2: server sent GOAWAY and closed the connection; LastStreamID=3, ErrCode=NO_ERROR, debug=\"\""), withCB: true, wantCalls: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := &flakyStreamLLM{failures: tc.failures, err: tc.err, emitFirst: tc.emitFirst}
			agent := NewAgent(model, tools.NewToolRegistry(nil), AgentConfig{MaxIterations: 2})
			reported := 0
			cb := &AgentCallbacks{}
			var attempts [][2]int
			cb.OnModelRequest = func(iteration, attempt int) { attempts = append(attempts, [2]int{iteration, attempt}) }
			if tc.withCB {
				cb.OnContent = func(string) {}
				cb.OnError = func(error) { reported++ }
			}
			_, err := agent.Run(context.Background(), &CompletionRequest{}, cb)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Run() error = %v, want error %v", err, tc.wantErr)
			}
			if model.calls != tc.wantCalls {
				t.Fatalf("provider calls = %d, want %d", model.calls, tc.wantCalls)
			}
			if len(attempts) != model.calls {
				t.Fatalf("request activity missed retries: %v, calls=%d", attempts, model.calls)
			}
			for n, attempt := range attempts {
				if attempt != [2]int{0, n} {
					t.Fatalf("request attempt %d: %v", n, attempt)
				}
			}
			// A re-sent attempt is invisible: only a final failure reports.
			if tc.withCB {
				want := 0
				if tc.wantErr {
					want = 1
				}
				if reported != want {
					t.Fatalf("OnError calls = %d, want %d", reported, want)
				}
			}
		})
	}
}

// Esc during the backoff between attempts is an interrupt: the run reports
// the cancellation, not the transport error it was about to retry, and
// nothing reaches OnError.
func TestAgentCancelledDuringStreamRetryBackoffReportsTheInterrupt(t *testing.T) {
	model := &flakyStreamLLM{failures: 9, err: syscall.ECONNRESET}
	agent := NewAgent(model, tools.NewToolRegistry(nil), AgentConfig{MaxIterations: 2})
	reported := 0
	cb := &AgentCallbacks{OnContent: func(string) {}, OnError: func(error) { reported++ }}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err := agent.Run(ctx, &CompletionRequest{}, cb)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want the cancellation", err)
	}
	if model.calls != 1 {
		t.Fatalf("provider calls = %d, want 1", model.calls)
	}
	if reported != 0 {
		t.Fatalf("OnError calls = %d, want 0", reported)
	}
}
