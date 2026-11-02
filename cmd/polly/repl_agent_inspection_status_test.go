package main

import (
	"slices"
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
		phase, status string
	}{
		{"queued", "waiting   12s"},
		{"preparing", "waiting   12s"},
		{"requesting", "waiting   12s"},
		{"retrying", "retrying  12s"},
		{"receiving", "thinking  12s"},
		{"thinking", "thinking  12s"},
		{"generating", "thinking  12s"},
		{"tools", "toolcall  12s"},
		{"waiting", "waiting   12s"},
	} {
		t.Run(tc.phase, func(t *testing.T) {
			s, m, a, now := agentInspectionStatusFixture()
			a.Phase = tc.phase
			status, warn, details := agentInspectionStatus(s, m, a, false, now)
			if status != tc.status || warn || len(details) != 0 {
				t.Fatalf("got %q, warn %v, %q; want %q without diagnostics", status, warn, details, tc.status)
			}
			if inline, inlineWarn := agentRowStatus(s, m, a, now); inline != status || inlineWarn != warn {
				t.Fatalf("inspection %q, warn %v differs from inline %q, warn %v", status, warn, inline, inlineWarn)
			}
		})
	}
}

func TestAgentInspectionStatusFencesActivity(t *testing.T) {
	for _, tc := range []struct {
		name, outcome, phase, status string
		stale, approval, stopped     bool
	}{
		{name: "unknown live phase", outcome: "running", phase: "future-phase", status: "Running · phase unavailable"},
		{name: "missing activity", outcome: "running", status: "Running · phase unavailable"},
		{name: "old generation", outcome: "running", phase: "thinking", stale: true, status: "Running · phase unavailable"},
		{name: "queued without activity", outcome: "queued", status: "Queued for execution"},
		{name: "queued activity", outcome: "queued", phase: "queued", status: "waiting   12s"},
		{name: "queue overrides stream", outcome: "queued", phase: "generating", status: "Queued for execution"},
		{name: "parked without activity", outcome: "waiting", status: "Waiting for updates"},
		{name: "parked activity", outcome: "waiting", phase: "waiting", status: "waiting   12s"},
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
			status, warn, details := agentInspectionStatus(s, m, a, tc.approval, now)
			if status != tc.status || warn || len(details) != 0 {
				t.Fatalf("got %q, warn %v, %q; want %q without diagnostics", status, warn, details, tc.status)
			}
			// Rejected activity cannot leak its warning or request details.
			if tc.phase != "queued" && tc.phase != "waiting" || tc.approval {
				a.RetryCount, a.RetryReason = 2, "provider overloaded"
				if status, warn, details := agentInspectionStatus(s, m, a, tc.approval, now); status != tc.status || warn || len(details) != 0 {
					t.Fatalf("rejected activity leaked diagnostics: %q, warn %v, %q", status, warn, details)
				}
			}
		})
	}
	s, m, a, now := agentInspectionStatusFixture()
	a.Execution = "previous-execution"
	if status, warn, details := agentInspectionStatus(s, m, a, false, now); status != "Running · phase unavailable" || warn || len(details) != 0 {
		t.Fatalf("old execution activity leaked: %q, warn %v, %q", status, warn, details)
	}
	for _, missing := range []struct {
		s *swarm.State
		m *swarm.Member
	}{{nil, m}, {s, nil}} {
		if status, warn, details := agentInspectionStatus(missing.s, missing.m, a, false, now); status != "" || warn || len(details) != 0 {
			t.Fatalf("missing member/state: %q, warn %v, %q", status, warn, details)
		}
	}
	delete(s.Executions, m.Execution)
	delete(s.Tasks, m.Task)
	if status, warn, details := agentInspectionStatus(s, m, a, false, now); status != "Idle" || warn || len(details) != 0 {
		t.Fatalf("missing execution: %q, warn %v, %q", status, warn, details)
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
			status, warn, details := agentInspectionStatus(s, m, a, false, now)
			if status != tc.status || warn || !slices.Equal(details, tc.details) {
				t.Fatalf("got %q, warn %v, %q; want %q, %q", status, warn, details, tc.status, tc.details)
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
		if status, warn, details := agentInspectionStatus(s, m, a, false, now); status != want || warn || len(details) != 0 {
			t.Fatalf("got %q, warn %v, %q; want %q", status, warn, details, want)
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
	a.RequestActive, a.RequestStarted = true, now.Add(-6*time.Minute-14*time.Second)
	a.Since = a.RequestStarted
	s.Executions[m.Execution].ResumedAt = now.Add(-time.Hour)
	check := func(want string, wantWarn bool, wantDetails ...string) {
		t.Helper()
		status, warn, details := agentInspectionStatus(s, m, a, false, now)
		if status != want || warn != wantWarn || !slices.Equal(details, wantDetails) {
			t.Fatalf("got %q, warn %v, %q; want %q, warn %v, %q", status, warn, details, want, wantWarn, wantDetails)
		}
		if inline, inlineWarn := agentRowStatus(s, m, a, now); inline != status || inlineWarn != warn {
			t.Fatalf("inspection %q, warn %v differs from inline %q, warn %v", status, warn, inline, inlineWarn)
		}
	}
	// Streaming changes the word, not the request clock. ResumedAt is not
	// the clock's source, and inspection must not invent a lifetime duration.
	check("waiting    6m", false)
	for _, phase := range []string{"receiving", "thinking", "generating"} {
		a.Phase = phase
		check("thinking   6m", false)
	}
	a.Since = now.Add(-3 * time.Hour)
	check("thinking   3h", false)
	a.RequestStarted, a.Since = time.Time{}, time.Time{}
	check("thinking     ", false)
	a.Since = now.Add(time.Minute)
	check("thinking   0s", false)
	a.Phase, a.Since, a.RequestActive = "retrying", now.Add(-3*time.Second), false
	a.RetryCount, a.NextRetryAt, a.RetryReason = 2, now.Add(90*time.Second), "provider\n overloaded\ttry later"
	check("retrying   1m", true, "retry 2 in 1m30s · provider overloaded try later")
	// The inline countdown uses "now" below a minute; inspection retains
	// the precise countdown in its diagnostics, not a competing status row.
	a.NextRetryAt = now.Add(1500 * time.Millisecond)
	check("retrying  now", true, "retry 2 in 2s · provider overloaded try later")
	a.NextRetryAt = now.Add(-time.Second)
	check("retrying  now", true, "retry 2 in 0s · provider overloaded try later")
	a.RetryCount, a.NextRetryAt, a.RetryReason = 0, time.Time{}, ""
	a.Phase, a.RequestStarted, a.RequestActive, a.Attempt, a.StallTimeout = "requesting", now.Add(-59*time.Second), true, 2, time.Minute
	a.Since = a.RequestStarted
	check("waiting   59s", true, "attempt 2 · timeout in 1s")
}
