// Package retry provides a bounded budget shared by nested retry loops. It
// bounds recovery from failures, not the duration of a healthy request or stream.
package retry

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"
)

// Policy bounds retries after the initial attempt. MaxRetries zero disables
// retries; negative values are treated as zero. Nonpositive durations use the
// corresponding DefaultPolicy values, so a budget is always bounded.
type Policy struct {
	MaxRetries   int
	MaxElapsed   time.Duration
	InitialDelay time.Duration
	MaxDelay     time.Duration
}

func DefaultPolicy() Policy {
	return Policy{MaxRetries: 12, MaxElapsed: 5 * time.Minute, InitialDelay: 500 * time.Millisecond, MaxDelay: 30 * time.Second}
}

// ExhaustedError preserves the last failure and the earliest time another
// retry would have been scheduled. RetryCount counts granted retries, including
// a wait interrupted before its attempt; Attempts is RetryCount plus the initial
// attempt. Neither count includes the retry that was refused.
type ExhaustedError struct {
	Err         error
	Attempts    int
	RetryCount  int
	NextRetryAt time.Time
}

func (e *ExhaustedError) Error() string {
	return fmt.Sprintf("retry budget exhausted after %d retries: %v", e.RetryCount, e.Err)
}

func (e *ExhaustedError) Unwrap() error   { return e.Err }
func (e *ExhaustedError) Retryable() bool { return false }

// RetryEvent describes an accepted retry. Waiting is true before the wait and
// false when the wait ends, including cancellation. RetryCount is one-based:
// the first retry follows the initial attempt and has count 1.
type RetryEvent struct {
	Waiting     bool
	Err         error
	RetryCount  int
	Delay       time.Duration
	NextRetryAt time.Time
	// Source identifies the layer that requested the retry (http or stream).
	// WaitError is populated on the ending event if the wait was interrupted
	// or the budget expired before another attempt could start.
	Source    string
	WaitError error
	Canceled  bool
}

type sourceKey struct{}

func WithSource(ctx context.Context, source string) context.Context {
	return context.WithValue(ctx, sourceKey{}, source)
}

// Budget shares retry count and elapsed time across callers. It is safe for
// concurrent use. The elapsed clock starts on the first failure passed to Wait;
// no deadline is installed on the caller's context.
type Budget struct {
	mu      sync.Mutex
	policy  Policy
	started time.Time
	retries int
	now     func() time.Time
	sleep   func(context.Context, time.Duration) error
	jitter  func(time.Duration) time.Duration
}

func New(policy Policy) *Budget {
	defaults := DefaultPolicy()
	policy.MaxRetries = max(policy.MaxRetries, 0)
	if policy.MaxElapsed <= 0 {
		policy.MaxElapsed = defaults.MaxElapsed
	}
	if policy.InitialDelay <= 0 {
		policy.InitialDelay = defaults.InitialDelay
	}
	if policy.MaxDelay <= 0 {
		policy.MaxDelay = defaults.MaxDelay
	}
	return &Budget{policy: policy, now: time.Now, sleep: sleep, jitter: jitter}
}

type budgetKey struct{}
type observerKey struct{}

func WithBudget(ctx context.Context, budget *Budget) context.Context {
	return context.WithValue(ctx, budgetKey{}, budget)
}

func FromContext(ctx context.Context) *Budget {
	budget, _ := ctx.Value(budgetKey{}).(*Budget)
	return budget
}

// WithObserver reports accepted retries synchronously before waiting. Observers
// run outside the budget lock and should return promptly. Callers sharing a
// budget may invoke their context's observers concurrently.
func WithObserver(ctx context.Context, observer func(RetryEvent)) context.Context {
	return context.WithValue(ctx, observerKey{}, observer)
}

// Observer returns the installed observer so request wrappers can compose it
// with local bookkeeping without hiding the host's retry notifications.
func Observer(ctx context.Context) func(RetryEvent) {
	observer, _ := ctx.Value(observerKey{}).(func(RetryEvent))
	return observer
}

// Wait reserves one retry and waits until it may begin. The caller decides
// whether err is retryable. A negative retryAfter means no server hint; zero is
// a valid hint. Nonnegative hints are minimum delays, never capped or jittered
// down, and exhaust the budget immediately if they do not fit MaxElapsed.
// Already-exhausted errors pass through unchanged to prevent nested retries.
func (b *Budget) Wait(ctx context.Context, err error, retryAfter time.Duration) (waitErr error) {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var exhausted *ExhaustedError
	if errors.As(err, &exhausted) {
		return err
	}

	b.mu.Lock()
	now := b.now()
	if b.started.IsZero() {
		b.started = now
	}
	delay := min(b.policy.InitialDelay, b.policy.MaxDelay)
	for n := 0; n < b.retries && delay < b.policy.MaxDelay; n++ {
		if delay > b.policy.MaxDelay/2 {
			delay = b.policy.MaxDelay
		} else {
			delay *= 2
		}
	}
	delay = max(b.jitter(delay), retryAfter)
	next := now.Add(delay)
	remaining := b.policy.MaxElapsed - now.Sub(b.started)
	if b.retries >= b.policy.MaxRetries || remaining <= 0 || delay >= remaining {
		exhausted = b.exhausted(err, next)
		b.mu.Unlock()
		return exhausted
	}
	b.retries++
	event := RetryEvent{Waiting: true, Err: err, RetryCount: b.retries, Delay: delay, NextRetryAt: next}
	event.Source, _ = ctx.Value(sourceKey{}).(string)
	b.mu.Unlock()

	if observer, _ := ctx.Value(observerKey{}).(func(RetryEvent)); observer != nil {
		observer(event)
		defer func() {
			event.Waiting = false
			event.WaitError = waitErr
			if ctx.Err() != nil {
				event.Canceled = true
				event.WaitError = context.Cause(ctx)
			}
			observer(event)
		}()
	}
	if err := b.sleep(ctx, max(next.Sub(b.now()), 0)); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// A slow observer or scheduler must not launch a retry after the elapsed
	// budget. Successful in-flight requests, however, are never interrupted.
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.now().Sub(b.started) >= b.policy.MaxElapsed {
		return b.exhausted(err, next)
	}
	return nil
}

// exhausted requires b.mu to be held.
func (b *Budget) exhausted(err error, next time.Time) *ExhaustedError {
	return &ExhaustedError{Err: err, Attempts: b.retries + 1, RetryCount: b.retries, NextRetryAt: next}
}

func jitter(delay time.Duration) time.Duration {
	// Equal jitter avoids synchronized retries without near-zero busy loops.
	low := delay / 2
	return low + time.Duration(rand.Int64N(int64(delay-low)+1))
}

func sleep(ctx context.Context, delay time.Duration) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}
