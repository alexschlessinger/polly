package swarm

import (
	"sync"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
)

// LiveActivity is a read-only, in-memory snapshot of an invocation. It carries
// no generated text and is never written to coordination state per chunk.
// Match Execution and Generation before combining it with persisted state.
type LiveActivity struct {
	Execution              string
	Generation             int
	Phase                  string
	Since                  time.Time
	RequestStarted         time.Time
	LastData               time.Time
	RequestActive          bool
	Attempt                int // one-based, including retries
	StallTimeout, Deadline time.Duration
	StreamedBytes          int // text and reasoning; bytes / 4 is only a rough token estimate
	OutputTokens           int // provider-reported count for the current request, when available
}

type liveActivity struct {
	mu                 sync.Mutex
	snapshot           LiveActivity
	epoch              int
	iteration, attempt int
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
			if len(calls) == 1 && calls[0].Name == "wait_agent" {
				a.Phase = "waiting"
			}
		})
		if toolStart != nil {
			toolStart(calls)
		}
	}
}
