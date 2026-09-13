package swarm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestWorkflowReconciliationExampleNeverReplaysPatch(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "examples", "workflows", "reconcile-integration.js"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(source)) != workflowHelpExamples(t)[3] {
		t.Fatal("bundled reconciliation differs from the runnable file")
	}
	t.Chdir(t.TempDir())
	for _, tc := range []struct{ contents, status string }{
		{"base\n", "not_applied"}, {"candidate\n", "applied"}, {"later unrelated edit\n", "recovery_required"},
	} {
		t.Run(tc.status, func(t *testing.T) {
			r, plan := applyFixture(t, false)
			ctx := context.Background()
			path := filepath.Join(r.config.Root, "a.txt")
			if err := os.WriteFile(path, []byte(tc.contents), 0600); err != nil {
				t.Fatal(err)
			}
			if err := r.update(ctx, func(s *State) error {
				s.Applies[plan.ID] = &ApplyRecord{ID: plan.ID, Plan: plan, Tasks: []TaskReference{{Task: "task", Revision: 1}}, Status: "applying"}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			report, err := r.RunWorkflow(ctx, string(source), map[string]any{"id": plan.ID})
			if err != nil {
				t.Fatal(err)
			}
			receipt := report.Output.(map[string]any)["receipt"].(map[string]any)
			if receipt["status"] != tc.status {
				t.Fatalf("wrong observed outcome: %+v", receipt)
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != tc.contents {
				t.Fatalf("reconciliation wrote a patch: %q, %v", data, err)
			}
			s, _ := r.State(ctx)
			if len(s.Applies) != 1 || (s.Tasks["task"].Status == "done") != (tc.status == "applied") {
				t.Fatal("reconciliation lost receipt or completed unapplied work")
			}
			for _, step := range report.Steps {
				if step.Kind != "integration" || step.Args["op"] != "reconcile" && step.Args["op"] != "read" {
					t.Fatalf("unexpected recovery effect: %+v", step)
				}
			}
		})
	}
}

func TestWorkflowConflictRepairAndStepwiseApply(t *testing.T) {
	r, base, _ := integrateFixture(t)
	ctx := context.Background()
	a := submittedInput(t, r, base, map[string]string{"a.txt": "first\n"})
	b := submittedInput(t, r, base, map[string]string{"a.txt": "second\n"})
	r.config.Client = modelFunc(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		for _, m := range req.Messages {
			if m.ToolName == "write_file" {
				return answer("Resolved a.txt and checked both contributions")
			}
		}
		return iterationTool("repair", "write_file", `{"path":"a.txt","content":"resolved\n"}`)
	})
	if _, err := r.config.Registry.LoadToolAuto("write_file"); err != nil {
		t.Fatal(err)
	}
	source := `polly.defineWorkflow({name:"repair",inputSchema:polly.schema.object({tasks:polly.schema.array(polly.schema.object({task:polly.schema.string(),revision:polly.schema.integer()}))}),async run(input){
 const candidate=await polly.integration.prepare(input);
 if(!(candidate.conflicts||[]).length) polly.fail("expected conflict");
 const repair=await polly.agent({commit:candidate.merged.commit,label:"Resolve conflict",task:"Resolve a.txt and verify both contributions"});
 const task=await polly.tasks.read(repair.task);
 const successor=await polly.integration.revise(candidate.id,{task:task.id,revision:task.revision});
 if((successor.conflicts||[]).length) polly.fail("unresolved conflict");
 await polly.integration.accept(successor.id);
 return await polly.integration.apply(successor.id);
 }});`
	report, err := r.RunWorkflow(ctx, source, map[string]any{"tasks": []TaskReference{a, b}})
	if err != nil {
		t.Fatal(err)
	}
	if report.Output.(map[string]any)["status"] != "applied" {
		t.Fatal(report.Output)
	}
	data, err := os.ReadFile(filepath.Join(r.config.Root, "a.txt"))
	if err != nil || string(data) != "resolved\n" {
		t.Fatalf("repair did not apply: %q, %v", data, err)
	}
	s, _ := r.State(ctx)
	for _, task := range s.Tasks {
		if task.Status != "done" {
			t.Fatalf("repair lost contributing obligation: %+v", task)
		}
	}
}

func TestWorkflowParentDriftRefresh(t *testing.T) {
	r, base, _ := integrateFixture(t)
	ctx := context.Background()
	ref := submittedInput(t, r, base, map[string]string{"a.txt": "candidate\n"})
	prepare := `polly.defineWorkflow({name:"prepare",inputSchema:polly.schema.object({task:polly.schema.string(),revision:polly.schema.integer()}),async run(input){return await polly.integration.prepare({tasks:[input],drift:"tree"});}});`
	prepared, err := r.RunWorkflow(ctx, prepare, map[string]any{"task": ref.Task, "revision": ref.Revision})
	if err != nil {
		t.Fatal(err)
	}
	id := prepared.Output.(map[string]any)["id"]
	if err := os.WriteFile(filepath.Join(r.config.Root, "parent.txt"), []byte("parent drift"), 0600); err != nil {
		t.Fatal(err)
	}
	repair := `polly.defineWorkflow({name:"refresh",inputSchema:polly.schema.object({id:polly.schema.string()}),async run({id}){
 try {await polly.integrate({candidate:id});polly.fail("accepted stale parent");}
 catch(e) {if(e.code!=="parent_changed") throw e;}
 const refreshed=await polly.integration.refresh(id);
 if(!refreshed.changed) polly.fail("expected changed candidate");
 const check=await polly.agent({commit:refreshed.merged.commit,label:"Validate refresh",task:"Verify the refreshed candidate",readOnly:true});
 return {check,result:await polly.integrate({candidate:refreshed.id})};
 }});`
	report, err := r.RunWorkflow(ctx, repair, map[string]any{"id": id})
	if err != nil || report.Output.(map[string]any)["result"].(map[string]any)["status"] != "applied" {
		t.Fatalf("drift refresh: %+v, %v", report, err)
	}
	for file, want := range map[string]string{"a.txt": "candidate\n", "parent.txt": "parent drift"} {
		data, err := os.ReadFile(filepath.Join(r.config.Root, file))
		if err != nil || string(data) != want {
			t.Fatalf("refresh lost %s: %q, %v", file, data, err)
		}
	}
}
