package swarm

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/workflow"
)

func admitParent(t *testing.T, r *Runtime) []messages.ChatMessage {
	t.Helper()
	cb := &llm.AgentCallbacks{}
	r.bindParent(cb, nil)
	input, err := cb.AdmitInput(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := cb.Checkpoint(context.Background(), llm.AgentCheckpoint{Generated: input}); err != nil {
		t.Fatal(err)
	}
	return input
}

func TestResearchCompletesOnlyOnDurableMailDelivery(t *testing.T) {
	ctx := context.Background()
	r := runtimeTest(t, nilModel(), 1, 5)
	result, err := r.Agent(ctx, "", AgentRequest{Task: "inspect", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := r.read(ctx)
	task := s.Tasks[result.Task]
	if !deliveringTask(s, task) || task.Delivery != nil || task.Revision != result.Revision || result.Execution != task.Execution {
		t.Fatalf("completion identity: task=%+v result=%+v", task, result)
	}
	p := MemberState(s, s.Members[result.Session])
	if !p.Delivering || p.Busy || p.Attention {
		t.Fatalf("presentation: %+v", p)
	}
	if err := r.Settle(ctx); err == nil || !strings.Contains(err.Error(), "awaits delivery") {
		t.Fatalf("settlement: %v", err)
	}
	cb := &llm.AgentCallbacks{}
	r.bindParent(cb, nil)
	input, err := cb.AdmitInput(ctx)
	if err != nil || len(input) != 1 || !strings.Contains(input[0].Content, "unused") {
		t.Fatalf("admission: %+v %v", input, err)
	}
	s, _ = r.read(ctx)
	if s.Tasks[task.ID].Status == "done" {
		t.Fatal("reading staged input counted as durable delivery")
	}
	if err := cb.Checkpoint(ctx, llm.AgentCheckpoint{Generated: input}); err != nil {
		t.Fatal(err)
	}
	s, _ = r.read(ctx)
	task = s.Tasks[task.ID]
	if task.Status != "done" || task.Delivery == nil || task.Delivery.Via != "mail" || task.AcceptedRevision != 0 {
		t.Fatalf("delivery: %+v", task)
	}
	if err := r.Review(ctx, task.ID, task.Revision, true, ""); err == nil || !strings.Contains(err.Error(), "completes on delivery") {
		t.Fatalf("review: %v", err)
	}
}

func TestWorkflowStepDeliversBeforeNextOperation(t *testing.T) {
	ctx := context.Background()
	r := runtimeTest(t, nilModel(), 1, 4)
	report, err := r.RunWorkflow(ctx, `polly.defineWorkflow({name:"receipt",inputSchema:polly.schema.object({}),async run(){const a=await polly.agent({task:"inspect",readOnly:true});const t=await polly.tasks.read(a.task);if(t.status!=="done"||t.delivery.via!=="workflow_step")throw Error("not delivered");return a;}})`, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := r.read(ctx)
	if s.Workflows[report.ID].Acknowledged {
		t.Fatal("workflow output acknowledged before parent delivery")
	}
	input := admitParent(t, r)
	if len(input) != 1 || !strings.Contains(input[0].Content, "unused") {
		t.Fatalf("workflow output: %+v", input)
	}
	s, _ = r.read(ctx)
	if !s.Workflows[report.ID].Acknowledged {
		t.Fatal("workflow delivery not recorded")
	}
}

func TestWorkflowReceiptRefusesStaleOrUnrelatedIdentity(t *testing.T) {
	for _, kind := range []string{"agent", "followup"} {
		for _, corrupt := range []string{"none", "task", "member", "execution", "revision", "workflow", "call", "saved"} {
			t.Run(kind+"/"+corrupt, func(t *testing.T) {
				result := AgentResult{Task: "task", Session: "member", Execution: "execution", Revision: 2}
				saved := result
				s := &State{Tasks: map[string]*Task{"task": {ID: "task", Owner: "member", Execution: "execution", Revision: 2, Requirement: RequirementDelivered, Status: "running"}}, Executions: map[string]*Execution{"execution": {ID: "execution", Member: "member", Status: "completed", Workflow: "w", Request: AgentRequest{CallID: "w/1"}, Result: &saved}}}
				switch corrupt {
				case "task":
					result.Task = "wrong"
				case "member":
					result.Session = "wrong"
				case "execution":
					result.Execution = "wrong"
				case "revision":
					result.Revision++
				case "workflow":
					s.Executions["execution"].Workflow = "other"
				case "call":
					s.Executions["execution"].Request.CallID = "other"
				case "saved":
					saved.Revision++
				}
				data, _ := json.Marshal(result)
				var value map[string]any
				json.Unmarshal(data, &value)
				w := &workflow.Report{ID: "w", Steps: []workflow.Step{{Operation: workflow.Operation{ID: "w/1", Kind: kind}, Status: "completed", Value: value}}}
				recordWorkflowDeliveries(s, w)
				if got := s.Tasks["task"].Status == "done"; got != (corrupt == "none") {
					t.Fatalf("delivery=%v corrupt=%s", got, corrupt)
				}
			})
		}
	}
}

func TestDeliveryAdmissionBoundsAndRecoversLostNotice(t *testing.T) {
	ctx := context.Background()
	r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
		return answer(strings.Repeat("r", 20000))
	}), 1, 20)
	var result AgentResult
	for n := 0; n < 18; n++ {
		var err error
		result, err = r.Agent(ctx, "", AgentRequest{Task: "inspect", ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
	}
	// Simulate the lost step/notice window on one completed execution.
	if err := r.update(ctx, func(s *State) error {
		for id, m := range s.Messages {
			if m.Task == result.Task {
				delete(s.Messages, id)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.Settle(ctx); err == nil {
		t.Fatal("pending results settled")
	}
	s, _ := r.read(ctx)
	if resultNotice(s, s.Tasks[result.Task]) == nil {
		t.Fatal("notice not repaired")
	}
	first := admitParent(t, r)
	if len(first) != 1 || len(first[0].Content) > admissionBytes || !strings.Contains(first[0].Content, "full text: swarm_tasks") {
		t.Fatalf("bounded preview: %+v", first)
	}
	s, _ = r.read(ctx)
	if len(inbox(s, r.ID, true)) != 2 {
		t.Fatalf("remaining notices: %d", len(inbox(s, r.ID, true)))
	}
	admitParent(t, r)
	s, _ = r.read(ctx)
	for _, task := range s.Tasks {
		if task.Status != "done" || task.Delivery.Inline {
			t.Fatalf("receipt: %+v", task)
		}
	}
}

func TestContinuationPreservesUndeliveredRevision(t *testing.T) {
	ctx := context.Background()
	r := runtimeTest(t, nilModel(), 1, 4)
	first, err := r.Agent(ctx, "", AgentRequest{Task: "first", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.Agent(ctx, "", AgentRequest{Session: first.Session, Task: "second"})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := r.read(ctx)
	if first.Task == second.Task || s.Tasks[second.Task].Follows != first.Task || s.Tasks[first.Task].Revision != first.Revision || s.Tasks[first.Task].Result != first.Value {
		t.Fatal("continuation overwrote an undelivered result")
	}
	if err := r.UpdateTask(ctx, first.Task, first.Revision, "", nil); err == nil {
		t.Fatal("reassignment invalidated a pending notice")
	}
	if err := r.BlockTask(ctx, first.Session, first.Task, first.Revision, "later"); err == nil {
		t.Fatal("member invalidated a pending notice")
	}
	admitParent(t, r)
	s, _ = r.read(ctx)
	if s.Tasks[first.Task].Status != "done" || s.Tasks[second.Task].Status != "done" {
		t.Fatal("not every preserved result was delivered")
	}
}

func TestClaimDoesNotInheritCompletedExecutionReceipt(t *testing.T) {
	ctx := context.Background()
	r := runtimeTest(t, nilModel(), 1, 3)
	first, err := r.Agent(ctx, "", AgentRequest{Task: "first", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	task, err := r.CreateTask(ctx, "second", "", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Claim(ctx, first.Session, task.ID, task.Revision); err != nil {
		t.Fatal(err)
	}
	s, _ := r.read(ctx)
	if deliveringTask(s, s.Tasks[task.ID]) {
		t.Fatal("claim inherited an earlier task's completed result")
	}
	if !deliveringTask(s, s.Tasks[first.Task]) {
		t.Fatal("claim invalidated the previous result")
	}
}

func TestDeliveryCheckpointFailurePreservesDependencyAndReceipt(t *testing.T) {
	ctx := context.Background()
	r := runtimeTest(t, doneModel(), 1, 3)
	suspendAutoRelease(t, r)
	a, err := r.Agent(ctx, "", AgentRequest{Task: "inspect", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	child, err := r.CreateTask(ctx, "dependent", "", []string{a.Task}, "")
	if err != nil {
		t.Fatal(err)
	}
	original := r.parent
	failed := &failCoordination{CoordinationSession: original, remaining: 1}
	r.parent = failed
	cb := &llm.AgentCallbacks{}
	r.bindParent(cb, nil)
	input, err := cb.AdmitInput(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := cb.Checkpoint(ctx, llm.AgentCheckpoint{Generated: input}); err == nil {
		t.Fatal("injected checkpoint succeeded")
	}
	s, _ := r.read(ctx)
	if s.Tasks[a.Task].Delivery != nil || depsDone(s, s.Tasks[child.ID]) || resultNotice(s, s.Tasks[a.Task]).Delivered {
		t.Fatal("failed save admitted result")
	}
	if err := cb.Checkpoint(ctx, llm.AgentCheckpoint{Generated: input}); err != nil {
		t.Fatal(err)
	}
	s, _ = r.read(ctx)
	if s.Tasks[a.Task].Delivery == nil || !depsDone(s, s.Tasks[child.ID]) {
		t.Fatal("committed delivery did not unlock dependency")
	}
}

func TestCanceledWorkflowRepairsSuppressedCompletionNotice(t *testing.T) {
	for _, status := range []string{"canceled", "failed", "interrupted"} {
		t.Run(status, func(t *testing.T) {
			ctx := context.Background()
			r := runtimeTest(t, doneModel(), 1, 2)
			if err := r.SaveWorkflow(ctx, workflow.Report{ID: "workflow", Status: "running"}); err != nil {
				t.Fatal(err)
			}
			a, err := r.Agent(ctx, "workflow", AgentRequest{Task: "inspect", ReadOnly: true, CallID: "workflow/1"})
			if err != nil {
				t.Fatal(err)
			}
			s, _ := r.read(ctx)
			if resultNotice(s, s.Tasks[a.Task]) != nil {
				t.Fatal("running script posted parent notice")
			}
			if err := r.SaveWorkflow(ctx, workflow.Report{ID: "workflow", Status: status}); err != nil {
				t.Fatal(err)
			}
			s, _ = r.read(ctx)
			if resultNotice(s, s.Tasks[a.Task]) == nil {
				t.Fatal("terminal workflow stranded a result before step save")
			}
			admitParent(t, r)
			s, _ = r.read(ctx)
			if s.Tasks[a.Task].Status != "done" || s.Tasks[a.Task].Delivery.Via != "mail" {
				t.Fatal("recovered notice did not deliver")
			}
		})
	}
}

func TestOversizedMailCannotBlockResults(t *testing.T) {
	ctx := context.Background()
	r := runtimeTest(t, doneModel(), 1, 2)
	huge, err := r.Send(ctx, r.ID, r.ID, "info", "", strings.Repeat("x", 100000))
	if err != nil {
		t.Fatal(err)
	}
	a, err := r.Agent(ctx, "", AgentRequest{Task: "inspect", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	input := admitParent(t, r)
	if len(input) != 1 || len(input[0].Content) > admissionBytes || !strings.Contains(input[0].Content, "read_messages({message:") {
		t.Fatalf("unbounded message: %+v", input)
	}
	s, _ := r.read(ctx)
	if !s.Messages[huge.ID].Delivered || s.Tasks[a.Task].Status != "done" || len(s.Messages[huge.ID].Text) != 100000 {
		t.Fatal("oversized mail blocked or discarded results")
	}
}

func TestInlineDeliveryStopsAtByteCap(t *testing.T) {
	ctx := context.Background()
	r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
		return answer(strings.Repeat("x", 16000))
	}), 1, 5)
	for range 5 {
		if _, err := r.Agent(ctx, "", AgentRequest{Task: "inspect", ReadOnly: true}); err != nil {
			t.Fatal(err)
		}
	}
	input := admitParent(t, r)
	if len(input) != 1 || len(input[0].Content) > admissionBytes {
		t.Fatal("admission exceeded byte cap")
	}
	s, _ := r.read(ctx)
	if got := len(inbox(s, r.ID, true)); got != 1 {
		t.Fatalf("pending=%d; four inline results should fit", got)
	}
	admitParent(t, r)
	s, _ = r.read(ctx)
	if len(inbox(s, r.ID, true)) != 0 {
		t.Fatal("second boundary stranded result")
	}
}

func TestLateResultRepromptsExactlyOnce(t *testing.T) {
	ctx := context.Background()
	r := runtimeTest(t, doneModel(), 1, 2)
	// The child finishes after the parent has constructed its initial answer.
	if _, err := r.Agent(ctx, "", AgentRequest{Task: "late result", ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	cb := &llm.AgentCallbacks{}
	r.bindParent(cb, nil)
	nudge, err := cb.ContinueAfterFinal(ctx, nil)
	if err != nil || len(nudge) != 1 {
		t.Fatalf("first final: %d %v", len(nudge), err)
	}
	input, err := cb.AdmitInput(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := cb.Checkpoint(ctx, llm.AgentCheckpoint{Generated: append(nudge, input...)}); err != nil {
		t.Fatal(err)
	}
	again, err := cb.ContinueAfterFinal(ctx, nil)
	if err != nil || len(again) != 0 {
		t.Fatalf("delivered result reprompted again: %d %v", len(again), err)
	}
}

func TestWorkflowSuccessiveFollowupsAndDependentNeedNoParentRoundTrip(t *testing.T) {
	ctx := context.Background()
	r := runtimeTest(t, doneModel(), 2, 4)
	first, err := r.CreateTask(ctx, "initial research", "", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	dependent, err := r.CreateTask(ctx, "dependent", "", []string{first.ID}, "")
	if err != nil {
		t.Fatal(err)
	}
	source := `polly.defineWorkflow({name:"followups and dependent",inputSchema:polly.schema.object({first:polly.schema.string(),dependent:polly.schema.string()}),async run(input){const a=await polly.agent({task:"inspect",taskID:input.first,readOnly:true});const b=await polly.followup({task:a.task,question:"explain"});await polly.release(b.context);const c=await polly.followup({task:b.task,question:"more detail"});if(a.session!==c.session||c.context===b.context)throw Error("lost member or workspace recreation");return await polly.agent({task:"dependent",taskID:input.dependent,readOnly:true})}})`
	report, err := r.RunWorkflow(ctx, source, map[string]any{"first": first.ID, "dependent": dependent.ID})
	if err != nil {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	s, _ := r.read(ctx)
	for _, task := range s.Tasks {
		if task.Status != "done" || task.Delivery == nil || task.Delivery.Via != "workflow_step" {
			t.Fatalf("script work not delivered: %+v", task)
		}
	}
	if len(inbox(s, r.ID, true)) != 1 {
		t.Fatal("script required intermediate parent notices")
	}
	if s.Workflows[report.ID].Acknowledged {
		t.Fatal("fixture admitted parent output")
	}
}
