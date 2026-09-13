package swarm

import (
	"context"
	"github.com/alexschlessinger/pollytool/subagent"
	"reflect"
	"testing"
)

func TestRequirementEntrypoints(t *testing.T) {
	ctx := context.Background()
	r := runtimeTest(t, nilModel(), 1, 10)
	result, err := r.Agent(ctx, "", AgentRequest{Label: "Test agent", Task: "review", ReadOnly: true, Review: true})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := r.read(ctx)
	if s.Tasks[result.Task].Requirement != RequirementReviewed {
		t.Fatal("agent lost review request")
	}
	child, err := r.Spawn(ctx, subagent.Request{Label: "Test agent", Task: "review spawned", ReadOnly: true, Review: true})
	if err != nil {
		t.Fatal(err)
	}
	s, _ = r.read(ctx)
	if s.Tasks[s.Members[child.Session].Task].Requirement != RequirementReviewed {
		t.Fatal("spawn lost review request")
	}
	task, err := r.CreateTask(ctx, "edit", "custom criteria", nil, "", CreateTaskOptions{Requirement: RequirementApplied})
	if err != nil {
		t.Fatal(err)
	}
	before := *task
	if err := r.UpdateTask(ctx, task.ID, task.Revision, result.Session, nil); err == nil {
		t.Fatal("reassigned editing obligation to researcher")
	}
	s, _ = r.read(ctx)
	if !reflect.DeepEqual(*s.Tasks[task.ID], before) {
		t.Fatal("refused reassignment mutated the task")
	}
	if _, err := r.Agent(ctx, "", AgentRequest{Label: "Test agent", Task: "invalid", Review: true}); err == nil {
		t.Fatal("editing review request accepted")
	}
	after, _ := r.read(ctx)
	if len(after.Contexts) != len(s.Contexts) {
		t.Fatal("invalid request allocated workspace")
	}
}

func TestReassignmentCannotWeakenDependentEditingObligation(t *testing.T) {
	r := runtimeTest(t, doneModel(), 1, 3)
	ctx := context.Background()
	research, err := r.Agent(ctx, "", AgentRequest{Label: "Test agent", Task: "review", ReadOnly: true, Review: true})
	if err != nil {
		t.Fatal(err)
	}
	task, err := r.CreateTask(ctx, "edit", "", nil, "", CreateTaskOptions{Requirement: RequirementApplied})
	if err != nil {
		t.Fatal(err)
	}
	dependent, err := r.CreateTask(ctx, "verify", "", []string{task.ID}, "")
	if err != nil {
		t.Fatal(err)
	}
	before := *task
	if err := r.UpdateTask(ctx, task.ID, task.Revision, research.Session, nil); err == nil {
		t.Fatal("editing obligation converted to delivered research")
	}
	if err := r.Claim(ctx, research.Session, task.ID, task.Revision); err == nil {
		t.Fatal("researcher claimed editing obligation")
	}
	s, _ := r.read(ctx)
	if !reflect.DeepEqual(*s.Tasks[task.ID], before) || depsDone(s, s.Tasks[dependent.ID]) {
		t.Fatal("failed reassignment changed dependencies or provenance")
	}
	if err := r.CancelTask(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	s, _ = r.read(ctx)
	if depsDone(s, s.Tasks[dependent.ID]) {
		t.Fatal("cancellation satisfied a dependency")
	}
}
