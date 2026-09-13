package swarm

import (
	"context"
	"strings"
	"testing"
)

// Acknowledging a completed workflow accepts the read-only research its script
// consumed without reviewing; results the script reviewed and snapshot-backed
// candidates in the same run are left alone.
func TestAcknowledgeCompletedWorkflowDoesNotAcceptTasks(t *testing.T) {
	// Two starts: the script's rejection leaves member a with undelivered
	// request mail, and a third start would let the post-workflow wake spend it.
	r := runtimeTest(t, nilModel(), 1, 2)
	ctx := context.Background()
	report, err := r.RunWorkflow(ctx, `polly.defineWorkflow({name:"consume",inputSchema:polly.schema.object({}),async run(){
const a=await polly.agent({label:"Test agent",task:"investigate a",readOnly:true,review:true});
const b=await polly.agent({label:"Test agent",task:"investigate b",readOnly:true});
const t=await polly.tasks.read(a.task);
await polly.tasks.review({task:t.id,revision:t.revision,accept:false,feedback:"look deeper"});
return {a:a.task,b:b.task};
}})`, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run := s.Workflows[report.ID].Run
	byDescription := func(s *State, description string) *Task {
		t.Helper()
		for _, task := range s.Tasks {
			if task.Description == description {
				return task
			}
		}
		t.Fatalf("missing task %q", description)
		return nil
	}
	// Snapshot-backed tasks in the same run must survive acknowledgment untouched.
	if err := r.update(ctx, func(s *State) error {
		for _, tc := range []struct {
			id, member string
			readOnly   bool
		}{{"edit", "editor", false}, {"snap", "snapper", true}} {
			s.Members[tc.member] = &Member{ID: tc.member, Name: tc.member, ReadOnly: tc.readOnly, Task: tc.id, Execution: tc.id + "-run"}
			s.Executions[tc.id+"-run"] = &Execution{ID: tc.id + "-run", Workflow: report.ID, Run: run, Member: tc.member, Status: "completed", Generation: 1}
			s.Tasks[tc.id] = &Task{ID: tc.id, Run: run, Owner: tc.member, Execution: tc.id + "-run", Status: "awaiting_review", Revision: 2, Snapshot: "candidate", Description: tc.id}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	err = r.AcknowledgeWorkflow(ctx, report.ID)
	if err != nil {
		t.Fatalf("acknowledge = %d, %v; want 1 accepted", 0, err)
	}
	if s, err = r.State(ctx); err != nil {
		t.Fatal(err)
	}
	if b := byDescription(s, "investigate b"); b.Status != "done" || b.AcceptedRevision != 0 || b.Delivery == nil {
		t.Fatalf("consumed research not accepted: %+v", b)
	}
	if a := byDescription(s, "investigate a"); a.Status != "changes_requested" || a.AcceptedRevision != 0 {
		t.Fatalf("script-reviewed task changed: %+v", a)
	}
	for _, id := range []string{"edit", "snap"} {
		if task := s.Tasks[id]; task.Status != "awaiting_review" || task.AcceptedRevision != 0 {
			t.Fatalf("snapshot-backed task %s changed: %+v", id, task)
		}
	}
	if s.Workflows[report.ID].Acknowledged || len(s.Applies) != 0 {
		t.Fatalf("acknowledgment state: acknowledged=%v applies=%d", s.Workflows[report.ID].Acknowledged, len(s.Applies))
	}
	admitParent(t, r)
	s, _ = r.read(ctx)
	if !s.Workflows[report.ID].Acknowledged {
		t.Fatal("delivered output was not acknowledged")
	}
	if err := r.AcknowledgeWorkflow(ctx, report.ID); err != nil {
		t.Fatalf("second acknowledge = %d, %v; want nothing left to accept", 0, err)
	}
}

func TestWorkflowAcknowledgeToolIsHarmlessOnCompletedReport(t *testing.T) {
	r := runtimeTest(t, nilModel(), 1, 2)
	ctx := context.Background()
	report, err := r.RunWorkflow(ctx, `polly.defineWorkflow({name:"one",inputSchema:polly.schema.object({}),async run(){return await polly.agent({label:"Test agent",task:"investigate",readOnly:true});}})`, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	r.RegisterParentTools(r.config.Registry)
	tool, _, _ := r.config.Registry.GetIfAllowed("swarm_control")
	if tool == nil {
		t.Fatal("swarm_control is not registered")
	}
	if !strings.Contains(tool.GetSchema().Description(), "Completed reports need no acknowledgment") {
		t.Fatalf("description: %s", tool.GetSchema().Description())
	}
	if out, err := tool.Execute(ctx, map[string]any{"action": "acknowledge_workflow", "id": report.ID}); err != nil || out != `"acknowledged"` {
		t.Fatalf("first acknowledge = %q, %v", out, err)
	}
	if out, err := tool.Execute(ctx, map[string]any{"action": "acknowledge_workflow", "id": report.ID}); err != nil || out != `"acknowledged"` {
		t.Fatalf("second acknowledge = %q, %v", out, err)
	}
}

// A terminal failure keeps acknowledgment as bookkeeping only.
func TestAcknowledgeFailedWorkflowDoesNotAcceptTasks(t *testing.T) {
	r, id, task := failedResearchWorkflow(t)
	ctx := context.Background()
	if err := r.AcknowledgeWorkflow(ctx, id); err != nil {
		t.Fatalf("acknowledge = %d, %v; want nothing accepted", 0, err)
	}
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Tasks[task.ID]; got.Status != "awaiting_review" || got.AcceptedRevision != 0 {
		t.Fatalf("failed workflow's research changed: %+v", got)
	}
	if !s.Workflows[id].Acknowledged {
		t.Fatal("acknowledgment not recorded")
	}
	if err := r.Settle(ctx); err == nil || !strings.Contains(err.Error(), task.ID) || !strings.Contains(err.Error(), "awaiting parent review") {
		t.Fatalf("settlement after acknowledging a failure = %v", err)
	}
}
