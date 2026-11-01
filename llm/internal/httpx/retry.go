package httpx

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// retryAfter returns the longest valid server hint, or -1 if absent. A huge
// positive hint saturates rather than overflowing into a short or negative wait.
func retryAfter(resp *http.Response, now time.Time) time.Duration {
	if resp == nil {
		return -1
	}
	delay := durationHint(resp.Header.Get("Retry-After-Ms"), time.Millisecond)
	raw := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if seconds := durationHint(raw, time.Second); seconds >= 0 {
		delay = max(delay, seconds)
	} else if at, err := http.ParseTime(raw); err == nil {
		delay = max(delay, max(at.Sub(now), 0))
	}
	return delay
}

func durationHint(raw string, unit time.Duration) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return -1
	}
	const maxDuration = time.Duration(1<<63 - 1)
	limit := maxDuration / unit
	var n time.Duration
	overflow := false
	for _, r := range raw {
		if r < '0' || r > '9' {
			return -1
		}
		digit := time.Duration(r - '0')
		if n > limit/10 || n == limit/10 && digit > limit%10 {
			overflow = true
		}
		if !overflow {
			n = n*10 + digit
		}
	}
	if overflow {
		return maxDuration
	}
	return n * unit
}

// sleepBeforeRetry retains the legacy policy when no shared budget is present.
func sleepBeforeRetry(ctx context.Context, resp *http.Response, attempt int) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	delay := 500 * time.Millisecond * time.Duration(1<<min(max(attempt, 0), 4))
	if hint := retryAfter(resp, time.Now()); hint >= 0 {
		delay = hint
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
