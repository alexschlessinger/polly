package main

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/alexschlessinger/pollytool/internal/ids"
	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

// turnAudit collects observations without writing during the agent run:
// swarm checkpoints own the transcript sequence fence until the run settles.
type turnAudit struct {
	mu     sync.Mutex
	record messages.TurnAudit
}

func newTurnAudit() *turnAudit {
	id := ids.New()
	return &turnAudit{record: messages.TurnAudit{Version: 1, ID: id, InputTurnID: id, StartedAt: time.Now().UTC()}}
}

func (a *turnAudit) input(msg messages.ChatMessage) messages.ChatMessage {
	msg = msg.Clone()
	if msg.Metadata == nil {
		msg.Metadata = make(map[string]any)
	}
	msg.Metadata[messages.MetadataKeyTurnID] = a.record.ID
	msg.Metadata[messages.MetadataKeyTurnStartedAt] = a.record.StartedAt.Format(time.RFC3339Nano)
	return msg
}

func (a *turnAudit) modelAttempt(event llm.ModelAttemptEvent) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !event.Finished {
		a.record.Attempts = append(a.record.Attempts, messages.ModelAttemptAudit{
			ID: event.ID, Model: event.Model, Purpose: event.Purpose, Iteration: event.Iteration, Attempt: event.Attempt, StartedAt: event.At,
		})
		return
	}
	for i := len(a.record.Attempts) - 1; i >= 0; i-- {
		attempt := &a.record.Attempts[i]
		if attempt.ID == event.ID {
			at := event.At
			attempt.FinishedAt, attempt.StopReason = &at, event.StopReason
			attempt.Error, attempt.InterruptionOrigin = auditError(event.Err), cancellationOrigin(event.Err, event.Canceled)
			return
		}
	}
}

func (a *turnAudit) retry(event llm.RetryEvent) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.record.Retries = append(a.record.Retries, messages.RetryAudit{
		At: time.Now().UTC(), Waiting: event.Waiting, Source: event.Source, RetryCount: event.RetryCount,
		DelayMillis: event.Delay.Milliseconds(), NextRetryAt: event.NextRetryAt.UTC(), Error: auditError(event.Err),
		WaitError: auditError(event.WaitError), InterruptionOrigin: cancellationOrigin(event.WaitError, event.Canceled),
	})
}

func (a *turnAudit) finish(ctx context.Context, err error) messages.TurnAudit {
	a.mu.Lock()
	defer a.mu.Unlock()
	record := a.record
	// Provider retry observers may finish draining after cancellation. The
	// persisted record is an immutable snapshot of observations at settlement.
	record.Attempts = append([]messages.ModelAttemptAudit(nil), record.Attempts...)
	record.Retries = append([]messages.RetryAudit(nil), record.Retries...)
	record.RunFinishedAt = time.Now().UTC()
	record.Error, record.InterruptionOrigin = auditError(err), cancellationOrigin(err, ctx.Err() != nil)
	if ctx.Err() != nil {
		// Prefer the root cause over a concurrent request-local timeout, but
		// keep explicit signal/lease/stop errors if the context is generic.
		switch record.InterruptionOrigin {
		case "", "context_deadline", "external_context_unknown", "context_canceled_unknown":
			record.InterruptionOrigin = cancellationOrigin(context.Cause(ctx), true)
		}
	}
	switch {
	case err == nil:
		record.Status = "completed"
	case record.InterruptionOrigin != "":
		record.Status = "interrupted"
	case retryPauseError(err):
		record.Status = "retry_exhausted"
	default:
		record.Status = "failed"
	}
	return record
}

func auditError(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// All interactive tab stop paths (Ctrl-C, stop, quit and detach cleanup)
// share this cause. Do not claim which gesture was used, or label an arbitrary
// caller's context.Canceled as a user action.
var errInteractiveTurnStopped = interactiveTurnStop{}

type interactiveTurnStop struct{}

func (interactiveTurnStop) Error() string { return "interactive turn stopped: context canceled" }
func (interactiveTurnStop) Unwrap() error { return context.Canceled }

func interruptionOrigin(err error) string {
	var signal *shutdownSignal
	switch {
	case err == nil:
		return ""
	case errors.Is(err, errInteractiveTurnStopped):
		return "interactive_stop"
	case errors.As(err, &signal):
		return "process_signal"
	case errors.Is(err, sessions.ErrSessionLeaseLost):
		return "session_lease_lost"
	case errors.Is(err, sessions.ErrStoreClosed):
		return "session_store_closed"
	case errors.Is(err, context.DeadlineExceeded):
		return "context_deadline"
	case errors.Is(err, context.Canceled):
		return "external_context_unknown"
	}
	return ""
}

func cancellationOrigin(err error, canceled bool) string {
	origin := interruptionOrigin(err)
	// Provider/request deadlines can expire while the owning turn remains
	// healthy. They are failures eligible for retry, not turn interruptions.
	if origin == "context_deadline" && !canceled {
		return ""
	}
	if origin == "external_context_unknown" && !canceled {
		return "context_canceled_unknown"
	}
	if origin == "" && canceled {
		return "external_context_unknown"
	}
	return origin
}

func isTurnAuditOnly(msg messages.ChatMessage) bool {
	return msg.Role == messages.MessageRoleInternal && len(msg.Metadata) == 1 && msg.Metadata[messages.MetadataKeyTurnAudit] != nil
}
