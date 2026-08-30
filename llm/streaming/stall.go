package streaming

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// StallError is the cancellation cause recorded when a stream produced no
// provider data for the configured stall timeout. The watchdog bounds
// silence, not total stream length: healthy long generations keep resetting
// it, while a hung connection or an unresponsive provider is cut off.
type StallError struct{ Timeout time.Duration }

func (e *StallError) Error() string {
	return fmt.Sprintf("stream stalled: no data from provider for %s", e.Timeout)
}

// DeadlineError is the cancellation cause recorded when a request exceeded
// its hard wall-clock deadline. Unlike the stall timeout, arriving data does
// not push it out — it caps total call duration, catching endpoints that
// trickle keepalive data forever without ever finishing.
type DeadlineError struct{ Deadline time.Duration }

func (e *DeadlineError) Error() string {
	return fmt.Sprintf("request exceeded the %s deadline", e.Deadline)
}

// WatchdogCause returns the stall or deadline error that canceled ctx, or nil
// when ctx is alive or was canceled for any other reason. These causes must
// be surfaced deterministically: the watchdog canceled the stream itself, so
// no provider error can be relied on to tell the story.
func WatchdogCause(ctx context.Context) error {
	cause := context.Cause(ctx)
	var stall *StallError
	if errors.As(cause, &stall) {
		return stall
	}
	var deadline *DeadlineError
	if errors.As(cause, &deadline) {
		return deadline
	}
	return nil
}
