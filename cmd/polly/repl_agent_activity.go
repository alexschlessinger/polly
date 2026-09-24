package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/swarm"
)

func currentAgentActivity(s *swarm.State, m *swarm.Member, a swarm.LiveActivity) bool {
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

func agentExecutionBusy(s *swarm.State, m *swarm.Member) bool {
	e := s.Executions[m.Execution]
	return e != nil && (e.Status == "running" || e.Status == "queued" || e.Status == "waiting")
}

// agentStopLabel is the status of an agent the user stopped.
func agentStopLabel(s *swarm.State, m *swarm.Member) string {
	if m.Control != swarm.MemberControlStopped {
		return ""
	}
	if agentExecutionBusy(s, m) {
		return "Stopping · stopped by you"
	}
	return "Stopped by you"
}

// agentLiveWord is waiting, thinking or toolcall for a current activity.
func agentLiveWord(s *swarm.State, m *swarm.Member, a swarm.LiveActivity) string {
	if !currentAgentActivity(s, m, a) {
		return ""
	}
	if s.Executions[m.Execution].Status == "waiting" {
		return "waiting"
	}
	return map[string]string{"queued": "waiting", "preparing": "waiting", "requesting": "waiting", "waiting": "waiting", "receiving": "thinking", "thinking": "thinking", "generating": "thinking", "tools": "toolcall"}[a.Phase]
}

// agentRowStatus is the live status for list, transcript and inspector
// rows, which keep their width while the clock ticks: the word padded to
// eight cells and a one-unit clock right-aligned in three. warn stands in for
// agentLiveExtras, and is only ever set on that fixed form.
func agentRowStatus(s *swarm.State, m *swarm.Member, a swarm.LiveActivity, now time.Time) (status string, warn bool) {
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
	if !a.Since.IsZero() {
		d := max(0, now.Sub(a.Since))
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

// Details are computed from cached snapshots, never by opening the child or
// querying SQLite on the render path.
func agentActivityDetails(s *swarm.State, m *swarm.Member) []string {
	if m == nil {
		return nil
	}
	var lines []string
	if e := s.Executions[m.Execution]; e != nil {
		if m.Control != swarm.MemberControlStopped && (e.Status == "paused" || e.Status == "failed") && e.Error != "" {
			lines = append(lines, "Reason: "+style.SanitizeImageText(e.Error))
		}
		if !e.ResumedAt.IsZero() {
			actor := "parent"
			if e.ResumedBy == "user" {
				actor = "you"
			}
			lines = append(lines, "Resumed by "+actor+" at "+e.ResumedAt.Local().Format("3:04 PM"))
		}
	}
	if m.Control == swarm.MemberControlStopped {
		lines = append(lines, "Only you can resume this agent")
	}
	return lines
}
