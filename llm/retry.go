package llm

import "github.com/alexschlessinger/pollytool/llm/internal/retry"

// RetryPolicy bounds recovery of one model call, shared by its HTTP and stream
// attempts. A nil CompletionRequest.RetryPolicy selects DefaultRetryPolicy.
// MaxRetries is the number of additional attempts; zero disables retries.
// MaxElapsed runs from the first transient failure, not the start of a healthy
// generation, and never cancels an attempt that is already running.
type RetryPolicy = retry.Policy

// DefaultRetryPolicy allows twelve retries within five minutes of the first
// transient failure, with jittered exponential backoff capped at thirty seconds.
func DefaultRetryPolicy() RetryPolicy { return retry.DefaultPolicy() }

// RetryEvent announces a scheduled retry, including HTTP retries inside a
// provider call. RetryCount is one-based; NextRetryAt is the earliest next send.
type RetryEvent = retry.RetryEvent

// RetryExhaustedError reports a transient failure that exhausted recovery.
// The original error is available through errors.Unwrap. Coordinators can
// persist its attempt count and NextRetryAt for explicit, recoverable pauses.
type RetryExhaustedError = retry.ExhaustedError

func requestRetryPolicy(req *CompletionRequest) RetryPolicy {
	if req.RetryPolicy != nil {
		return *req.RetryPolicy
	}
	return DefaultRetryPolicy()
}
