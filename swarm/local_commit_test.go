package swarm

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/subagent"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/workflow"
)

func localCommitGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s %v", args, out, err)
	}
	return strings.TrimSpace(string(out))
}

func TestSpawnLocalCommitPreservesSelectedContents(t *testing.T) {
	r := scratchRuntime(t, doneModel(), true)
	suspendAutoRelease(t, r)
	ctx := context.Background()
	root := r.config.Root
	commit := localCommitGit(t, root, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(root, "dirty.txt"), []byte("parent only\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r.RegisterParentTools(r.config.Registry)
	tool, _, _ := r.config.Registry.GetIfAllowed("spawn_agent")
	callCtx := subagent.WithCallID(ctx, "local-commit-spawn")
	args := map[string]any{"task_name": "review_commit", "message": "Review this exact revision", "read_only": true, "review": true, "commit": commit}
	text, err := tool.Execute(callCtx, args)
	if err != nil {
		t.Fatal(err)
	}
	var result struct{ Member string }
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		t.Fatal(err)
	}
	awaitIdle(t, r, ctx)
	s, _ := r.State(ctx)
	c := s.Contexts[s.Members[result.Member].Context]
	if c.Checkout.Base.Commit != commit || localCommitGit(t, c.Root, "rev-parse", "HEAD") != commit {
		t.Fatal("worker did not receive exact local commit")
	}
	if _, err := os.Stat(filepath.Join(c.Root, "dirty.txt")); !os.IsNotExist(err) {
		t.Fatal("worker received dirty parent files")
	}
	for _, task := range s.Tasks {
		if task.Status != "awaiting_review" || task.AcceptedRevision != 0 {
			t.Fatalf("commit admission accepted work: %+v", task)
		}
	}
	if _, err := tool.Execute(callCtx, args); err != nil {
		t.Fatal("matching retry failed:", err)
	}
	after, _ := r.State(ctx)
	if len(after.Snapshots) != len(s.Snapshots) || len(after.Executions) != len(s.Executions) {
		t.Fatal("retry imported or launched again")
	}
}

func TestLocalCommitJavaScriptContextAgentAndFollowup(t *testing.T) {
	r := scratchRuntime(t, doneModel(), true)
	suspendAutoRelease(t, r)
	ctx := context.Background()
	root := r.config.Root
	first := localCommitGit(t, root, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(root, "new.txt"), []byte("new commit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	localCommitGit(t, root, "add", ".")
	localCommitGit(t, root, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "-qm", "new files")
	second := localCommitGit(t, root, "rev-parse", "HEAD")
	source := `polly.defineWorkflow({name:"local-commit",inputSchema:polly.schema.object({first:polly.schema.string(),second:polly.schema.string()}),async run(input){
 const context=await polly.context({commit:input.first,readOnly:true});
 const worker=await polly.agent({context,commit:input.first,label:"Inspect old revision",task:"inspect",readOnly:true});
 await polly.release(worker.context);
 const next=await polly.followup({task:worker.task,question:"Inspect new revision",commit:input.second});
 return {worker,next,task:await polly.tasks.read(next.task)};
 }});`
	report, err := r.RunWorkflow(ctx, source, map[string]any{"first": first, "second": second})
	if err != nil {
		t.Fatalf("workflow: %+v %v", report, err)
	}
	output := report.Output.(map[string]any)
	worker, next := output["worker"].(map[string]any), output["next"].(map[string]any)
	if worker["session"] != next["session"] || output["task"].(map[string]any)["baseCommit"] != second {
		t.Fatalf("followup lost identity/baseline: %#v", output)
	}
	s, _ := r.State(ctx)
	c := s.Contexts[next["context"].(string)]
	if localCommitGit(t, c.Root, "rev-parse", "HEAD^") != first {
		t.Fatal("followup lost commit ancestry")
	}
	if data, _ := os.ReadFile(filepath.Join(c.Root, "new.txt")); string(data) != "new commit\n" {
		t.Fatal("followup lost selected contents")
	}
	// The persisted record survives reopening; no import is needed again.
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(r.config)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.snapshotForCommit(ctx, second); err != nil {
		t.Fatal("retained local commit did not survive restart:", err)
	}
}

func TestLocalCommitAdmissionGuardsAndConcurrency(t *testing.T) {
	r := scratchRuntime(t, doneModel(), true)
	suspendAutoRelease(t, r)
	ctx := context.Background()
	commit := localCommitGit(t, r.config.Root, "rev-parse", "HEAD")
	args := map[string]any{"commit": commit}
	// A publication does not import new baselines or establish ownership.
	if _, err := r.commitArgument(ctx, args); err == nil {
		t.Fatal("publication admitted an unretained commit")
	}
	h := &workflowHost{runtime: r, controller: "different-workflow"}
	defer h.close()
	for _, a := range []map[string]any{
		{"context": "foreign", "commit": commit},
		{"session": "existing", "commit": commit},
		{"snapshot": commit}, {"Snapshot": commit}, {"commit": "HEAD"}, {"commit": commit[:12]}, {"commit": false},
	} {
		if _, err := h.Call(ctx, workflow.Operation{Kind: "agent", Args: a}); err == nil {
			t.Fatalf("accepted invalid or unauthorized request: %v", a)
		}
	}
	s, _ := r.State(ctx)
	if len(s.Snapshots) != 0 {
		t.Fatal("rejected calls admitted commits")
	}
	const n = 6
	var wg sync.WaitGroup
	ids, errs := make([]string, n), make([]error, n)
	for i := range n {
		wg.Go(func() { ids[i], errs[i] = r.baselineCommitArgument(ctx, args, "") })
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil || ids[i] != ids[0] {
			t.Fatalf("concurrent admission %d: %s %v", i, ids[i], errs[i])
		}
	}
	s, _ = r.State(ctx)
	if len(s.Snapshots) != 1 {
		t.Fatal("concurrent calls duplicated retention")
	}
	if err := r.Forget(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.snapshotForCommit(ctx, commit); err == nil {
		t.Fatal("forget retained old record")
	}
	// Explicit selection can admit the still-local commit again, without
	// resurrecting old record identity or old assignments.
	newID, err := r.baselineCommitArgument(ctx, args, "")
	if err != nil || newID == ids[0] {
		t.Fatalf("explicit readmission: %s %v", newID, err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.baselineCommitArgument(ctx, args, ""); err == nil {
		t.Fatal("admitted commit after shutdown")
	}
}

func TestLocalCommitEditingRequiresExplicitIntegration(t *testing.T) {
	model := modelFunc(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		if req.Messages[len(req.Messages)-1].Role != messages.MessageRoleTool {
			return messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: "write", Name: "write_file", Arguments: tools.Result(map[string]any{"path": "edit.txt", "content": "worker edit\n"})}}}
		}
		return answer("finished editing")
	})
	r := scratchRuntime(t, model, true)
	suspendAutoRelease(t, r)
	if _, err := r.config.Registry.LoadToolAuto("write_file"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	commit := localCommitGit(t, r.config.Root, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(r.config.Root, "dirty.txt"), []byte("keep parent edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	h := &workflowHost{runtime: r}
	defer h.close()
	value, err := h.Call(ctx, workflow.Operation{Kind: "agent", Args: map[string]any{"commit": commit, "label": "Edit local commit", "task": "write edit.txt", "readOnly": false}})
	if err != nil {
		t.Fatal(err)
	}
	result := value.(AgentResult)
	s, _ := r.State(ctx)
	task := s.Tasks[result.Task]
	if task.Status != "awaiting_review" || task.AcceptedRevision != 0 || snapshotCommit(s, task.StartingSnapshot) != commit || task.Snapshot == "" {
		t.Fatalf("lost capture or acceptance boundary: %+v", task)
	}
	if _, err := os.Stat(filepath.Join(r.config.Root, "edit.txt")); !os.IsNotExist(err) {
		t.Fatal("worker edit reached parent without integration")
	}
	out, err := r.Integrate(ctx, IntegrateRequest{Tasks: []TaskReference{{Task: task.ID, Revision: task.Revision}}})
	if err != nil || out.Status != "applied" {
		t.Fatalf("integration: %+v %v", out, err)
	}
	for name, want := range map[string]string{"edit.txt": "worker edit\n", "dirty.txt": "keep parent edit\n"} {
		if data, _ := os.ReadFile(filepath.Join(r.config.Root, name)); string(data) != want {
			t.Fatalf("parent %s = %q", name, data)
		}
	}
	if localCommitGit(t, r.config.Root, "rev-parse", "HEAD") != commit {
		t.Fatal("integration changed parent HEAD")
	}
}

func TestLocalCommitStorageFailureDoesNotLaunch(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "rollback", true: "lost-reply"}[committed], func(t *testing.T) {
			r := scratchRuntime(t, doneModel(), true)
			ctx := context.Background()
			commit := localCommitGit(t, r.config.Root, "rev-parse", "HEAD")
			fault := &refreshStoreFault{CoordinationSession: r.parent, match: func(before, after *State) bool { return len(after.Snapshots) > len(before.Snapshots) }}
			if committed {
				fault.after = func() error { return errors.New("lost storage reply") }
			} else {
				fault.before = func(*State) error { return errors.New("storage rollback") }
			}
			r.parent = fault
			args := map[string]any{"task_name": "review", "message": "inspect", "read_only": true, "commit": commit}
			if _, err := execParentTool(t, r, "spawn_agent", args); err == nil {
				t.Fatal("ignored failed storage")
			}
			s, _ := r.State(ctx)
			if len(s.Tasks) != 0 || len(s.Executions) != 0 || len(s.Members) != 0 {
				t.Fatal("failed admission launched work")
			}
			if committed {
				if _, err := r.baselineCommitArgument(ctx, map[string]any{"commit": commit}, ""); err != nil {
					t.Fatal("could not retry:", err)
				}
			} else {
				// Recovery must clean even a pin with no persisted record.
				if err := r.Close(); err != nil {
					t.Fatal(err)
				}
				reopened, err := New(r.config)
				if err != nil {
					t.Fatal(err)
				}
				r = reopened
				defer r.Close()
			}
			if err := r.Forget(ctx); err != nil {
				t.Fatal(err)
			}
			if refs := localCommitGit(t, r.config.Root, "for-each-ref", "refs/polly/snapshots"); refs != "" {
				t.Fatalf("retention leaked refs after forget: %s", refs)
			}
		})
	}
}
