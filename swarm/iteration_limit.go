package swarm

import (
	"fmt"

	"github.com/alexschlessinger/pollytool/llm"
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
