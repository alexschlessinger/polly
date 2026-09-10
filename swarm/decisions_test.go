package swarm

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/workflow"
)

// assertSettleMatchesBlockers checks that Settle's outcome is exactly the
// error the settlement blockers produce: same text, same typed code, same
// sentinels. It returns Settle's error for the caller's own assertions.
func assertSettleMatchesBlockers(t *testing.T, r *Runtime) error {
	t.Helper()
	ctx := context.Background()
	got := r.Settle(ctx)
	s, err := r.read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f := deriveFacts(s, r.ID)
	want := blockerError(f, settlementBlockers(f))
	if (got == nil) != (want == nil) || got != nil && got.Error() != want.Error() {
		t.Fatalf("Settle = %v\nblockers = %v", got, want)
	}
	var gotCode, wantCode *workflow.Error
	if errors.As(got, &gotCode) != errors.As(want, &wantCode) || gotCode != nil && gotCode.Code != wantCode.Code {
		t.Fatalf("blocker codes differ: %v vs %v", got, want)
	}
	if errors.Is(got, ErrBudget) != errors.Is(want, ErrBudget) {
		t.Fatalf("budget sentinel differs: %v vs %v", got, want)
	}
	var gotLimit, wantLimit *IterationLimitError
	if errors.As(got, &gotLimit) != errors.As(want, &wantLimit) {
		t.Fatalf("iteration limit wrapping differs: %v vs %v", got, want)
	}
	return got
}

// blockerFixture seeds one obstacle of every kind. Two tasks stay open so the
// task count prefix and the iteration limit wrapping are both exercised.
func blockerFixture() *State {
	s := &State{
		Runs:       map[string]*Run{"run": {ID: "run", Status: "paused", Starts: 4, Limit: 4}},
		Members:    map[string]*Member{},
		Tasks:      map[string]*Task{},
		Executions: map[string]*Execution{},
		Messages:   map[string]*Mail{},
		Workflows:  map[string]*workflow.Report{},
		Applies:    map[string]*ApplyRecord{"apply": {ID: "apply", Status: "recovery_required"}},
	}
	s.Messages["ask"] = &Mail{ID: "ask", From: "worker", To: "parent", Kind: "request", Text: "which branch?"}
	s.Workflows["done"] = &workflow.Report{ID: "done", Run: "run", Status: "completed"}
	s.Workflows["broken"] = &workflow.Report{ID: "broken", Run: "run", Status: "failed"}
	s.Members["researcher"] = &Member{ID: "researcher", ReadOnly: true, Task: "research", Execution: "e-research"}
	s.Executions["e-research"] = &Execution{ID: "e-research", Run: "run", Member: "researcher", Workflow: "done", Status: "completed"}
	s.Tasks["research"] = &Task{ID: "research", Run: "run", Owner: "researcher", Execution: "e-research", Status: "awaiting_review", Revision: 1}
	s.Members["worker"] = &Member{ID: "worker", Task: "a-limit", Execution: "e-limit"}
	s.Executions["e-limit"] = &Execution{ID: "e-limit", Run: "run", Member: "worker", Status: "paused", StopReason: messages.StopReasonMaxIterations, Iterations: 3, Request: AgentRequest{MaxIterations: 3}}
	s.Tasks["a-limit"] = &Task{ID: "a-limit", Run: "run", Owner: "worker", Execution: "e-limit", Status: "running", Revision: 2}
	s.Tasks["b-pending"] = &Task{ID: "b-pending", Run: "run", Status: "pending", Revision: 1}
	return s
}

