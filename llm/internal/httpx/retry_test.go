package httpx

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm/internal/retry"
)

type retryTransport func(*http.Request) (*http.Response, error)

func (fn retryTransport) RoundTrip(req *http.Request) (*http.Response, error) { return fn(req) }

func retryResponse(status int, hint string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Retry-After-Ms": []string{hint}}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"busy"}}`))}
}

func fastBudget(retries int) *retry.Budget {
	return retry.New(retry.Policy{MaxRetries: retries, InitialDelay: time.Nanosecond, MaxDelay: time.Nanosecond})
}

func retryRequest(ctx context.Context) func() (*http.Request, error) {
	return func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodPost, "https://example.invalid", nil)
	}
}

func TestRetrierUsesOnlySharedBudget(t *testing.T) {
	for _, tc := range []struct {
		name   string
		legacy int
		shared int
	}{
		{"shared permits more than legacy", 0, 3},
		{"shared forbids legacy retries", 20, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			r := Retrier{MaxRetries: tc.legacy, ErrorFromResponse: testErrorFromResponse, Client: &http.Client{Transport: retryTransport(func(*http.Request) (*http.Response, error) {
				calls++
				return retryResponse(503, "0"), nil
			})}}
			ctx := retry.WithBudget(context.Background(), fastBudget(tc.shared))
			_, err := r.Do(ctx, retryRequest(ctx))
			var exhausted *retry.ExhaustedError
			var apiErr *testError
			if !errors.As(err, &exhausted) || !errors.As(err, &apiErr) || apiErr.Status != 503 {
				t.Fatalf("Do = %v, want typed exhaustion wrapping last API error", err)
			}
			if calls != tc.shared+1 || exhausted.RetryCount != tc.shared {
				t.Fatalf("calls=%d exhaustion=%+v, shared retries=%d", calls, exhausted, tc.shared)
			}
		})
	}
}

func TestRetrierObservesHTTPWaitAndCancellationCause(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(context.Canceled)
	cause := errors.New("caller stopped HTTP retry")
	ctx = retry.WithBudget(ctx, fastBudget(1))
	var events []retry.RetryEvent
	ctx = retry.WithObserver(ctx, func(event retry.RetryEvent) {
		events = append(events, event)
		if event.Waiting {
			cancel(cause)
		}
	})
	r := Retrier{ErrorFromResponse: testErrorFromResponse, Client: &http.Client{Transport: retryTransport(func(*http.Request) (*http.Response, error) {
		return retryResponse(503, "0"), nil
	})}}
	_, err := r.Do(ctx, retryRequest(ctx))
	if !errors.Is(err, context.Canceled) || len(events) != 2 || events[0].Source != "http" || !events[0].Waiting || events[1].Waiting || events[1].WaitError != cause || !events[1].Canceled {
		t.Fatalf("HTTP wait err=%v events=%+v", err, events)
	}
}

func TestRetrierCancellationReturnsContextError(t *testing.T) {
	for _, shared := range []bool{false, true} {
		for _, transportFailure := range []bool{false, true} {
			t.Run(strings.Join([]string{map[bool]string{false: "legacy", true: "shared"}[shared], map[bool]string{false: "http", true: "transport"}[transportFailure]}, "/"), func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if shared {
					ctx = retry.WithBudget(ctx, retry.New(retry.Policy{MaxRetries: 2, MaxElapsed: 2 * time.Hour}))
				}
				requested := make(chan struct{})
				r := Retrier{MaxRetries: 2, ErrorFromResponse: testErrorFromResponse, Client: &http.Client{Transport: retryTransport(func(*http.Request) (*http.Response, error) {
					close(requested)
					if transportFailure {
						return nil, io.ErrUnexpectedEOF
					}
					return retryResponse(503, "3600000"), nil
				})}}
				done := make(chan error, 1)
				go func() { _, err := r.Do(ctx, retryRequest(ctx)); done <- err }()
				<-requested
				cancel()
				select {
				case err := <-done:
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("Do = %v, want context cancellation", err)
					}
				case <-time.After(time.Second):
					t.Fatal("cancel did not interrupt retry wait")
				}
			})
		}
	}
}

type classifiedError struct{ retryable bool }

func (e *classifiedError) Error() string   { return "classified" }
func (e *classifiedError) Retryable() bool { return e.retryable }

func TestRetrierTerminalAndTypedErrorsBypassBudget(t *testing.T) {
	for _, hook := range []bool{false, true} {
		calls := 0
		failure := &classifiedError{retryable: hook}
		r := Retrier{MaxRetries: 2, ErrorFromResponse: func(*http.Response) error { return failure }, Client: &http.Client{Transport: retryTransport(func(*http.Request) (*http.Response, error) {
			calls++
			return retryResponse(429, "3600000"), nil
		})}}
		if hook {
			r.Terminal = func(error) bool { return true }
		}
		budget := fastBudget(1)
		ctx := retry.WithBudget(context.Background(), budget)
		if _, err := r.Do(ctx, retryRequest(ctx)); err != failure || calls != 1 {
			t.Fatalf("terminal hook=%v: calls=%d err=%v", hook, calls, err)
		}
		if err := budget.Wait(ctx, io.ErrUnexpectedEOF, -1); err != nil {
			t.Fatalf("terminal failure spent shared budget: %v", err)
		}
	}
}

func TestRetrierHintThatCannotFitBudgetExhaustsImmediately(t *testing.T) {
	calls := 0
	r := Retrier{MaxRetries: 20, ErrorFromResponse: testErrorFromResponse, Client: &http.Client{Transport: retryTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return retryResponse(503, "999999999999999999999999999999999"), nil
	})}}
	ctx := retry.WithBudget(context.Background(), fastBudget(2))
	_, err := r.Do(ctx, retryRequest(ctx))
	var exhausted *retry.ExhaustedError
	if !errors.As(err, &exhausted) || calls != 1 || exhausted.RetryCount != 0 || time.Until(exhausted.NextRetryAt) < 100*365*24*time.Hour {
		t.Fatalf("oversized hint ignored or shortened: calls=%d err=%+v", calls, err)
	}
}

func TestRetryAfterParsing(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const maximum = time.Duration(1<<63 - 1)
	for _, tc := range []struct {
		name, ms, seconds string
		want              time.Duration
	}{
		{"absent", "", "", -1},
		{"zero", "0", "", 0},
		{"seconds", "", "2", 2 * time.Second},
		{"milliseconds", "25", "", 25 * time.Millisecond},
		{"both hints are minimums", "25", "2", 2 * time.Second},
		{"invalid ms falls back", "bad", "2", 2 * time.Second},
		{"negative invalid", "-1", "-2", -1},
		{"overflow seconds", "", "9223372036854775807", maximum},
		{"overflow milliseconds", "9223372036854775807", "", maximum},
		{"overflow parser", "99999999999999999999999999999", "", maximum},
		{"overflow but malformed", "99999999999999999999999999999x", "", -1},
		{"date", "", now.Add(2 * time.Minute).Format(http.TimeFormat), 2 * time.Minute},
		{"past date", "", now.Add(-time.Minute).Format(http.TimeFormat), 0},
		{"whitespace", "  25  ", "", 25 * time.Millisecond},
		{"max whole milliseconds", "9223372036854", "", 9223372036854 * time.Millisecond},
		{"next millisecond saturates", "9223372036855", "", maximum},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := retryResponse(429, tc.ms)
			defer resp.Body.Close()
			resp.Header.Set("Retry-After", tc.seconds)
			if got := retryAfter(resp, now); got != tc.want {
				t.Fatalf("retryAfter = %v, want %v", got, tc.want)
			}
		})
	}
}
