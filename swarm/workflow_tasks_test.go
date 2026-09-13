package swarm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

func TestWorkflowTaskDependenciesAndReassignmentExample(t *testing.T) {
	r := runtimeTest(t, doneModel(), 1, 6)
	source, err := os.ReadFile(filepath.Join("..", "examples", "workflows", "task-dependencies.js"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(source)) != workflowHelpExamples(t)[2] {
		t.Fatal("bundled task example differs from the runnable file")
	}
	t.Chdir(t.TempDir())
	r.RegisterParentTools(r.config.Registry)
	ctx := context.Background()
	out, err := execParentTool(t, r, "workflow_run", map[string]any{
		"source": string(source), "input": `{"question":"Inspect the cache","feedback":"Check the failure paths independently"}`,
	})
	if err != nil || !strings.Contains(out, `"status": "completed"`) {
		t.Fatalf("example = %s, %v", out, err)
	}
	s, _ := r.State(ctx)
	if len(s.Tasks) != 2 || len(s.Members) != 3 || len(s.Executions) != 3 {
		t.Fatalf("taskID duplicated work: tasks=%d members=%d executions=%d", len(s.Tasks), len(s.Members), len(s.Executions))
	}
	for _, task := range s.Tasks {
		if task.Status != "done" || task.Result == nil {
			t.Fatalf("incomplete task: %+v", task)
		}
		if task.Requirement == RequirementReviewed && (task.AcceptedRevision != task.Revision || len(task.Dependencies) != 1 || task.Revision < 5) {
			t.Fatalf("reassignment lost review or dependency: %+v", task)
		}
	}
}

func TestWorkflowTaskValidation(t *testing.T) {
	r := runtimeTest(t, doneModel(), 1, 8)
	source := `polly.defineWorkflow({name:"validate tasks",inputSchema:polly.schema.object({}),async run(){
 const a=await polly.tasks.create({description:"prerequisite"});
 const b=await polly.tasks.create({description:"dependent",dependencies:[a.id]});
 const errors=[];
 async function rejected(fn) {try {await fn(); throw new Error("unexpected success");} catch(e) {if(e.message==="unexpected success") throw e; errors.push(e.message);}}
 await rejected(()=>polly.agent({taskID:b.id,label:"Too early",task:"dependent",readOnly:true}));
 await rejected(()=>polly.tasks.update({task:a.id,revision:a.revision+1,owner:"",dependencies:[]}));
 await rejected(()=>polly.tasks.update({task:a.id,revision:a.revision,owner:"",dependencies:[b.id]}));
 await rejected(()=>polly.tasks.update({task:a.id,revision:a.revision,owner:""}));
 await rejected(()=>polly.tasks.update({task:a.id,revision:a.revision,owner:"",dependencies:[],requirement:"applied"}));
 await rejected(()=>polly.tasks.create({description:"forged",actor:"parent"}));
 const editor=await polly.tasks.create({description:"edit",requirement:"applied"});
 const researcher=await polly.agent({label:"Researcher",task:"research",readOnly:true});
 await rejected(()=>polly.tasks.update({task:editor.id,revision:editor.revision,owner:researcher.session,dependencies:[]}));
 return {errors,a:await polly.tasks.read(a.id),b:await polly.tasks.read(b.id),editor:await polly.tasks.read(editor.id)};
 }});`
	report, err := r.RunWorkflow(context.Background(), source, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	out := report.Output.(map[string]any)
	if len(out["errors"].([]any)) != 7 {
		t.Fatal(out)
	}
	for _, key := range []string{"a", "b", "editor"} {
		task := out[key].(map[string]any)
		if task["revision"] != float64(1) || task["status"] != "pending" || task["owner"] != nil && task["owner"] != "" {
			t.Fatalf("rejected operation changed %s: %+v", key, task)
		}
	}
	if out["editor"].(map[string]any)["requirement"] != RequirementApplied {
		t.Fatal("reassignment weakened editing obligation")
	}
}

func TestBlockedFinalDoesNotSubmitOrComplete(t *testing.T) {
	for _, requirement := range []string{RequirementDelivered, RequirementReviewed, RequirementApplied} {
		t.Run(requirement, func(t *testing.T) {
			var r *Runtime
			model := modelFunc(func(ctx context.Context, req *llm.CompletionRequest) messages.ChatMessage {
				for _, m := range req.Messages {
					if m.ToolName == "swarm_block" {
						return answer("Blocked: necessary evidence is unavailable")
					}
				}
				s, err := r.State(ctx)
				if err != nil {
					t.Error(err)
					return answer("state unavailable")
				}
				for _, task := range s.Tasks {
					return iterationTool("block", "swarm_block", tools.Result(map[string]any{"task": task.ID, "revision": task.Revision, "reason": "evidence unavailable"}))
				}
				t.Error("missing task")
				return answer("missing task")
			})
			r = scratchRuntime(t, model, requirement == RequirementApplied)
			result, err := r.Agent(context.Background(), "", AgentRequest{Label: "Blocked worker", Task: "Report a blocker", ReadOnly: requirement != RequirementApplied, Review: requirement == RequirementReviewed})
			if err != nil {
				t.Fatal(err)
			}
			s, _ := r.State(context.Background())
			task := s.Tasks[result.Task]
			if task.Status != "blocked" || task.Snapshot != "" || task.Result != nil || task.Delivery != nil || task.AcceptedRevision != 0 {
				t.Fatalf("blocked final completed or submitted work: %+v", task)
			}
		})
	}
}

func TestPlainFinalAutomaticallySubmitsAndRequiresAcceptance(t *testing.T) {
	for _, requirement := range []string{RequirementDelivered, RequirementReviewed, RequirementApplied} {
		t.Run(requirement, func(t *testing.T) {
			model := modelFunc(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
				if requirement == RequirementApplied {
					wrote := false
					for _, m := range req.Messages {
						wrote = wrote || m.ToolName == "write_file"
					}
					if !wrote {
						return iterationTool("edit", "write_file", `{"path":"result.txt","content":"edited evidence"}`)
					}
				}
				return answer("Final evidence")
			})
			r := scratchRuntime(t, model, requirement == RequirementApplied)
			if requirement == RequirementApplied {
				if _, err := r.config.Registry.LoadToolAuto("write_file"); err != nil {
					t.Fatal(err)
				}
			}
			ctx := context.Background()
			result, err := r.Agent(ctx, "", AgentRequest{Label: "Complete assignment", Task: "Produce evidence", ReadOnly: requirement != RequirementApplied, Review: requirement == RequirementReviewed})
			if err != nil {
				t.Fatal(err)
			}
			task, err := r.ReadTask(ctx, result.Task)
			if err != nil || task.Result != "Final evidence" {
				t.Fatalf("final result lost: %+v, %v", task, err)
			}
			if requirement == RequirementDelivered {
				// Direct Go Agent returns before the parent's admission checkpoint.
				admitParent(t, r)
				task, _ = r.ReadTask(ctx, task.ID)
				if task.Status != "done" || task.Delivery == nil {
					t.Fatalf("ordinary research was not delivered: %+v", task)
				}
				return
			}
			if task.Status != "awaiting_review" || task.AcceptedRevision != 0 {
				t.Fatalf("final answer implicitly accepted work: %+v", task)
			}
			if requirement == RequirementReviewed {
				_, err = execParentTool(t, r, "swarm_review", map[string]any{"task": task.ID, "revision": task.Revision, "accept": true})
			} else {
				if task.Snapshot == "" || task.StartingSnapshot == "" {
					t.Fatal("editing final did not capture snapshots")
				}
				state, _ := r.read(ctx)
				notice := resultNotice(state, task)
				commit := snapshotCommit(state, task.Snapshot)
				if notice == nil || commit == "" || !strings.Contains(notice.Text, "Submitted commit "+commit) || strings.Contains(notice.Text, task.Snapshot) {
					t.Fatal("editing completion notice omitted its commit or exposed the capture record ID")
				}
				_, err = execParentTool(t, r, "swarm_integrate", map[string]any{"tasks": []TaskReference{{Task: task.ID, Revision: task.Revision}}})
			}
			if err != nil {
				t.Fatal(err)
			}
			task, _ = r.ReadTask(ctx, task.ID)
			if task.Status != "done" {
				t.Fatalf("explicit acceptance did not complete work: %+v", task)
			}
		})
	}
}
