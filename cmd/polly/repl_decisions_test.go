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

// The status row and the picker summary lead with approvals, then decisions,
// then running agents.
func TestAgentsStatusPrefersApprovalsThenDecisions(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	tab := r.visibleTab()
	tab.viewTarget.ID = "parent"
	tab.swarmSnapshot = decisionSnapshot()
	r.model.mu.Lock()
	defer r.model.mu.Unlock()
	if text, color := r.agentsStatus(); text != "1 needs decision" || color != "active" {
		t.Fatalf("status = %q %q", text, color)
	}
	if summary := r.agentsSummary(tab.name); summary != "1 needs decision · 1 running" {
		t.Fatalf("picker summary = %q", summary)
	}
	tab.swarmSnapshot.Tasks["Q"] = &swarm.Task{ID: "Q", Run: "run", Status: "pending", Revision: 1}
	if text, _ := r.agentsStatus(); text != "2 need decision" {
		t.Fatalf("status = %q", text)
	}
	r.model.approval = &approvalState{requester: "m"}
	if text, _ := r.agentsStatus(); text != "1 needs approval" {
		t.Fatalf("status with an approval = %q", text)
	}
	// A member waiting on an approval counts as that, not as running.
	if summary := r.agentsSummary(tab.name); summary != "1 needs approval · 2 need decision" {
		t.Fatalf("picker summary with an approval = %q", summary)
	}
	r.model.approval = nil
	delete(tab.swarmSnapshot.Tasks, "P")
	delete(tab.swarmSnapshot.Tasks, "Q")
	if text, color := r.agentsStatus(); text != "1 agent running" || color != "run" {
		t.Fatalf("status with only running work = %q %q", text, color)
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
