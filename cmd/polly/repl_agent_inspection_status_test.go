package main

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/swarm"
	"github.com/alexschlessinger/pollytool/workflow"
	"github.com/alexschlessinger/pollytool/worktree"
)

func agentInspectionStatusFixture() (*swarm.State, *swarm.Member, swarm.LiveActivity, time.Time) {
	now := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	m := &swarm.Member{ID: "member", Execution: "execution", Task: "task"}
	e := &swarm.Execution{ID: m.Execution, Member: m.ID, Status: "running", Generation: 2}
	task := &swarm.Task{ID: m.Task, Owner: m.ID, Execution: e.ID, Status: "running", Revision: 1}
	s := &swarm.State{
		Members:    map[string]*swarm.Member{m.ID: m},
		Executions: map[string]*swarm.Execution{e.ID: e},
		Tasks:      map[string]*swarm.Task{task.ID: task},
	}
	a := swarm.LiveActivity{Execution: e.ID, Generation: e.Generation, Phase: "requesting", Since: now.Add(-12 * time.Second)}
	return s, m, a, now
}

func TestAgentInspectionStatusPhases(t *testing.T) {
	for _, tc := range []struct {
		phase, status, clock, compact string
	}{
		{"queued", "Queued for execution", "Phase elapsed: 12s", "waiting   12s"},
		{"preparing", "Preparing workspace", "Phase elapsed: 12s", "waiting   12s"},
		{"requesting", "Waiting for model response", "Request elapsed: 12s", "waiting   12s"},
		{"retrying", "Retrying model request", "Phase elapsed: 12s", "retrying  12s"},
		{"receiving", "Receiving model response", "Request elapsed: 12s", "thinking  12s"},
		{"thinking", "Model reasoning", "Request elapsed: 12s", "thinking  12s"},
		{"generating", "Generating response", "Request elapsed: 12s", "thinking  12s"},
		{"tools", "Running tools", "Phase elapsed: 12s", "toolcall  12s"},
		{"waiting", "Waiting for updates", "Phase elapsed: 12s", "waiting   12s"},
	} {
		t.Run(tc.phase, func(t *testing.T) {
			s, m, a, now := agentInspectionStatusFixture()
			a.Phase = tc.phase
			status, details := agentInspectionStatus(s, m, a, false, now)
			if status != tc.status || !slices.Equal(details, []string{tc.clock}) {
				t.Fatalf("got %q, %q; want %q, %q", status, details, tc.status, tc.clock)
			}
			if compact, warn := agentRowStatus(s, m, a, now); compact != tc.compact || warn {
				t.Fatalf("compact status changed: %q, warn %v", compact, warn)
			}
		})
	}
}

