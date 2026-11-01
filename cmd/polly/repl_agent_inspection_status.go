package main

import (
	"time"

	"github.com/alexschlessinger/pollytool/swarm"
)

// agentInspectionStatus describes cached coordination and activity snapshots in
// full, independently of the compact list status. Details are plain text for
// the inspector to wrap; reading this helper never opens the agent's session.
func agentInspectionStatus(s *swarm.State, m *swarm.Member, a swarm.LiveActivity, approval bool, now time.Time) (status string, details []string) {
	if s == nil || m == nil {
		return "", nil
	}
	if approval {
		return "Approval needed", nil
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
			status = "Queued for execution"
			if phase != "queued" {
				return status, nil
			}
		case "waiting":
			status = "Waiting for updates"
			if phase != "waiting" {
				return status, nil
			}
		}
		clock, since := "Phase elapsed: ", a.Since
		switch phase {
		case "queued":
			status = "Queued for execution"
		case "preparing":
			status = "Preparing workspace"
		case "requesting":
			status = "Waiting for model response"
		case "retrying":
			status = "Retrying model request"
		case "receiving":
			status = "Receiving model response"
		case "thinking":
			status = "Model reasoning"
		case "generating":
			status = "Generating response"
		case "tools":
			status = "Running tools"
		case "waiting":
			status = "Waiting for updates"
		default:
			return "Running · phase unavailable", nil
		}
		switch phase {
		case "requesting", "receiving", "thinking", "generating":
			// Streaming changes the phase but keeps Since at the start of
			// the request. Do not call it time spent in the current phase.
			clock = "Request elapsed: "
			if !a.RequestStarted.IsZero() {
				since = a.RequestStarted
			}
		}
		if !since.IsZero() {
			details = append(details, clock+activityElapsed(now.Sub(since)))
		}
		if extras := agentLiveExtras(a, now); extras != "" {
			details = append(details, extras)
		}
		// Executions have no start/finish timestamps. In particular, neither
		// Since nor ResumedAt is a total execution duration.
		return status, details
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
	return status, details
}
