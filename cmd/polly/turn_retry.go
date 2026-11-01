package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/swarm"
)

// attemptTurnUI is deliberately optional, not a no-op on turnUIBase. A
// non-nil abort callback opts the caller into replay after displayed output.
type attemptTurnUI interface {
	SupportsAttemptRecovery() bool
	ModelAttemptStarted(iteration, attempt int)
	ModelAttemptAborted(iteration, attempt int, err error)
}

type retryStatusUI interface {
	ModelRetry(llm.RetryEvent)
}

// modelRetryStatus is UI-owned and locked by the surface. OnRetry can run on
// a provider goroutine; render ticks compute countdowns without transcript writes.
type modelRetryStatus struct {
	waiting bool
	count   int
	next    time.Time
	reason  string
}

func (s *modelRetryStatus) update(event llm.RetryEvent) {
	*s = modelRetryStatus{}
	if !event.Waiting {
		return
	}
	s.waiting, s.count, s.next = true, event.RetryCount, event.NextRetryAt
	if event.Err != nil {
		s.reason = style.Truncate(strings.Join(strings.Fields(cleanActivityText(event.Err.Error())), " "), 160)
	}
}

func (s modelRetryStatus) label(now time.Time) string {
	label := fmt.Sprintf("retry %d in %s", s.count, activityElapsed(max(0, s.next.Sub(now))+time.Second-1))
	if s.reason != "" {
		label += " · " + s.reason
	}
	return label
}

const retryPausedLabel = "paused · retry budget exhausted"
const retryPauseMetadataKey = "retry_budget_exhausted"

// retryPauseError accepts only a confirmed durable pause, never raw retry
// exhaustion or a joined persistence/output failure. Cancellation always wins.
func retryPauseError(err error) bool {
	if activityCanceled(err) {
		return false
	}
	if _, ok := err.(*swarm.RetryPauseError); ok {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !retryPauseError(child) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return retryPauseError(wrapped.Unwrap())
	}
	return false
}

func discardedAttemptLabel(attempt int) string {
	return fmt.Sprintf("discarded attempt %d · not saved", attempt+1)
}
