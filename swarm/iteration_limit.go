package swarm

import (
	"fmt"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
)

// IterationLimitError is a recoverable pause with saved execution identity.
// The host can continue it with ResumeWithIterations after an explicit grant.
type IterationLimitError struct {
	Session   string
	Execution string
	Used      int
	Limit     int
}

func (e *IterationLimitError) Error() string {
	return fmt.Sprintf("member %s paused: iteration limit reached (%d/%d model calls); saved work is retained. Grant additional calls with /swarm resume %s N", e.Session, e.Used, e.Limit, e.Session)
}

func (e *IterationLimitError) Unwrap() error { return llm.ErrMaxIterations }

func (e *Execution) iterationLimitError() *IterationLimitError {
	return &IterationLimitError{Session: e.Member, Execution: e.ID, Used: e.Iterations, Limit: e.Request.MaxIterations}
}

// Older swarm records classified a plain iteration limit as a failure. Only
// migrate that exact error; mixed failures still require failure recovery.
func (s *State) normalizeIterationLimits() {
	for _, e := range s.Executions {
		if e.Status == "failed" && e.Error == llm.ErrMaxIterations.Error() {
			e.Status = "paused"
			e.StopReason = messages.StopReasonMaxIterations
			e.Error = e.iterationLimitError().Error()
		}
	}
}
