package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/swarm"
)

func currentAgentActivity(view swarm.DisplayView, m *swarm.Member, a swarm.LiveActivity) bool {
	if view == nil {
		return false
	}
	s := view.DisplaySnapshot("")
	if s == nil || m == nil || m.Control == swarm.MemberControlStopped {
		return false
	}
	e := s.Executions[m.Execution]
	return e != nil && e.ID == a.Execution && e.Generation == a.Generation && (e.Status == "running" || e.Status == "queued" || e.Status == "waiting")
}

func activityElapsed(d time.Duration) string {
	d = max(0, d).Truncate(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d/time.Second))
	}
	if d >= time.Hour {
		return fmt.Sprintf("%dh%02dm%02ds", int(d/time.Hour), int(d/time.Minute)%60, int(d/time.Second)%60)
	}
	return formatElapsed(d)
}

func agentExecutionBusy(view swarm.DisplayView, m *swarm.Member) bool {
	if view == nil || m == nil {
		return false
	}
	s := view.DisplaySnapshot("")
	if s == nil {
		return false
	}
	return executionBusy(s.Executions[m.Execution])
}

func executionBusy(e *swarm.Execution) bool {
	return e != nil && (e.Status == "running" || e.Status == "queued" || e.Status == "waiting")
}

// agentStopLabel is the status of an agent the user stopped.
func agentStopLabel(s swarm.DisplayView, m *swarm.Member) string {
	if m.Control != swarm.MemberControlStopped {
		return ""
	}
	if agentExecutionBusy(s, m) {
		return "Stopping · stopped by you"
	}
	return "Stopped by you"
}

// agentLiveWord is the phase word of a current activity: querying while a
// model reply is awaited, thinking, toolcall, retrying, or parked for an
// execution that yielded until addressed input. Waiting names the agents
// list's section for agents that wait on a person, never a live phase.
func agentLiveWord(s swarm.DisplayView, m *swarm.Member, a swarm.LiveActivity) string {
	if !currentAgentActivity(s, m, a) {
		return ""
	}
	if s.DisplaySnapshot("").Executions[m.Execution].Status == "waiting" {
		return "parked"
	}
	return map[string]string{"queued": "querying", "preparing": "querying", "requesting": "querying", "retrying": "retrying", "waiting": "parked", "receiving": "thinking", "thinking": "thinking", "generating": "thinking", "tools": "toolcall"}[a.Phase]
}

// executionRunTime is how long an execution has run: from its first start
// to its end, ticking while it is busy. Records that predate the timestamps
// have none.
func executionRunTime(e *swarm.Execution, now time.Time) (time.Duration, bool) {
	if e == nil || e.Started.IsZero() {
		return 0, false
	}
	end := e.Ended
	if end.IsZero() {
		if !executionBusy(e) {
			return 0, false
		}
		end = now
	}
	return max(0, end.Sub(e.Started)), true
}

// agentRowStatus is the live status for list, transcript and inspector
// rows, which keep their width while the clock ticks: the word padded to
// eight cells and a one-unit clock right-aligned in three. warn stands in for
// agentLiveExtras, and is only ever set on that fixed form.
func agentRowStatus(s swarm.DisplayView, m *swarm.Member, a swarm.LiveActivity, now time.Time) (status string, warn bool) {
	if m == nil {
		return "", false
	}
	if stop := agentStopLabel(s, m); stop != "" {
		return stop, false
	}
	word := agentLiveWord(s, m, a)
	if word == "" {
		return "", false
	}
	clock := ""
	// A retry counts down to its next attempt, rounded up; a phase counts
	// up. Both read in seconds under a minute.
	var d time.Duration
	if a.RetryCount > 0 {
		d = max(0, a.NextRetryAt.Sub(now)) + time.Second - 1
	} else if !a.Since.IsZero() {
		d = max(0, now.Sub(a.Since))
	}
	if a.RetryCount > 0 || !a.Since.IsZero() {
		clock = formatCompactDuration(d)
		if d < time.Minute {
			clock = fmt.Sprintf("%ds", int(d/time.Second))
		}
	}
	return fmt.Sprintf("%-8s  %3s", word, clock), agentLiveExtras(a, now) != ""
}

// agentLiveExtras spells out what a warned clock stands for: a retry and the
// time left before the nearer provider limit.
func agentLiveExtras(a swarm.LiveActivity, now time.Time) string {
	var parts []string
	if a.RetryCount > 0 {
		retry := modelRetryStatus{waiting: true, count: a.RetryCount, next: a.NextRetryAt}
		if a.RetryReason != "" {
			retry.reason = style.Truncate(strings.Join(strings.Fields(cleanActivityText(a.RetryReason)), " "), 160)
		}
		parts = append(parts, retry.label(now))
	}
	if a.RequestActive && a.Attempt > 1 {
		parts = append(parts, fmt.Sprintf("attempt %d", a.Attempt))
	}
	if left, near := requestLimitLeft(a, now); near {
		// A countdown rounds up, so it reads 0s only once the limit passed.
		parts = append(parts, "timeout in "+activityElapsed(left+time.Second-1))
	}
	return strings.Join(parts, " · ")
}

// warnedClock splits a warned row status into its text and the three-cell
// clock that takes the attention color; an unwarned status has no clock part.
func warnedClock(status string, warn bool) (text, clock string) {
	if !warn || len(status) < 3 {
		return status, ""
	}
	return status[:len(status)-3], status[len(status)-3:]
}

func rowStatusMarkup(status, color string, warn bool) string {
	text, clock := warnedClock(status, warn)
	return style.Styled(text, color, "") + style.Styled(clock, "active", "")
}

// requestLimitLeft is the time until the sooner of the silence timeout and
// the request deadline ends this provider request, near once that limit has
// used three quarters of its length. The limits describe this request, not
// the lifetime of the whole assignment or a tool's separate budget.
func requestLimitLeft(a swarm.LiveActivity, now time.Time) (left time.Duration, near bool) {
	if !a.RequestActive {
		return 0, false
	}
	var limit time.Duration
	consider := func(length time.Duration, from time.Time) {
		if length > 0 && (limit == 0 || length-now.Sub(from) < left) {
			limit, left = length, length-now.Sub(from)
		}
	}
	last := a.LastData
	if last.IsZero() {
		last = a.RequestStarted
	}
	consider(a.StallTimeout, last)
	consider(a.Deadline, a.RequestStarted)
	return left, limit > 0 && left < limit/4
}

// agentListStatus is the one status every listing of an agent shows: its
// presentation's words, or the live phase and clock while it works. An
// approval outranks the live phase. warn colors the clock (see agentRowStatus).
// It reads cached snapshots only, never the child's session.
func agentListStatus(s swarm.DisplayView, m *swarm.Member, p swarm.AgentPresentation, a swarm.LiveActivity, approval bool, now time.Time) (status string, warn bool) {
	status = listingLabel(p, approval)
	if live, liveWarn := agentRowStatus(s, m, a, now); live != "" && !approval {
		status, warn = live, liveWarn
	}
	return status, warn
}

// agentFailureReason is the one line a paused or failed execution adds under
// its status: the error the cached snapshot recorded. A stop by the user is
// its own status, not a failure.
func agentFailureReason(s swarm.DisplayView, m *swarm.Member) string {
	if s == nil || m == nil || m.Control == swarm.MemberControlStopped {
		return ""
	}
	e := s.DisplaySnapshot("").Executions[m.Execution]
	if e == nil || e.Status != "paused" && e.Status != "failed" || e.Error == "" {
		return ""
	}
	return "reason: " + style.SanitizeImageText(e.Error)
}
