package retry

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func testBudget(policy Policy) (*Budget, *time.Time, *[]time.Duration) {
	b := New(policy)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var delays []time.Duration
	b.now = func() time.Time { return now }
	b.jitter = func(delay time.Duration) time.Duration { return delay }
	b.sleep = func(ctx context.Context, delay time.Duration) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		delays = append(delays, delay)
		now = now.Add(delay)
		return nil
	}
	return b, &now, &delays
}

func TestDefaultPolicyAndNormalization(t *testing.T) {
	want := Policy{MaxRetries: 12, MaxElapsed: 5 * time.Minute, InitialDelay: 500 * time.Millisecond, MaxDelay: 30 * time.Second}
	if got := DefaultPolicy(); got != want {
		t.Fatalf("DefaultPolicy = %+v, want %+v", got, want)
	}
	b := New(Policy{MaxRetries: -1})
	want.MaxRetries = 0
	if b.policy != want {
		t.Fatalf("normalized policy = %+v, want %+v", b.policy, want)
	}
}

func TestBudgetSharedCountAndExhaustionMetadata(t *testing.T) {
	b, now, delays := testBudget(Policy{MaxRetries: 2})
	failure := errors.New("temporary failure")
	ctx := WithBudget(context.Background(), b)
	var events []RetryEvent
	ctx = WithObserver(ctx, func(event RetryEvent) { events = append(events, event) })
	child := context.WithValue(ctx, "child", true)
	if FromContext(child) != b || FromContext(context.Background()) != nil {
		t.Fatal("context lost the shared budget")
	}
	for _, scope := range []context.Context{ctx, child} {
		if err := FromContext(scope).Wait(scope, failure, -1); err != nil {
			t.Fatalf("Wait: %v", err)
		}
	}
	err := b.Wait(ctx, failure, -1)
	var exhausted *ExhaustedError
	if !errors.As(err, &exhausted) || !errors.Is(err, failure) {
		t.Fatalf("exhaustion did not preserve cause: %v", err)
	}
	if exhausted.Attempts != 3 || exhausted.RetryCount != 2 || exhausted.Retryable() || exhausted.NextRetryAt != now.Add(2*time.Second) {
		t.Fatalf("exhaustion metadata = %+v", exhausted)
	}
	wrapped := fmt.Errorf("inner request: %w", err)
	if got := b.Wait(ctx, wrapped, -1); got != wrapped {
		t.Fatalf("nested exhaustion = %v, want unchanged error", got)
	}
	if len(*delays) != 2 || (*delays)[0] != 500*time.Millisecond || (*delays)[1] != time.Second {
		t.Fatalf("waits = %v", *delays)
	}
	if len(events) != 4 {
		t.Fatalf("events = %+v", events)
	}
	for i, event := range events {
		if event.Err != failure || event.RetryCount != i/2+1 || event.Waiting != (i%2 == 0) {
			t.Fatalf("event %d = %+v", i, event)
		}
		if i%2 == 1 && (event.Delay != events[i-1].Delay || event.NextRetryAt != events[i-1].NextRetryAt) {
			t.Fatalf("wait-end event lost schedule metadata: %+v", event)
		}
	}
}

func TestBudgetElapsedStartsAtFirstFailure(t *testing.T) {
	b, now, delays := testBudget(Policy{MaxRetries: 20, MaxElapsed: 2 * time.Second})
	*now = now.Add(time.Hour) // A long healthy generation before its first failure.
	ctx := WithBudget(context.Background(), b)
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("budget imposed a deadline on healthy work")
	}
	if err := b.Wait(ctx, errors.New("first"), -1); err != nil {
		t.Fatalf("first failure consumed healthy time: %v", err)
	}
	*now = now.Add(2 * time.Second)
	var exhausted *ExhaustedError
	if err := b.Wait(ctx, errors.New("later"), -1); !errors.As(err, &exhausted) {
		t.Fatalf("elapsed budget = %v, want exhaustion", err)
	}
	if len(*delays) != 1 || ctx.Err() != nil {
		t.Fatalf("elapsed budget waited or cancelled caller: delays=%v ctx=%v", *delays, ctx.Err())
	}
}

