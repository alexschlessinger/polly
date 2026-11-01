package swarm

import (
	"slices"
	"sync"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
)

// LiveToolActivity identifies an executing call, without retaining its output.
// Started is when the batch was observed, before parallel execution began.
type LiveToolActivity struct {
	Call    messages.ChatMessageToolCall
	Started time.Time
}

// LiveActivity is a read-only, in-memory snapshot of an invocation. It carries
// no generated prose, reasoning or tool output and is never written to
// coordination state per chunk. Match Execution and Generation before combining
// it with persisted state.
type LiveActivity struct {
	Execution              string
	Generation             int
	Phase                  string
	Since                  time.Time
	RequestStarted         time.Time
	LastData               time.Time
	RequestActive          bool
	Attempt                int // one-based, including stream attempts, not HTTP retries
	RetryCount             int // one-based next retry while waiting
	NextRetryAt            time.Time
	RetryReason            string // error text; display callers must sanitize
	StallTimeout, Deadline time.Duration
	StreamedBytes          int                // text and reasoning; bytes / 4 is only a rough token estimate
	OutputTokens           int                // provider-reported count for the current request, when available
	Tools                  []LiveToolActivity // active calls only, in batch order
}

type liveActivity struct {
	mu                 sync.Mutex
	snapshot           LiveActivity
	epoch              int
	iteration, attempt int
}

// bindAbortObserver runs before text wrappers so observing output cannot opt a
// real external display into replay without its own abort hook.
func bindAbortObserver(cb *llm.AgentCallbacks, observer func(int, int, error)) {
	prior := cb.OnAttemptAborted
	if prior == nil && (cb.OnContent != nil || cb.OnReasoning != nil || cb.OnCommentary != nil) {
		return
	}
	cb.OnAttemptAborted = func(iteration, attempt int, err error) {
		observer(iteration, attempt, err)
		if prior != nil {
			prior(iteration, attempt, err)
		}
	}
}

// LiveActivities returns independent copies; callers never acquire child
// session leases or change execution ownership to inspect activity.
func (r *Runtime) LiveActivities() map[string]LiveActivity {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]LiveActivity, len(r.active))
	for member, i := range r.active {
		i.activity.mu.Lock()
		a := i.activity.snapshot
		// Calls contain only strings; cloning the slice also isolates each
		// call value from both the runtime and other inspection snapshots.
		a.Tools = slices.Clone(a.Tools)
		i.activity.mu.Unlock()
		a.Execution, a.Generation = i.id, i.generation
		if a.Phase == "" {
			a.Phase = "queued"
		}
		out[member] = a
	}
	return out
}

func (i *invocation) bindActivity(cb *llm.AgentCallbacks, stall, deadline time.Duration) {
	live := &i.activity
	live.mu.Lock()
	live.epoch++
	epoch := live.epoch
	live.snapshot = LiveActivity{Phase: "preparing", Since: time.Now()}
	live.mu.Unlock()
	update := func(fn func(*LiveActivity)) {
		live.mu.Lock()
		defer live.mu.Unlock()
		if live.epoch == epoch {
			fn(&live.snapshot)
		}
	}
	bindAbortObserver(cb, func(_, _ int, _ error) {
		update(func(a *LiveActivity) {
			a.RequestActive = false
			a.StreamedBytes, a.OutputTokens = 0, 0
			a.Tools = nil
			a.Phase, a.Since = "preparing", time.Now()
		})
	})
	var retryRequestActive bool // guarded by live.mu through update
	retry := cb.OnRetry
	cb.OnRetry = func(event llm.RetryEvent) {
		update(func(a *LiveActivity) {
			a.Since = time.Now()
			if event.Waiting {
				retryRequestActive = a.RequestActive
				a.RequestActive = false
				a.Phase, a.RetryCount, a.NextRetryAt = "retrying", event.RetryCount, event.NextRetryAt
				a.RetryReason = ""
				if event.Err != nil {
					a.RetryReason = event.Err.Error()
				}
			} else {
				a.RetryCount, a.NextRetryAt, a.RetryReason = 0, time.Time{}, ""
				a.RequestActive = retryRequestActive
				a.Phase = "preparing"
				if a.RequestActive {
					a.Phase = "requesting"
				}
			}
		})
		if retry != nil {
			retry(event)
		}
	}
	request := cb.OnModelRequest
	cb.OnModelRequest = func(iteration, attempt int) {
		update(func(a *LiveActivity) {
			now := time.Now()
			*a = LiveActivity{Phase: "requesting", Since: now, RequestStarted: now, RequestActive: true, Attempt: attempt + 1, StallTimeout: stall, Deadline: deadline}
			live.iteration, live.attempt = iteration, attempt
		})
		if request != nil {
			request(iteration, attempt)
		}
	}
	activity := cb.OnStreamActivity
	cb.OnStreamActivity = func(iteration, attempt int) {
		update(func(a *LiveActivity) {
			if a.RequestActive && live.iteration == iteration && live.attempt == attempt {
				a.LastData = time.Now()
				if a.Phase == "requesting" {
					a.Phase = "receiving"
				}
			}
		})
		if activity != nil {
			activity(iteration, attempt)
		}
	}
	text := func(phase, content string) {
		if content == "" {
			return
		}
		update(func(a *LiveActivity) {
			a.Phase, a.LastData = phase, time.Now()
			a.StreamedBytes += len(content)
		})
	}
	reasoning := cb.OnReasoning
	cb.OnReasoning = func(content string) {
		text("thinking", content)
		if reasoning != nil {
			reasoning(content)
		}
	}
	content := cb.OnContent
	cb.OnContent = func(chunk string) {
		text("generating", chunk)
		if content != nil {
			content(chunk)
		}
	}
	usage := cb.OnUsageProgress
	cb.OnUsageProgress = func(u llm.UsageUpdate) {
		update(func(a *LiveActivity) { a.OutputTokens = u.OutputTokens })
		if usage != nil {
			usage(u)
		}
	}
	finished := cb.OnIterationUsage
	cb.OnIterationUsage = func(iteration, in, out int) {
		update(func(a *LiveActivity) { a.RequestActive = false; a.OutputTokens = out })
		if finished != nil {
			finished(iteration, in, out)
		}
	}
	toolStart := cb.OnToolStart
	cb.OnToolStart = func(calls []messages.ChatMessageToolCall) {
		update(func(a *LiveActivity) {
			a.Phase, a.Since = "tools", time.Now()
			a.Tools = make([]LiveToolActivity, len(calls))
			for n, call := range calls {
				a.Tools[n] = LiveToolActivity{Call: call, Started: a.Since}
			}
			if len(calls) == 1 && calls[0].Name == "wait_agent" {
				a.Phase = "waiting"
			}
		})
		if toolStart != nil {
			toolStart(calls)
		}
	}
	toolEnd := cb.OnToolEnd
	cb.OnToolEnd = func(call messages.ChatMessageToolCall, result string, duration time.Duration, err error) {
		update(func(a *LiveActivity) {
			for n, tool := range a.Tools {
				if call.DisplayID != "" {
					if tool.Call.DisplayID != call.DisplayID {
						continue
					}
				} else if tool.Call.ID != call.ID || (call.ID == "" && tool.Call != call) {
					continue
				}
				a.Tools = slices.Delete(a.Tools, n, n+1)
				if len(a.Tools) == 0 {
					a.Tools = nil
				}
				break
			}
		})
		if toolEnd != nil {
			toolEnd(call, result, duration, err)
		}
	}
}
