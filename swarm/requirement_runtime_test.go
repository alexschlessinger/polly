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
	result, err := r.Agent(ctx, "", AgentRequest{Task: "review", ReadOnly: true, Review: true})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := r.read(ctx)
	if s.Tasks[result.Task].Requirement != RequirementReviewed {
		t.Fatal("agent lost review request")
	}
	child, err := r.Spawn(ctx, subagent.Request{Task: "review spawned", ReadOnly: true, Review: true})
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
	if _, err := r.Agent(ctx, "", AgentRequest{Task: "invalid", Review: true}); err == nil {
		t.Fatal("editing review request accepted")
	}
	after, _ := r.read(ctx)
	if len(after.Contexts) != len(s.Contexts) {
		t.Fatal("invalid request allocated workspace")
	}
}