func TestAgentInspectionStatusFencesActivity(t *testing.T) {
	for _, tc := range []struct {
		name, outcome, phase, status string
		stale, approval, stopped     bool
		clock                        bool
	}{
		{name: "unknown live phase", outcome: "running", phase: "future-phase", status: "Running · phase unavailable"},
		{name: "missing activity", outcome: "running", status: "Running · phase unavailable"},
		{name: "old generation", outcome: "running", phase: "thinking", stale: true, status: "Running · phase unavailable"},
		{name: "queued without activity", outcome: "queued", status: "Queued for execution"},
		{name: "queued activity", outcome: "queued", phase: "queued", status: "Queued for execution", clock: true},
		{name: "queue overrides stream", outcome: "queued", phase: "generating", status: "Queued for execution"},
		{name: "parked without activity", outcome: "waiting", status: "Waiting for updates"},
		{name: "parked activity", outcome: "waiting", phase: "waiting", status: "Waiting for updates", clock: true},
		{name: "park overrides stream", outcome: "waiting", phase: "generating", status: "Waiting for updates"},
		{name: "settled stream", outcome: "completed", phase: "thinking", status: "Finished"},
		{name: "approval overrides stream", outcome: "running", phase: "thinking", approval: true, status: "Approval needed"},
		{name: "approval overrides queue", outcome: "queued", phase: "queued", approval: true, status: "Approval needed"},
		{name: "stopping overrides stream", outcome: "running", phase: "thinking", stopped: true, status: "Stopping · stopped by you"},
		{name: "stopped overrides completion", outcome: "completed", phase: "thinking", stopped: true, status: "Stopped by you"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, m, a, now := agentInspectionStatusFixture()
			s.Executions[m.Execution].Status = tc.outcome
			s.Tasks[m.Task].Status = "done"
			a.Phase = tc.phase
			if tc.stale {
				a.Generation--
			}
			if tc.stopped {
				m.Control = swarm.MemberControlStopped
			}
			status, details := agentInspectionStatus(s, m, a, tc.approval, now)
			var want []string
			if tc.clock {
				want = []string{"Phase elapsed: 12s"}
			}
			if status != tc.status || !slices.Equal(details, want) {
				t.Fatalf("got %q, %q; want %q, %q", status, details, tc.status, want)
			}
		})
	}
	s, m, a, now := agentInspectionStatusFixture()
	a.Execution = "previous-execution"
	if status, details := agentInspectionStatus(s, m, a, false, now); status != "Running · phase unavailable" || len(details) != 0 {
		t.Fatalf("old execution activity leaked: %q, %q", status, details)
	}
	for _, missing := range []struct {
		s *swarm.State
		m *swarm.Member
	}{{nil, m}, {s, nil}} {
		if status, details := agentInspectionStatus(missing.s, missing.m, a, false, now); status != "" || len(details) != 0 {
			t.Fatalf("missing member/state: %q, %q", status, details)
		}
	}
	delete(s.Executions, m.Execution)
	delete(s.Tasks, m.Task)
	if status, details := agentInspectionStatus(s, m, a, false, now); status != "Idle" || len(details) != 0 {
		t.Fatalf("missing execution: %q, %q", status, details)
	}
}

func TestAgentInspectionStatusOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name, outcome, task, status string
		stop                        messages.StopReason
		retryPause                  bool
		details                     []string
	}{
		{name: "failed", outcome: "failed", task: "blocked", status: "Failed", details: []string{"Task: blocked"}},
		{name: "interrupted", outcome: "paused", task: "blocked", status: "Paused · interrupted", details: []string{"Task: blocked"}},
		{name: "iteration limit", outcome: "paused", task: "blocked", stop: messages.StopReasonMaxIterations, status: "Paused · iteration limit (3/5)", details: []string{"Task: blocked"}},
		{name: "retry budget exhausted", outcome: "paused", task: "blocked", retryPause: true, status: "Paused · retry budget exhausted", details: []string{"Task: blocked"}},
		{name: "finished task", outcome: "completed", task: "done", status: "Finished"},
		{name: "execution only", outcome: "completed", status: "Execution finished"},
		{name: "blocked task", outcome: "completed", task: "blocked", status: "Blocked"},
		{name: "review outstanding", outcome: "completed", task: "awaiting_review", status: "Awaiting review"},
		{name: "idle unfinished", outcome: "completed", task: "running", status: "Idle · task running"},
		{name: "canceled task", outcome: "completed", task: "canceled", status: "Task canceled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, m, a, now := agentInspectionStatusFixture()
			e := s.Executions[m.Execution]
			e.Status, e.StopReason, e.Iterations, e.Request.MaxIterations = tc.outcome, tc.stop, 3, 5
			if tc.retryPause {
				e.RetryPause = &swarm.RetryPause{}
			}
			s.Tasks[m.Task].Status = tc.task
			status, details := agentInspectionStatus(s, m, a, false, now)
			if status != tc.status || !slices.Equal(details, tc.details) {
				t.Fatalf("got %q, %q; want %q, %q", status, details, tc.status, tc.details)
			}
		})
	}
}

