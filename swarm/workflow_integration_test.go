package swarm

import (
	"context"
	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/alexschlessinger/pollytool/workflow"
)

func TestWorkflowReleaseDuringAttemptAndRetainsDirtyCopies(t *testing.T) {
	r, p := applyFixture(t, false)
	ctx := context.Background()
	h := &workflowHost{runtime: r, controller: "workflow"}
	defer h.close()
	if err := r.pinIntegrationSnapshot(ctx, p.Parent); err != nil {
		t.Fatal(err)
	}
	makeCopy := func() string {
		t.Helper()
		result, err := h.Call(ctx, workflow.Operation{Kind: "context", Args: map[string]any{"snapshot": p.Parent.ID}})
		if err != nil {
			t.Fatal(err)
		}
		return result.(string)
	}
	clean, dirty, busy := makeCopy(), makeCopy(), makeCopy()
	s, _ := r.read(ctx)
	if err := os.WriteFile(filepath.Join(s.Contexts[dirty].Root, "a.txt"), []byte("dirty\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	r.workflowCancels[h.controller] = func() {}
	r.mu.Unlock()
	defer func() { r.mu.Lock(); delete(r.workflowCancels, h.controller); r.mu.Unlock() }()
	unlock := r.lockContext(busy)
	_, err := h.release(ctx, busy)
	candidateError(t, err, "context_busy")
	unlock()
	_, err = h.release(ctx, dirty)
	candidateError(t, err, "unintegrated_changes")
	if err := r.Cleanup(ctx, clean); err == nil {
		t.Fatal("general cleanup ignored the active workflow")
	}
	if _, err = h.release(ctx, clean); err != nil {
		t.Fatal(err)
	}
	after, _ := r.read(ctx)
	if after.Contexts[clean] != nil || after.Contexts[dirty] == nil || after.Snapshots[p.Parent.ID] == nil {
		t.Fatal("release deleted dirty context or pinned snapshot")
	}
	if _, err = os.Stat(s.Contexts[clean].Root); !os.IsNotExist(err) {
		t.Fatal("clean checkout retained", err)
	}
	if _, err = h.Call(ctx, workflow.Operation{Kind: "tool", Args: map[string]any{"context": clean, "name": "write_file"}}); err == nil {
		t.Fatal("released context reused")
	}
	other := &workflowHost{runtime: r, controller: "other"}
	defer other.close()
	_, err = other.release(ctx, dirty)
	candidateError(t, err, "unknown_context")
}

func TestContextCleanupRetainsIntegrationProvenance(t *testing.T) {
	for _, via := range []string{"direct", "workflow"} {
		t.Run(via, func(t *testing.T) {
			r, p := applyFixture(t, false)
			ctx := context.Background()
			ref := submittedInput(t, r, p.Parent, map[string]string{})
			cleanup := contextCleanupCaller(t, r, via, ref.Task)
			if via == "workflow" {
				if err := r.Review(ctx, ref.Task, ref.Revision, true, ""); err != nil {
					t.Fatal(err)
				}
			}
			if err := cleanup(ctx, ref.Task); err != nil {
				t.Fatal(err)
			}
			c, err := r.PrepareIntegration(ctx, []TaskReference{ref}, "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = r.AcceptIntegration(ctx, c.ID); err != nil {
				t.Fatal(err)
			}
			if _, err = r.ApplyIntegration(ctx, c.ID); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestParentWorkflowIntegrationAndTaskAuthority(t *testing.T) {
	r, p := applyFixture(t, false)
	ctx := context.Background()
	ref := submittedInput(t, r, p.Parent, map[string]string{"a.txt": "workflow\n"})
	script := `polly.defineWorkflow({name:"integrate",inputSchema:polly.schema.object({task:polly.schema.string()}),async run(input){
 const task=await polly.tasks.read(input.task);
 const candidate=await polly.integration.prepare({tasks:[{task:task.id,revision:task.revision}]});
 const copy=await polly.context({snapshot:candidate.merged.id});
 await polly.release(copy);
 await polly.integration.accept(candidate.id);
 return await polly.integration.apply(candidate.id);
}});`
	report, err := r.RunWorkflow(ctx, script, map[string]any{"task": ref.Task})
	if err != nil {
		t.Fatal(err)
	}
	if report.Output.(map[string]any)["status"] != "applied" {
		t.Fatal(report)
	}
	for _, body := range []string{
		`await polly.integration.prepare({tasks:[],actor:"parent"})`,
		`await polly.tasks.review({task:"x",revision:1,accept:true,identity:"parent"})`,
		`await polly.release({context:"x",identity:"parent"})`,
	} {
		_, err := r.RunWorkflow(ctx, `polly.defineWorkflow({name:"forged",inputSchema:polly.schema.object({}),async run(){`+body+`}})`, map[string]any{})
		if err == nil {
			t.Fatal("forgery accepted", body)
		}
	}
}

func TestWorkflowSerializationCannotApplyIntegration(t *testing.T) {
	r, plan := applyFixture(t, false)
	ctx := context.Background()
	source := `polly.defineWorkflow({name:"serialize-apply",inputSchema:polly.schema.object({id:polly.schema.string()}),async run(input){
 return {toJSON(){polly.integration.apply(input.id); return {ok:true};}};
}});`
	report, err := r.RunWorkflow(ctx, source, map[string]any{"id": plan.ID})
	if err == nil || report.Status != "failed" || len(report.Steps) != 0 {
		t.Fatalf("serialization apply was not refused: %v %+v", err, report)
	}
	data, err := os.ReadFile(filepath.Join(r.config.Root, "a.txt"))
	if err != nil || string(data) != "base\n" {
		t.Fatalf("serialization changed parent files: %q %v", data, err)
	}
	state, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state.Applies[plan.ID] != nil || state.Tasks["task"].Status != "awaiting_review" {
		t.Fatal("serialization started an apply or completed its task")
	}
}

func TestWorkflowRequestedChangesRequireExplicitContinuation(t *testing.T) {
	var calls atomic.Int32
	r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
		calls.Add(1)
		return answer("reviewed")
	}), 1, 4)
	script := `polly.defineWorkflow({name:"continue",inputSchema:polly.schema.object({}),async run(){
 const first=await polly.agent({task:"inspect",readOnly:true,review:true,tools:[]});
 const task=await polly.tasks.read(first.task);
 await polly.tasks.review({task:task.id,revision:task.revision,accept:false,feedback:"check again"});
 const second=await polly.agent({session:first.session,task:"check again"});
 const revised=await polly.tasks.read(second.task);
 await polly.tasks.review({task:revised.id,revision:revised.revision,accept:true});
 return second;
 }});`
	if _, err := r.RunWorkflow(context.Background(), script, map[string]any{}); err != nil {
		t.Fatal(err)
	}
	state, err := r.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || len(state.Executions) != 2 || len(state.Members) != 1 {
		t.Fatalf("request changes auto-restarted a reservation: calls=%d executions=%d members=%d", calls.Load(), len(state.Executions), len(state.Members))
	}
}
