package contract

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm/internal/retry"
	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestRunStreamBackoffPausesSilenceButNotDeadline(t *testing.T) {
	for _, deadline := range []time.Duration{0, 60 * time.Millisecond} {
		t.Run(deadline.String(), func(t *testing.T) {
			budget := retry.New(retry.Policy{MaxRetries: 1, MaxElapsed: time.Second, InitialDelay: time.Millisecond})
			ctx := retry.WithBudget(context.Background(), budget)
			var notifications []bool
			ctx = retry.WithObserver(ctx, func(e retry.RetryEvent) { notifications = append(notifications, e.Waiting) })
			started := time.Now()
			events := RunStream(ctx, 30*time.Millisecond, deadline, messages.NewStreamProcessor(), nopAdapter{}, func(ctx context.Context, core *streaming.StreamingCore) {
				if err := budget.Wait(ctx, errors.New("overloaded"), 100*time.Millisecond); err != nil {
					core.EmitError(err)
					return
				}
				core.EmitContent("recovered")
				core.SetStopReason(messages.StopReasonEndTurn)
				core.Complete()
			})
			got := collectStreamEvents(t, events, 3*time.Second)
			err := firstStreamError(got)
			if !reflect.DeepEqual(notifications, []bool{true, false}) {
				t.Fatalf("observer hidden: %v", notifications)
			}
			if deadline != 0 {
				var limit *streaming.DeadlineError
				if !errors.As(err, &limit) {
					t.Fatalf("backoff bypassed hard deadline or lost typed cause: %v", err)
				}
				return
			}
			if err != nil || time.Since(started) < 100*time.Millisecond {
				t.Fatalf("server wait cut short: %v elapsed=%s", err, time.Since(started))
			}
		})
	}
}

func TestRunStreamStallRearmsAfterBackoff(t *testing.T) {
	budget := retry.New(retry.Policy{MaxRetries: 1, MaxElapsed: time.Second, InitialDelay: time.Millisecond})
	ctx := retry.WithBudget(context.Background(), budget)
	events := RunStream(ctx, 30*time.Millisecond, 0, messages.NewStreamProcessor(), nopAdapter{}, func(ctx context.Context, core *streaming.StreamingCore) {
		if err := budget.Wait(ctx, errors.New("busy"), 60*time.Millisecond); err != nil {
			core.EmitError(err)
			return
		}
		<-ctx.Done()
	})
	got := collectStreamEvents(t, events, 3*time.Second)
	var stall *streaming.StallError
	if err := firstStreamError(got); !errors.As(err, &stall) {
		t.Fatalf("silence watchdog did not rearm: %v", err)
	}
}