func TestBudgetHintsAndElapsedBound(t *testing.T) {
	for _, tc := range []struct {
		name      string
		hint      time.Duration
		wantDelay time.Duration
		exhausted bool
	}{
		{"absent", -1, time.Second, false},
		{"zero is valid", 0, time.Second, false},
		{"minimum", 500 * time.Millisecond, time.Second, false},
		{"larger than backoff cap", 4 * time.Second, 4 * time.Second, false},
		{"does not fit", 6 * time.Second, 0, true},
		{"no time left for attempt", 5 * time.Second, 0, true},
		{"huge", time.Duration(1<<63 - 1), 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, now, delays := testBudget(Policy{MaxRetries: 2, MaxElapsed: 5 * time.Second, InitialDelay: time.Second, MaxDelay: time.Second})
			start := *now
			err := b.Wait(context.Background(), errors.New("busy"), tc.hint)
			var exhausted *ExhaustedError
			if errors.As(err, &exhausted) != tc.exhausted {
				t.Fatalf("Wait = %v, want exhausted %v", err, tc.exhausted)
			}
			if tc.exhausted {
				if len(*delays) != 0 || exhausted.RetryCount != 0 || exhausted.NextRetryAt.Before(start.Add(tc.hint)) {
					t.Fatalf("hint shortened or waited: error=%+v delays=%v", exhausted, *delays)
				}
			} else if len(*delays) != 1 || (*delays)[0] != tc.wantDelay {
				t.Fatalf("waits = %v, want %v", *delays, tc.wantDelay)
			}
		})
	}
}

func TestBudgetObserverCancellationAndSlowObserver(t *testing.T) {
	for _, cancelWait := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelWait), func(t *testing.T) {
			b, now, _ := testBudget(Policy{MaxRetries: 1, MaxElapsed: time.Second, InitialDelay: time.Millisecond})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var events []RetryEvent
			ctx = WithObserver(ctx, func(event RetryEvent) {
				events = append(events, event)
				if event.Waiting {
					if cancelWait {
						cancel()
					} else {
						*now = now.Add(time.Second)
					}
				}
			})
			err := b.Wait(ctx, errors.New("busy"), -1)
			var exhausted *ExhaustedError
			if cancelWait && !errors.Is(err, context.Canceled) || !cancelWait && !errors.As(err, &exhausted) {
				t.Fatalf("Wait = %v", err)
			}
			if len(events) != 2 || !events[0].Waiting || events[1].Waiting {
				t.Fatalf("observer did not end wait: %+v", events)
			}
		})
	}
}

func TestBudgetCancellationDuringWait(t *testing.T) {
	b := New(Policy{MaxRetries: 1, InitialDelay: time.Hour, MaxDelay: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiting := make(chan struct{})
	ctx = WithObserver(ctx, func(event RetryEvent) {
		if event.Waiting {
			close(waiting)
		}
	})
	// Use a hint that fits the elapsed bound but cannot finish during the test.
	b.policy.MaxElapsed = 2 * time.Hour
	done := make(chan error, 1)
	go func() { done <- b.Wait(ctx, errors.New("busy"), time.Hour) }()
	<-waiting
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Wait = %v, want cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not interrupt wait")
	}
	if err := b.Wait(ctx, errors.New("exhausted"), -1); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost to exhaustion: %v", err)
	}
}

func TestBudgetConcurrentCallersShareBound(t *testing.T) {
	b := New(Policy{MaxRetries: 3, InitialDelay: time.Nanosecond, MaxDelay: time.Nanosecond})
	var wg sync.WaitGroup
	results := make(chan error, 20)
	for range 20 {
		wg.Go(func() { results <- b.Wait(context.Background(), errors.New("busy"), -1) })
	}
	wg.Wait()
	close(results)
	accepted := 0
	for err := range results {
		if err == nil {
			accepted++
		} else {
			var exhausted *ExhaustedError
			if !errors.As(err, &exhausted) {
				t.Fatalf("unexpected error: %v", err)
			}
		}
	}
	if accepted != 3 {
		t.Fatalf("granted %d retries, want shared limit 3", accepted)
	}
}

func TestJitterAndBackoffAreBoundedWithoutOverflow(t *testing.T) {
	for _, delay := range []time.Duration{1, time.Second, time.Duration(1<<63 - 1)} {
		for range 100 {
			if got := jitter(delay); got < delay/2 || got > delay {
				t.Fatalf("jitter(%v) = %v", delay, got)
			}
		}
	}
	b, _, delays := testBudget(Policy{MaxRetries: 5, InitialDelay: time.Second, MaxDelay: 3 * time.Second})
	for range 5 {
		if err := b.Wait(context.Background(), errors.New("busy"), -1); err != nil {
			t.Fatalf("Wait = %v", err)
		}
	}
	want := []time.Duration{time.Second, 2 * time.Second, 3 * time.Second, 3 * time.Second, 3 * time.Second}
	for i, delay := range *delays {
		if delay != want[i] {
			t.Fatalf("backoff = %v, want %v", *delays, want)
		}
	}
	b, _, _ = testBudget(Policy{MaxRetries: 10, MaxElapsed: time.Duration(1<<63 - 1), InitialDelay: time.Duration(1<<62 + 1), MaxDelay: time.Duration(1<<63 - 1)})
	b.retries = 9
	var exhausted *ExhaustedError
	if err := b.Wait(context.Background(), errors.New("busy"), -1); !errors.As(err, &exhausted) {
		t.Fatalf("overflowed backoff did not exhaust: %v", err)
	}
}
