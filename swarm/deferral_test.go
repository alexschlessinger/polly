package swarm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/workflow"
)

func failedResearchWorkflow(t *testing.T) (*Runtime, string, *Task) {
	t.Helper()
	r := runtimeTest(t, nilModel(), 1, 4)
	report, err := r.RunWorkflow(context.Background(), `polly.defineWorkflow({name:"defer fixture",inputSchema:polly.schema.object({}),async run(){await polly.agent({task:"investigate",readOnly:true});polly.fail("verification incomplete")}})`, map[string]any{})
	if err == nil {
		t.Fatal("workflow should fail")
	}
	s, err := r.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range s.Tasks {
		return r, report.ID, task
	}
	t.Fatal("missing research task")
	return nil, "", nil
}

func finishParent(t *testing.T, r *Runtime) {
	t.Helper()
	cb := &llm.AgentCallbacks{}
	r.bindParent(cb, nil)
	nudge, err := cb.ContinueAfterFinal(context.Background(), nil)
	if err != nil || len(nudge) != 0 {
		t.Fatalf("parent did not finish: %v %v", nudge, err)
	}
}

func TestWorkflowDeferralPersistsWithoutAcceptingOrApplying(t *testing.T) {
	r, id, task := failedResearchWorkflow(t)
	ctx := context.Background()
	if _, err := r.AcknowledgeWorkflow(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := r.Settle(ctx); err == nil {
		t.Fatal("acknowledgment alone accepted unresolved work")
	}
	if err := r.DeferWorkflow(ctx, id, " "); err == nil {
		t.Fatal("blank explanation accepted")
	}
	for range 2 {
		if err := r.DeferWorkflow(ctx, id, "Reported incomplete verification; retain for review"); err != nil {
			t.Fatal(err)
		}
	}
	finishParent(t, r)
	s, _ := r.State(ctx)
	if !TaskDeferred(s, s.Tasks[task.ID]) || s.Tasks[task.ID].Status != "awaiting_review" || s.Tasks[task.ID].AcceptedRevision != 0 || len(s.Applies) != 0 || s.Runs[task.Run].Status != "completed" {
		t.Fatalf("deferral changed disposition: %+v", s.Tasks[task.ID])
	}
	if s.Members[task.Owner].Control != MemberControlEnabled || MemberState(s, s.Members[task.Owner]).Outcome != "completed" {
		t.Fatal("failed workflow paused its completed agent")
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := New(r.config)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	s, _ = restored.State(ctx)
	if !TaskDeferred(s, s.Tasks[task.ID]) {
		t.Fatal("deferral did not survive restart")
	}
	finishParent(t, restored)
	if _, err := restored.inspectTasks(ctx, map[string]any{"task": task.ID, "section": "result"}); err != nil {
		t.Fatal(err)
	}
	s, _ = restored.State(ctx)
	if !TaskDeferred(s, s.Tasks[task.ID]) {
		t.Fatal("inspection reactivated work")
	}
	if err := restored.Review(ctx, task.ID, task.Revision, true, ""); err != nil {
		t.Fatal(err)
	}
	s, _ = restored.State(ctx)
	if s.Tasks[task.ID].Deferral != nil || s.Tasks[task.ID].Status != "done" || s.Runs[task.Run].Starts != 1 || s.Runs[task.Run].Limit != 4 {
		t.Fatal("review lost task or budget")
	}
	finishParent(t, restored)
}

func TestDeferralDoesNotHideOtherWorkOrChangedState(t *testing.T) {
	for _, kind := range []string{"revision", "generation", "acceptance", "execution", "request", "apply", "other task", "active"} {
		t.Run(kind, func(t *testing.T) {
			r, id, task := failedResearchWorkflow(t)
			ctx := context.Background()
			if err := r.DeferWorkflow(ctx, id, "later"); err != nil {
				t.Fatal(err)
			}
			if err := r.update(ctx, func(s *State) error {
				task := s.Tasks[task.ID]
				switch kind {
				case "revision":
					task.Revision++
				case "generation":
					s.Executions[task.Execution].Generation++
				case "acceptance":
					task.AcceptedRevision = task.Revision
				case "execution":
					task.Execution = "missing"
				case "request":
					s.Messages["request"] = &Mail{ID: "request", To: r.ID, Kind: "request"}
				case "apply":
					s.Applies["apply"] = &ApplyRecord{ID: "apply", Status: "recovery_required"}
				case "other task":
					s.Tasks["other"] = &Task{ID: "other", Run: task.Run, Status: "pending"}
				case "active":
					s.Executions[task.Execution].Status = "running"
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := assertSettleMatchesBlockers(t, r); err == nil {
				t.Fatal("unsafe or unrelated work was hidden")
			}
		})
	}
}

func TestDeferredRecoveryWaitsForNewerRun(t *testing.T) {
	r, id, task := failedResearchWorkflow(t)
	ctx := context.Background()
	if err := r.DeferWorkflow(ctx, id, "later"); err != nil {
		t.Fatal(err)
	}
	finishParent(t, r)
	other, err := r.CreateTask(ctx, "new work", "review", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func() error{
		func() error { return r.Review(ctx, task.ID, task.Revision, true, "") },
		func() error { return r.Resume(ctx, task.Owner, 0) },
	} {
		if err := mutate(); err == nil || !strings.Contains(err.Error(), "current run") {
			t.Fatalf("concurrent recovery: %v", err)
		}
	}
	s, _ := r.State(ctx)
	if !TaskDeferred(s, s.Tasks[task.ID]) || s.Runs[task.Run].Starts != 1 {
		t.Fatal("refused recovery mutated work")
	}
	if err := r.CancelTask(ctx, other.ID); err != nil {
		t.Fatal(err)
	}
	finishParent(t, r)
	if err := r.Review(ctx, task.ID, task.Revision, true, ""); err != nil {
		t.Fatal(err)
	}
	finishParent(t, r)
}

func TestDeferralCannotUseDisplayProvenanceOrForeignOwnership(t *testing.T) {
	r, id, task := failedResearchWorkflow(t)
	ctx := context.Background()
	if err := r.update(ctx, func(s *State) error {
		e := s.Executions[task.Execution]
		e.Workflow = ""
		// The report step call ID and controller still link this for display.
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.DeferWorkflow(ctx, id, "later"); err != nil {
		t.Fatal(err)
	}
	s, _ := r.State(ctx)
	if TaskDeferred(s, s.Tasks[task.ID]) || s.Tasks[task.ID].Deferral != nil {
		t.Fatal("display provenance granted deferral authority")
	}
	if err := r.DeferWorkflow(ctx, "other-family", "later"); err == nil {
		t.Fatal("foreign workflow accepted")
	}
}

func TestDeferredIterationRecoveryKeepsAccounting(t *testing.T) {
	r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
		return iterationTool("inspect", "list_agents", `{}`)
	}), 1, 4)
	r.UpdateDefaults(r.config.Request, llm.AgentConfig{MaxIterations: 1}, nil)
	ctx := context.Background()
	report, err := r.RunWorkflow(ctx, `polly.defineWorkflow({name:"limited",inputSchema:polly.schema.object({}),async run(){await polly.agent({task:"inspect",readOnly:true})}})`, map[string]any{})
	if err == nil {
		t.Fatal("iteration-limited workflow succeeded")
	}
	var task *Task
	s, _ := r.State(ctx)
	for _, v := range s.Tasks {
		task = v
	}
	if task == nil {
		t.Fatal("missing task")
	}
	if err := r.DeferWorkflow(ctx, report.ID, "Budget exhausted; resume later"); err != nil {
		t.Fatal(err)
	}
	finishParent(t, r)
	if err := r.Resume(ctx, task.Owner, 0); !llm.IsIterationLimit(err) {
		t.Fatalf("resume granted calls: %v", err)
	}
	s, _ = r.State(ctx)
	if !TaskDeferred(s, s.Tasks[task.ID]) {
		t.Fatal("failed resume cleared deferral")
	}
	r.config.Client = nilModel()
	if err := r.ResumeWithIterations(ctx, task.Owner, 1); err != nil {
		t.Fatal(err)
	}
	s = waitWorkflowIdle(t, r)
	e := s.Executions[task.Execution]
	if e.Iterations != 2 || e.Status != "completed" || s.Runs[task.Run].Starts != 1 || len(s.Executions) != 1 || s.Tasks[task.ID].Deferral != nil {
		t.Fatalf("recovery accounting: %+v", e)
	}
}

func TestDeferralCanSettleExhaustedRun(t *testing.T) {
	r, id, task := failedResearchWorkflow(t)
	ctx := context.Background()
	if err := r.update(ctx, func(s *State) error {
		s.Runs[task.Run].Status = "paused"
		s.Runs[task.Run].Limit = s.Runs[task.Run].Starts
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.DeferWorkflow(ctx, id, "launch budget exhausted"); err != nil {
		t.Fatal(err)
	}
	finishParent(t, r)
	s, _ := r.State(ctx)
	if s.Runs[task.Run].Limit != 1 {
		t.Fatal("deferral granted starts")
	}
}

func TestRefusedDeferredRestartRestoresDeferral(t *testing.T) {
	r, id, task := failedResearchWorkflow(t)
	ctx := context.Background()
	if err := r.update(ctx, func(s *State) error { s.Runs[task.Run].Limit = 1; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := r.DeferWorkflow(ctx, id, "retain without additional starts"); err != nil {
		t.Fatal(err)
	}
	finishParent(t, r)
	if err := r.Resume(ctx, task.Owner, 0); err == nil {
		t.Fatal("restart granted a start")
	}
	s, _ := r.State(ctx)
	if !TaskDeferred(s, s.Tasks[task.ID]) || s.Runs[task.Run].Status != "completed" || s.Runs[task.Run].Starts != 1 || len(s.Executions) != 1 {
		t.Fatal("refused restart reactivated retained work")
	}
	finishParent(t, r)
}

func TestDeferredEditingCandidateRemainsIsolated(t *testing.T) {
	r, plan := applyFixture(t, false)
	ctx := context.Background()
	ref := submittedInput(t, r, plan.Parent, map[string]string{"a.txt": "retained editor change\n"})
	if err := r.update(ctx, func(s *State) error {
		delete(s.Tasks, "task")
		s.Integrations = map[string]*IntegrationCandidate{}
		task := s.Tasks[ref.Task]
		task.Execution = "editing-execution"
		member := s.Members[task.Owner]
		member.Execution = task.Execution
		member.Controller = "failed-audit"
		s.Executions[task.Execution] = &Execution{ID: task.Execution, Run: task.Run, Member: task.Owner, Workflow: member.Controller, Status: "completed", Generation: 1}
		s.Workflows[member.Controller] = &workflow.Report{ID: member.Controller, Run: task.Run, Status: "failed"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.DeferWorkflow(ctx, "failed-audit", "post-repair verification failed"); err != nil {
		t.Fatal(err)
	}
	finishParent(t, r)
	s, _ := r.State(ctx)
	task := s.Tasks[ref.Task]
	member := s.Members[task.Owner]
	if task.AcceptedRevision != 0 || task.Status != "awaiting_review" || !TaskDeferred(s, task) || len(s.Applies) != 0 || s.Snapshots[task.Snapshot] == nil || s.Contexts[member.Context] == nil {
		t.Fatal("deferral accepted or discarded candidate")
	}
	parent, err := os.ReadFile(filepath.Join(r.config.Root, "a.txt"))
	if err != nil || string(parent) != "base\n" {
		t.Fatal("deferral changed parent tree")
	}
	edited, err := os.ReadFile(filepath.Join(s.Contexts[member.Context].Root, "a.txt"))
	if err != nil || string(edited) != "retained editor change\n" {
		t.Fatal("deferral discarded editor worktree")
	}
	candidate, err := r.PrepareIntegration(ctx, []TaskReference{ref}, "paths")
	if err != nil {
		t.Fatal(err)
	}
	other, err := r.CreateTask(ctx, "new run", "review", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.AcceptIntegration(ctx, candidate.ID); err == nil || !strings.Contains(err.Error(), "current run") {
		t.Fatalf("integration bypassed deferred recovery: %v", err)
	}
	if err := r.CancelTask(ctx, other.ID); err != nil {
		t.Fatal(err)
	}
	finishParent(t, r)
	if _, err := r.AcceptIntegration(ctx, candidate.ID); err != nil {
		t.Fatal(err)
	}
	if err := r.DeferWorkflow(ctx, "failed-audit", "accepted candidate retained before apply"); err != nil {
		t.Fatal(err)
	}
	finishParent(t, r)
	other, err = r.CreateTask(ctx, "another run", "review", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.ApplyIntegration(ctx, candidate.ID); err == nil || !strings.Contains(err.Error(), "current run") {
		t.Fatalf("apply bypassed deferred recovery: %v", err)
	}
	if err := r.CancelTask(ctx, other.ID); err != nil {
		t.Fatal(err)
	}
	finishParent(t, r)
	if _, err := r.ApplyIntegration(ctx, candidate.ID); err != nil {
		t.Fatal(err)
	}
	s, _ = r.State(ctx)
	if s.Tasks[ref.Task].Deferral != nil || s.Tasks[ref.Task].Status != "done" {
		t.Fatal("explicit integration did not reactivate and finish task")
	}

}

func TestHistoricalMemberOutcomeIsDisplayOnly(t *testing.T) {
	s := &State{Executions: map[string]*Execution{"e": {ID: "e", Member: "m", Status: "completed"}}, Tasks: map[string]*Task{"t": {ID: "t", Status: "done"}}, Workflows: map[string]*workflow.Report{}}
	m := &Member{ID: "m", Execution: "e", Task: "t"}
	p := MemberState(s, m)
	if p.Outcome != "completed" || p.Lifecycle != LifecycleIdle || p.Display != "idle · done" || p.Busy || p.Attention {
		t.Fatalf("projection: %+v", p)
	}
}
