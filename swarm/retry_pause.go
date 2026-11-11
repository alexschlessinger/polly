package swarm

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/subagent"
)

// RetryPause records exhausted transient recovery without authorizing another
// request. NextRetryAt is an eligibility boundary, never a scheduler deadline.
type RetryPause struct {
	Reason      string    `json:"reason"`
	Attempts    int       `json:"attempts"`
	RetryCount  int       `json:"retryCount"`
	NextRetryAt time.Time `json:"nextRetryAt,omitzero"`
}

// RetryPauseError is returned only after the session's accepted checkpoint and
// pause receipt are confirmed. Execution is empty for a parent turn. The
// underlying provider error remains inspectable until the receipt is reopened.
type RetryPauseError struct {
	Session   string
	Execution string
	Pause     RetryPause
	Err       error
}

func (e *RetryPauseError) Error() string {
	return fmt.Sprintf("session %s paused: %s; saved work is retained", e.Session, e.Pause.description())
}
func (e *RetryPauseError) Unwrap() error        { return e.Err }
func (e *RetryPauseError) Is(target error) bool { return target == subagent.ErrRetryPaused }

// A request-local HTTP timeout may exhaust retries while the root context is
// still live. Only cancellation outside that exhausted wrapper is a root stop.
func retryCanceled(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := err.(*llm.RetryExhaustedError); ok {
		return errors.Is(err, context.Canceled)
	}
	if err == context.Canceled || err == context.DeadlineExceeded {
		return true
	}
	switch e := err.(type) {
	case interface{ Unwrap() []error }:
		for _, cause := range e.Unwrap() {
			if retryCanceled(cause) {
				return true
			}
		}
	case interface{ Unwrap() error }:
		return retryCanceled(e.Unwrap())
	}
	return false
}

func retryPauseFromError(err error) *RetryPause {
	// Cancellation always wins, including when joined with an exhausted retry.
	if retryCanceled(err) {
		return nil
	}
	var exhausted *llm.RetryExhaustedError
	if !errors.As(err, &exhausted) {
		return nil
	}
	reason := exhausted.Error()
	if exhausted.Err != nil {
		reason = exhausted.Err.Error()
	}
	return &RetryPause{Reason: reason, Attempts: exhausted.Attempts, RetryCount: exhausted.RetryCount, NextRetryAt: exhausted.NextRetryAt}
}

// checkpointRetryPause commits the task disposition and result identity with
// the accepted prefix. A crash before finish must not leave a running task or
// require another model call to publish the pause notice.
func (r *Runtime) checkpointRetryPause(s *State, e *Execution, pause *RetryPause, accepted []messages.ChatMessage, cause error) {
	m := s.Members[e.Member]
	if m == nil || m.Execution != e.ID || m.Control == MemberControlStopped {
		return
	}
	e.Status, e.RetryPause, e.Error = "paused", pause, cause.Error()
	result := &AgentResult{Session: m.ID, Execution: e.ID, Context: e.Workspace, Usage: e.Usage}
	for _, message := range accepted {
		if message.Role == messages.MessageRoleAssistant && message.Content != "" {
			result.Value = message.Content
		}
	}
	task := s.Tasks[m.Task]
	if task != nil && task.Owner == m.ID && task.Execution == e.ID {
		if task.Status == "running" {
			task = s.ownTask(task.ID)
			task.Status, task.Feedback = "blocked", e.Error
			if result.Value != nil {
				task.Result = result.Value
			}
			task.Revision++
		}
		result.Task, result.Revision = task.ID, task.Revision
	}
	e.Result = result
	if !workflowControlled(s, e) {
		mail := r.completionNotice(s, m, e, task)
		s.Messages[mail.ID] = mail
	}
}

func (p *RetryPause) resumeError() error {
	if p == nil || !time.Now().Before(p.NextRetryAt) {
		return nil
	}
	return fail("retry_not_ready", fmt.Sprintf("retry budget exhausted; explicit continuation is available at %s (no automatic retry)", p.NextRetryAt.UTC().Format(time.RFC3339Nano)))
}

func (p *RetryPause) description() string {
	text := fmt.Sprintf("retry budget exhausted after %d attempts: %s", p.Attempts, p.Reason)
	if !p.NextRetryAt.IsZero() {
		text += "; eligible for explicit continuation at " + p.NextRetryAt.UTC().Format(time.RFC3339Nano)
	}
	return text + "; no automatic retry"
}

// A root may not have launched any members, so prepare has not necessarily
// promoted its one-shot memory store. Do this before committing a retry pause,
// never after reporting that the turn can be recovered from disk.
func (r *Runtime) promoteRetryPause(ctx context.Context) error {
	if r.config.Promote != nil {
		if err := r.config.Promote(ctx); err != nil {
			return fmt.Errorf("save recoverable retry pause: %w", err)
		}
	}
	if store, ok := r.config.Store.(sessions.DurableStore); ok {
		if mode, _ := store.Location(); mode == sessions.ModeMemory {
			return errors.New("cannot save recoverable retry pause: session storage is still in memory")
		}
	}
	return nil
}
