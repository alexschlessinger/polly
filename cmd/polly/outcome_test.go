package main

import (
	"errors"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
)

func resp(sr messages.StopReason) *llm.AgentResponse {
	return &llm.AgentResponse{Message: &messages.ChatMessage{StopReason: sr}}
}

func TestClassifyOutcome(t *testing.T) {
	cases := []struct {
		name     string
		resp     *llm.AgentResponse
		err      error
		wantStop messages.StopReason
		wantCode int
	}{
		{"end_turn", resp(messages.StopReasonEndTurn), nil, messages.StopReasonEndTurn, 0},
		{"max_tokens", resp(messages.StopReasonMaxTokens), nil, messages.StopReasonMaxTokens, 2},
		{"max_iterations", resp(messages.StopReasonMaxIterations), llm.ErrMaxIterations, messages.StopReasonMaxIterations, 3},
		{"hard_error", nil, errors.New("boom"), messages.StopReasonError, 1},
		{"nil_message", &llm.AgentResponse{}, nil, messages.StopReasonEndTurn, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotStop, gotCode := classifyOutcome(c.resp, c.err)
			if gotStop != c.wantStop || gotCode != c.wantCode {
				t.Fatalf("classifyOutcome = (%q, %d), want (%q, %d)", gotStop, gotCode, c.wantStop, c.wantCode)
			}
		})
	}
}

func TestExitError(t *testing.T) {
	inner := errors.New("api failed")
	e := &exitError{code: 1, err: inner}
	if !errors.Is(e, inner) {
		t.Fatal("exitError should unwrap to its inner error")
	}
	if e.Error() != "api failed" {
		t.Fatalf("Error() = %q, want %q", e.Error(), "api failed")
	}
	e2 := &exitError{code: 2}
	if e2.Error() == "" {
		t.Fatal("exitError with no inner error should still render a message")
	}
}
