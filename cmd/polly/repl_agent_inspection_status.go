package main

import (
	"time"

	"github.com/alexschlessinger/pollytool/swarm"
)

// agentInspectionStatus shares the inline phase and clock, with additional
// coordination and request details for inspection. Details are plain text for
// the inspector to wrap; reading this helper never opens the agent's session.
func agentInspectionStatus(s *swarm.State, m *swarm.Member, a swarm.LiveActivity, approval bool, now time.Time) (status string, warn bool, details []string) {
	if s == nil || m == nil {
		return "", false, nil
	}
	if approval {
		return "Approval needed", false, nil
	}
	p := swarmMemberActivity(s, m)
	if stop := agentStopLabel(s, m); stop != "" {
		status = stop
	} else if p.Busy {
		phase := ""
		if currentAgentActivity(s, m, a) {
			phase = a.Phase
		}
		// Durable queue/park state outranks a previous live phase. Only a
		// matching phase can supply its clock or request diagnostics.
		switch p.Outcome {
		case "queued":
			if phase != "queued" {
				return "Queued for execution", false, nil
			}
		case "waiting":
			if phase != "waiting" {
				return "Waiting for updates", false, nil
			}
		}
		status, warn = agentRowStatus(s, m, a, now)
		if status == "" {
			return "Running · phase unavailable", false, nil
		}
		if extras := agentLiveExtras(a, now); extras != "" {
			details = append(details, extras)
		}
		// The shared clock measures a phase/request, or a retry countdown,
		// never the lifetime of the assignment.
		return status, warn, details
	} else {
		switch {
		case p.Outcome == "failed":
			status = "Failed"
		case p.Lifecycle == swarm.LifecyclePaused:
			status = "Paused · " + p.Detail
		case p.TaskStatus == "done":
			status = "Finished"
		case p.TaskStatus == "blocked":
			status = "Blocked"
		case p.TaskStatus == "awaiting review":
			status = "Awaiting review"
		case p.TaskStatus == "integration pending":
			status = "Integration pending"
		case p.TaskStatus == "integration halted":
			status = "Integration halted"
		case p.TaskStatus == "delivering":
			status = "Delivering result"
		case p.TaskStatus == "canceled":
			status = "Task canceled"
		case p.TaskStatus != "":
			status = "Idle · task " + p.TaskStatus
		case p.Outcome == "completed":
			status = "Execution finished"
		default:
			status = "Idle"
		}
	}
	if p.Lifecycle == swarm.LifecyclePaused && p.TaskStatus != "" && p.TaskStatus != "done" && p.TaskStatus != "canceled" {
		details = append(details, "Task: "+p.TaskStatus)
	}
	if p.Deferred {
		status += " · deferred"
	}
	return status, warn, details
}
