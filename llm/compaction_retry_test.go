package llm

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
)

func TestSummaryRetryRetainsPolicyAndFailedUsage(t *testing.T) {
	policy := fastRetryPolicy(1)
	req := &CompletionRequest{Model: "test/model", RetryPolicy: policy}
	ask := summaryRequest(req, req.Model, "history", 100)
	if ask.RetryPolicy != policy {
		t.Fatal("summary lost retry policy")
	}
	calls := 0
	model := retryStreamFunc(func(_ context.Context, req *CompletionRequest) <-chan *messages.StreamEvent {
		calls++
		if calls == 1 {
			return retryEvents(&messages.StreamEvent{Type: messages.EventTypeUsage, InputTokens: 10, OutputTokens: 5}, &messages.StreamEvent{Type: messages.EventTypeError, Error: io.ErrUnexpectedEOF})
		}
		return retryEvents(retryAnswer())
	})
	agent := NewAgent(model, nil, AgentConfig{})
	defer agent.Close()
	var failed []messages.ChatMessage
	response, err := agent.complete(context.Background(), ask, func(m messages.ChatMessage) { failed = append(failed, m) })
	if err != nil || response.Content != "accepted" || calls != 2 || len(failed) != 1 || !failed[0].IsUsageRecord() || failed[0].GetOutputTokens() != 5 {
		t.Fatalf("summary retry calls=%d response=%+v failed=%+v err=%v", calls, response, failed, err)
	}
}

func TestInPlaceSummaryExhaustionDoesNotRestartAsTranscript(t *testing.T) {
	model := &flakyStreamLLM{failures: 100, err: io.ErrUnexpectedEOF}
	agent := NewAgent(model, nil, AgentConfig{})
	defer agent.Close()
	req := &CompletionRequest{Model: "test/model", RetryPolicy: fastRetryPolicy(1)}
	run := agent.newRun(req, nil)
	_, fallback, err := run.summarizeInPlace(context.Background(), req, compactionPlan{target: 100})
	var exhausted *RetryExhaustedError
	if !errors.As(err, &exhausted) || fallback || model.calls != 2 {
		t.Fatalf("summary exhaustion fallback=%v calls=%d err=%v", fallback, model.calls, err)
	}
}
