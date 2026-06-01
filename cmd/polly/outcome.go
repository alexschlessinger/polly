package main

import (
	"errors"
	"fmt"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
)

// exitError carries the process exit code a run should produce. main() unwraps
// it; any other error maps to exit 1.
type exitError struct {
	code int
	err  error // optional; nil for a nonzero-but-not-an-error outcome (e.g. truncation)
}

func (e *exitError) Error() string {
	if e.err != nil {
		return e.err.Error()
	}
	return fmt.Sprintf("exit status %d", e.code)
}

func (e *exitError) Unwrap() error { return e.err }

// classifyOutcome maps an agent run's (response, error) to a terminal stop
// reason and the process exit code per the documented contract:
// 0 end_turn, 2 max_tokens, 3 max_iterations, 1 hard error.
func classifyOutcome(resp *llm.AgentResponse, err error) (messages.StopReason, int) {
	switch {
	case errors.Is(err, llm.ErrMaxIterations):
		return messages.StopReasonMaxIterations, 3
	case err != nil:
		return messages.StopReasonError, 1
	case resp != nil && resp.Message != nil && resp.Message.StopReason == messages.StopReasonMaxTokens:
		return messages.StopReasonMaxTokens, 2
	case resp != nil && resp.Message != nil && resp.Message.StopReason != "":
		return resp.Message.StopReason, 0
	default:
		return messages.StopReasonEndTurn, 0
	}
}
