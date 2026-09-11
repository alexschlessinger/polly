package swarm

import (
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/workflow"
	"github.com/alexschlessinger/pollytool/worktree"
)

func TestCandidateReadyItemsAndBatchNext(t *testing.T) {
	x := seedState()
	x.member("editor-a", false, "a", "")
	a := x.task("a", "editor-a", "", "awaiting_review")
	p := x.present()
	if len(p.Decisions) != 1 || p.Decisions[0].Why != "candidate ready" || !strings.Contains(p.Decisions[0].Action, integrateTasksAction([]TaskReference{{Task: a.ID, Revision: a.Revision}})) {
		t.Fatalf("single ready %+v", p)
	}
	x.member("editor-b", false, "b", "")
	b := x.task("b", "editor-b", "", "awaiting_review")
	p = x.present()
	want := integrateTasksAction([]TaskReference{{Task: a.ID, Revision: a.Revision}, {Task: b.ID, Revision: b.Revision}})
	if len(p.Decisions) != 2 || !strings.Contains(p.Next, want) || !strings.Contains(p.Next, "one call") {
		t.Fatalf("batch next %+v", p)
	}
	x.member("researcher", true, "research", "")
	x.task("research", "researcher", "", "awaiting_review").Requirement = RequirementReviewed
	p = x.present()
	if p.Decisions[2].Why != "awaiting parent review" || p.Decisions[2].Action != "accept this revision or request changes" {
		t.Fatalf("research changed %+v", p.Decisions[2])
	}
}

// A valid candidate with fake immutable objects is sufficient to exercise the
// projection without doing filesystem work or persisting a display label.
func haltedState() (*State, *Task, *IntegrationCandidate) {
	x := seedState()
	x.member("editor", false, "task", "exec")
	e := x.exec("exec", "editor", "completed", "")
	t := x.task("task", "editor", "exec", "awaiting_review")
	e.Run = t.Run
	t.Requirement = RequirementApplied
	base := worktree.Snapshot{ID: "base", Tree: "base-tree", Commit: "base-commit", Source: "parent"}
	submitted := worktree.Snapshot{ID: "submitted", Tree: "submitted-tree", Commit: "submitted-commit", Source: "child"}
	t.StartingSnapshot, t.Snapshot, t.AcceptedRevision = base.ID, submitted.ID, t.Revision
	x.s.Snapshots = map[string]*worktree.Snapshot{base.ID: &base, submitted.ID: &submitted}
	c := &IntegrationCandidate{ID: "candidate", Run: t.Run, Status: "conflicted", Inputs: []IntegrationInput{{TaskReference: TaskReference{Task: t.ID, Revision: t.Revision}, Base: base, Submitted: submitted}}, Parent: base, Merged: submitted, Created: time.Now().UTC()}
	x.s.Integrations = map[string]*IntegrationCandidate{c.ID: c}
	return x.s, t, c
}

func TestTaskStatusInReportsHalt(t *testing.T) {
	s, task, c := haltedState()
	if TaskStatusIn(s, task) != "integration halted" || TaskStatus(task) != "integration pending" {
		t.Fatal("halt not derived independently")
	}
	task.Status = "done"
	if TaskStatusIn(s, task) != "done" {
		t.Fatal("done became halted")
	}
	task.Status = "canceled"
	if TaskStatusIn(s, task) != "canceled" {
		t.Fatal("canceled became halted")
	}
	task.Status = "awaiting_review"
	task.Requirement = RequirementReviewed
	s.Members[task.Owner].ReadOnly = true
	if TaskStatusIn(s, task) == "integration halted" {
		t.Fatal("research became halted")
	}
	task.Requirement = RequirementApplied
	s.Members[task.Owner].ReadOnly = false
	newer := *c
	newer.ID = "newer"
	newer.Status = "ready"
	s.Integrations[newer.ID] = &newer
	if currentCandidate(s, task) != &newer {
		t.Fatal("equal timestamp did not break tie by ID")
	}
	newer.Created = c.Created.Add(-time.Second)
	if currentCandidate(s, task) != c {
		t.Fatal("newest timestamp not selected")
	}
	c.Status = "superseded"
	c.Successor = newer.ID
	if currentCandidate(s, task) != &newer || TaskStatusIn(s, task) == "integration halted" {
		t.Fatal("superseded conflict still current")
	}
	task.Revision++
	if currentCandidate(s, task) != nil {
		t.Fatal("stale candidate still current")
	}
}

func TestParentDispositionReportsIntegrationHalt(t *testing.T) {
	s, task, _ := haltedState()
	r := &Runtime{}
	if p := r.ParentState(s); p.Display != "idle · integration halted" {
		t.Fatalf("parent %+v", p)
	}
	s.Tasks["research"] = &Task{ID: "research", Run: task.Run, Status: "awaiting_review", Revision: 1, Requirement: RequirementReviewed}
	if p := r.ParentState(s); p.Detail != "integration halted" {
		t.Fatalf("research masked halt %+v", p)
	}
	// Valid deferral removes this task from parent attention without changing
	// the retained candidate or the task's own diagnostic.
	e := s.Executions[task.Execution]
	e.Workflow = "failed"
	s.Workflows["failed"] = &workflow.Report{ID: "failed", Run: task.Run, Status: "failed", Acknowledged: true}
	task.Deferral = &TaskDeferral{Workflow: "failed", Execution: e.ID, Owner: task.Owner, Revision: task.Revision, Generation: e.Generation, AcceptedRevision: task.AcceptedRevision, Status: task.Status, Note: "later"}
	if !TaskDeferred(s, task) {
		t.Fatal("invalid deferral fixture")
	}
	if p := r.ParentState(s); p.Detail != "awaiting review" {
		t.Fatalf("deferred halt still counted %+v", p)
	}
	delete(s.Tasks, "research")
	s.Workflows["delivery"] = &workflow.Report{ID: "delivery", Status: "completed"}
	if p := r.ParentState(s); p.Detail != "delivering" {
		t.Fatalf("delivery label %+v", p)
	}
	s.Workflows["delivery"].Acknowledged = true
	if p := r.ParentState(s); p.Detail != "done" {
		t.Fatalf("settled projection %+v", p)
	}
}
