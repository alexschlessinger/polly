package main

import (
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/swarm"
	"github.com/alexschlessinger/pollytool/workflow"
)

// decisionSnapshot is a swarm with one busy member and one ownerless pending
// task: work running and a decision due at the same time.
func decisionSnapshot() *swarm.State {
	return &swarm.State{
		Runs:       map[string]*swarm.Run{"run": {ID: "run", Status: "running", Starts: 1, Limit: 4}},
		Members:    map[string]*swarm.Member{"m": {ID: "m", Name: "m", Label: "worker", Task: "T", Execution: "e"}},
		Executions: map[string]*swarm.Execution{"e": {ID: "e", Run: "run", Member: "m", Status: "running"}},
		Tasks: map[string]*swarm.Task{
			"T": {ID: "T", Run: "run", Owner: "m", Execution: "e", Status: "running", Revision: 1},
			"P": {ID: "P", Run: "run", Status: "pending", Revision: 1},
		},
		Messages:  map[string]*swarm.Mail{},
		Workflows: map[string]*workflow.Report{},
	}
}

// The status row shows approvals and execution activity; coordination
// decisions remain visible in the picker summary.
func TestAgentsStatusOmitsDecisions(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	tab := r.visibleTab()
	tab.viewTarget.ID = "parent"
	tab.swarmSnapshot = decisionSnapshot()
	r.model.mu.Lock()
	defer r.model.mu.Unlock()
	if text, styled := r.agentsStatus(); text != "Agents · 1 running" || plainStyledText(styled) != text {
		t.Fatalf("status = %q %q", text, styled)
	}
	if summary := r.agentsSummary(tab.name); summary != "1 needs decision · 1 running" {
		t.Fatalf("picker summary = %q", summary)
	}
	tab.swarmSnapshot.Tasks["Q"] = &swarm.Task{ID: "Q", Run: "run", Status: "pending", Revision: 1}
	if text, _ := r.agentsStatus(); text != "Agents · 1 running" {
		t.Fatalf("status = %q", text)
	}
	r.model.approval = &approvalState{requester: "m"}
	if text, _ := r.agentsStatus(); text != "Agents · 1 needs approval" {
		t.Fatalf("status with an approval = %q", text)
	}
	// A member waiting on an approval counts as that, not as running.
	if summary := r.agentsSummary(tab.name); summary != "1 needs approval · 2 need decision" {
		t.Fatalf("picker summary with an approval = %q", summary)
	}
	r.model.approval = nil
	delete(tab.swarmSnapshot.Tasks, "P")
	delete(tab.swarmSnapshot.Tasks, "Q")
	if text, _ := r.agentsStatus(); text != "Agents · 1 running" {
		t.Fatalf("status with only running work = %q", text)
	}
}

// Every agent counts once: a member parked on a message still runs, and one
// whose latest run ended counts as finished however it ended. The label stays
// once nothing runs and disappears only with the last agent.
func TestAgentsStatusCountsFinishedAgents(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	tab := r.visibleTab()
	tab.viewTarget.ID = "parent"
	s := decisionSnapshot()
	tab.swarmSnapshot = s
	for id, status := range map[string]string{"parked": "waiting", "ok": "completed", "broke": "failed", "cut": "interrupted"} {
		s.Members[id] = &swarm.Member{ID: id, Name: id, Execution: id}
		s.Executions[id] = &swarm.Execution{ID: id, Run: "run", Member: id, Status: status}
	}
	r.model.mu.Lock()
	defer r.model.mu.Unlock()
	if text, styled := r.agentsStatus(); text != "Agents · 2 running · 3 finished" || plainStyledText(styled) != text {
		t.Fatalf("status = %q %q", text, styled)
	}
	s.Executions["e"].Status, s.Executions["parked"].Status = "completed", "completed"
	if text, _ := r.agentsStatus(); text != "Agents · 5 finished" {
		t.Fatalf("status once nothing runs = %q", text)
	}
	s.Members = map[string]*swarm.Member{}
	if text, _ := r.agentsStatus(); text != "" {
		t.Fatalf("status without agents = %q", text)
	}
}

// The badge lands on its decision: a member when one owns it, else the swarm
// inspector section that lists it; an approval still wins.
func TestDecisionBadgeOpensAgentsList(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	tab := r.visibleTab()
	tab.viewTarget.ID = "parent"
	s := decisionSnapshot()
	tab.swarmSnapshot = s
	r.model.mu.Lock()
	defer r.model.mu.Unlock()
	if member, section := r.attentionTarget(); member != "" || section != "tasks" {
		t.Fatalf("ownerless pending task routes to %q/%q", member, section)
	}
	r.openAttention()
	if w := r.workspace(); !w.inspector.open || w.inspector.target.kind != agentsViewKind {
		t.Fatalf("badge did not open the agents list: %+v", w.inspector.target)
	}
	s.Messages["ask"] = &swarm.Mail{ID: "ask", From: "m", To: tab.viewID(), Kind: "request", Text: "?"}
	if member, section := r.attentionTarget(); member != "m" || section != "" {
		t.Fatalf("request routes to %q/%q", member, section)
	}
	delete(s.Messages, "ask")
	s.Applies = map[string]*swarm.ApplyRecord{"apply": {ID: "apply", Status: "recovery_required"}}
	if member, section := r.attentionTarget(); member != "" || section != "integrations" {
		t.Fatalf("uncertain apply routes to %q/%q", member, section)
	}
	s.Applies = nil
	s.Workflows["wf"] = &workflow.Report{ID: "wf", Run: "run", Status: "failed"}
	delete(s.Tasks, "P")
	if member, section := r.attentionTarget(); member != "" || section != "workflows" {
		t.Fatalf("failed workflow routes to %q/%q", member, section)
	}
	r.model.approval = &approvalState{requester: "m"}
	if member, section := r.attentionTarget(); member != "m" || section != "" {
		t.Fatalf("approval lost priority: %q/%q", member, section)
	}
}

// The /swarm members view leads with the decisions and the work in flight.
func TestSwarmInspectorLeadsWithDecisions(t *testing.T) {
	s := decisionSnapshot()
	parent := swarm.AgentPresentation{Lifecycle: swarm.LifecycleIdle, Display: "idle"}
	text := swarmInspectorTextFor(s, &parent, "members", "parent")
	want := "Parent — idle\n\nNeeds decision (1)\ntask P revision 1: pending; assign and run the task or cancel it\n\nWorking (1)\nworker · active · task T\n\n1 members · 2 tasks"
	if !strings.HasPrefix(text, want) {
		t.Fatalf("members view:\n%s", text)
	}
	if text := swarmInspectorTextFor(s, &parent, "tasks", "parent"); strings.Contains(text, "Needs decision") {
		t.Fatalf("task section carries the decision list: %q", text)
	}
}
