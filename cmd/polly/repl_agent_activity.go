package main

import (
	"fmt"
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

func agentLiveSummary(s *swarm.State, m *swarm.Member, a swarm.LiveActivity, now time.Time) string {
	if m == nil {
		return ""
	}
	if m.Control == swarm.MemberControlStopped {
		if agentExecutionBusy(s, m) {
			return "Stopping · stopped by you"
		}
		return "Stopped by you"
	}
	if !currentAgentActivity(s, m, a) {
		return ""
	}
	phase := map[string]string{"queued": "Queued", "preparing": "Preparing", "requesting": "Requesting model", "receiving": "Receiving response", "thinking": "Thinking", "generating": "Generating response", "tools": "Running tools", "waiting": "Waiting for agent"}[a.Phase]
	if s.Executions[m.Execution].Status == "waiting" {
		phase = "Waiting for agent"
	}
	if phase == "" {
		return ""
	}
	if !a.Since.IsZero() {
		phase += " · " + activityElapsed(now.Sub(a.Since))
	}
	if a.RequestActive && !a.LastData.IsZero() {
		phase += " · data " + activityElapsed(now.Sub(a.LastData)) + " ago"
	}
	return phase
}

// Details are computed from cached snapshots, never by opening the child or
// querying SQLite on the render path. Timeouts describe this provider request,
// not the lifetime of the whole assignment or a tool's separate budget.
func agentActivityDetails(s *swarm.State, m *swarm.Member, a swarm.LiveActivity, now time.Time) []string {
	if m == nil {
		return nil
	}
	var lines []string
	if currentAgentActivity(s, m, a) && a.RequestActive {
		request := "Model request · " + activityElapsed(now.Sub(a.RequestStarted))
		if a.Attempt > 1 {
			request += fmt.Sprintf(" · attempt %d", a.Attempt)
		}
		if a.LastData.IsZero() {
			request += " · no data yet"
		}
		if a.OutputTokens > 0 {
			request += fmt.Sprintf(" · %s output tokens", humanizeTokens(a.OutputTokens))
		} else if a.StreamedBytes > 0 {
			request += fmt.Sprintf(" · ≈%s output tokens", humanizeTokens(max(1, a.StreamedBytes/4)))
		}
		lines = append(lines, request)
		limits := "Silence timeout · "
		if a.StallTimeout > 0 {
			last := a.LastData
			if last.IsZero() {
				last = a.RequestStarted
			}
			limits += activityElapsed(a.StallTimeout) + " (" + activityElapsed(a.StallTimeout-now.Sub(last)) + " left)"
		} else {
			limits += "off"
		}
		lines = append(lines, limits)
		limits = "Request deadline · "
		if a.Deadline > 0 {
			limits += activityElapsed(a.Deadline) + " (" + activityElapsed(a.Deadline-now.Sub(a.RequestStarted)) + " left)"
		} else {
			limits += "off"
		}
		lines = append(lines, limits)
	}
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