func TestAgentInspectionStatusTaskProjections(t *testing.T) {
	s, m, a, now := agentInspectionStatusFixture()
	e, task := s.Executions[m.Execution], s.Tasks[m.Task]
	e.Status, task.Status, task.AcceptedRevision, task.Snapshot = "completed", "awaiting_review", task.Revision, "submitted"
	check := func(want string) {
		t.Helper()
		if status, details := agentInspectionStatus(s, m, a, false, now); status != want || len(details) != 0 {
			t.Fatalf("got %q, %q; want %q", status, details, want)
		}
	}
	check("Integration pending")
	c := &swarm.IntegrationCandidate{
		ID: "candidate", Status: "conflicted",
		Inputs: []swarm.IntegrationInput{{TaskReference: swarm.TaskReference{Task: task.ID, Revision: task.Revision}, Submitted: worktree.Snapshot{ID: task.Snapshot}}},
	}
	s.Integrations = map[string]*swarm.IntegrationCandidate{c.ID: c}
	check("Integration halted")
	// An obsolete candidate cannot make finished work look blocked.
	task.Status = "done"
	check("Finished")
	task.Status, task.Requirement, task.AcceptedRevision = "running", swarm.RequirementDelivered, 0
	e.Result = &swarm.AgentResult{Task: task.ID, Execution: e.ID, Revision: task.Revision}
	check("Delivering result")
	task.Status, task.Requirement = "awaiting_review", swarm.RequirementReviewed
	m.ReadOnly = true
	e.Workflow = "workflow"
	s.Workflows = map[string]*workflow.Report{e.Workflow: {ID: e.Workflow, Status: "failed", Acknowledged: true}}
	task.Deferral = &swarm.TaskDeferral{Workflow: e.Workflow, Execution: e.ID, Owner: m.ID, Revision: task.Revision, Generation: e.Generation, Status: task.Status, Note: "later"}
	check("Awaiting review · deferred")
}

func TestAgentInspectionStatusTimingAndRetry(t *testing.T) {
	s, m, a, now := agentInspectionStatusFixture()
	a.Phase, a.RequestActive, a.RequestStarted = "generating", true, now.Add(-6*time.Minute-14*time.Second)
	s.Executions[m.Execution].ResumedAt = now.Add(-time.Hour)
	status, details := agentInspectionStatus(s, m, a, false, now)
	if status != "Generating response" || !slices.Equal(details, []string{"Request elapsed: 6m14s"}) {
		t.Fatalf("request clock mislabeled as phase/execution duration: %q, %q", status, details)
	}
	a.RequestStarted, a.Since = time.Time{}, time.Time{}
	if _, details := agentInspectionStatus(s, m, a, false, now); len(details) != 0 {
		t.Fatalf("invented missing clock: %q", details)
	}
	a.Since = now.Add(time.Minute)
	if _, details := agentInspectionStatus(s, m, a, false, now); !slices.Equal(details, []string{"Request elapsed: 0s"}) {
		t.Fatalf("negative clock: %q", details)
	}
	a.Phase, a.Since, a.RequestActive = "retrying", now.Add(-3*time.Second), false
	a.RetryCount, a.NextRetryAt, a.RetryReason = 2, now.Add(1500*time.Millisecond), "provider\n overloaded\ttry later"
	status, details = agentInspectionStatus(s, m, a, false, now)
	if status != "Retrying model request" || !slices.Equal(details, []string{"Phase elapsed: 3s", "retry 2 in 2s · provider overloaded try later"}) {
		t.Fatalf("retry status: %q, %q", status, details)
	}
	a.RetryCount, a.NextRetryAt, a.RetryReason = 0, time.Time{}, ""
	a.Phase, a.RequestStarted, a.RequestActive, a.Attempt, a.StallTimeout = "requesting", now.Add(-59*time.Second), true, 2, time.Minute
	_, details = agentInspectionStatus(s, m, a, false, now)
	if !slices.Equal(details, []string{"Request elapsed: 59s", "attempt 2 · timeout in 1s"}) {
		t.Fatalf("request diagnostics: %q", details)
	}
	if strings.Contains(strings.Join(details, " "), "execution") {
		t.Fatalf("request clock presented as total execution duration: %q", details)
	}
}