func TestSettlementBlockersFollowSettleOrder(t *testing.T) {
	s := blockerFixture()
	f := deriveFacts(s, "parent")
	blockers := settlementBlockers(f)
	var kinds []string
	for _, b := range blockers {
		kinds = append(kinds, b.kind)
	}
	if want := []string{KindIntegration, KindMail, KindWorkflow, KindBudget, KindTask, KindTask, KindTask, KindWorkflow}; strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("blocker kinds = %v, want %v", kinds, want)
	}
	// The research task is covered by the completed workflow's blocker for
	// presentation, yet the settlement task set stays complete.
	if f.tasks[0].task.ID != "a-limit" || f.tasks[1].task.ID != "b-pending" || f.tasks[2].task.ID != "research" || f.tasks[2].covered != "done" {
		t.Fatalf("task facts = %+v", f.tasks)
	}
	// Each head kind reproduces today's sentence.
	cases := []struct {
		drop func(*State)
		want string
	}{
		{func(*State) {}, "integration apply has an unconfirmed outcome"},
		{func(s *State) { delete(s.Applies, "apply") }, "a member is waiting for a parent reply"},
		{func(s *State) { delete(s.Messages, "ask") }, "workflow done completed with 1 research result awaiting review; inspect workflow_read, then workflow_acknowledge to accept all of them, or swarm_review individual tasks first"},
		{func(s *State) { s.Workflows["done"].Acknowledged = true }, ErrBudget.Error()},
		{func(s *State) { s.Runs["run"].Status = "running" }, "3 tasks unsettled; first: task a-limit revision 2: member worker paused: iteration limit reached (3/3 model calls); saved work is retained. Grant additional calls with /swarm resume worker N"},
		{func(s *State) { delete(s.Tasks, "a-limit"); delete(s.Tasks, "b-pending"); delete(s.Tasks, "research") }, "workflow broken failed; inspect its report and explicitly acknowledge the failure after arranging recovery or reporting the blocker"},
	}
	var code *workflow.Error
	for i, tc := range cases {
		tc.drop(s)
		f := deriveFacts(s, "parent")
		err := blockerError(f, settlementBlockers(f))
		if err == nil || err.Error() != tc.want {
			t.Fatalf("case %d: blocker = %v, want %q", i, err, tc.want)
		}
		switch i {
		case 0:
			if !errors.As(err, &code) || code.Code != "recovery_required" {
				t.Fatalf("integration blocker code: %v", err)
			}
		case 3:
			if !errors.Is(err, ErrBudget) {
				t.Fatalf("budget blocker is not the sentinel: %v", err)
			}
		case 4:
			var limit *IterationLimitError
			if !errors.As(err, &limit) || limit.Session != "worker" {
				t.Fatalf("iteration limit lost behind the count: %v", err)
			}
		}
	}
	s.Workflows["broken"].Acknowledged = true
	f = deriveFacts(s, "parent")
	if err := blockerError(f, settlementBlockers(f)); err != nil {
		t.Fatalf("settled state still blocks: %v", err)
	}
}

// A workflow that caught one agent's iteration limit while another of its
// agents is parked: settlement keeps naming the limit through the count,
// with its typed error, because the blocker set is never filtered.
func TestIterationLimitSurvivesWorkflowOwnedTasks(t *testing.T) {
	r := runtimeTest(t, nilModel(), 2, 4)
	ctx := context.Background()
	if err := r.update(ctx, func(s *State) error {
		s.Runs["run"] = &Run{ID: "run", Status: "running", Starts: 2, Limit: 4}
		s.Workflows["wf"] = &workflow.Report{ID: "wf", Run: "run", Status: "running", Steps: []workflow.Step{{Operation: workflow.Operation{ID: "wf/a", Kind: "agent"}, Status: "running"}}}
		for _, id := range []string{"a", "b"} {
			s.Members[id] = &Member{ID: id, Task: "t-" + id, Execution: "e-" + id, Controller: "wf"}
			s.Tasks["t-"+id] = &Task{ID: "t-" + id, Run: "run", Owner: id, Execution: "e-" + id, Status: "running", Revision: 1}
		}
		s.Executions["e-a"] = &Execution{ID: "e-a", Run: "run", Member: "a", Workflow: "wf", Status: "paused", StopReason: messages.StopReasonMaxIterations, Iterations: 2, Request: AgentRequest{MaxIterations: 2}}
		s.Executions["e-b"] = &Execution{ID: "e-b", Run: "run", Member: "b", Workflow: "wf", Status: "waiting"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	err := assertSettleMatchesBlockers(t, r)
	var limit *IterationLimitError
	if err == nil || !strings.HasPrefix(err.Error(), "2 tasks unsettled; first: task t-a revision 1: member a paused: iteration limit reached (2/2 model calls)") || !errors.As(err, &limit) {
		t.Fatalf("workflow-owned iteration limit not reported: %v", err)
	}
}
