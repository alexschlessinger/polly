package swarm

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/workflow"
	"github.com/alexschlessinger/pollytool/worktree"
)

func retainAlias(t *testing.T, r *Runtime, snapshot worktree.Snapshot, id string) worktree.Snapshot {
	t.Helper()
	snapshot.ID = id
	if err := r.pinIntegrationSnapshot(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestCommitResolverRetainedIdentityAndValidation(t *testing.T) {
	r, base, _ := integrateFixture(t)
	ctx := context.Background()
	alias := retainAlias(t, r, base, "000-first-capture")
	retainAlias(t, r, base, "zzz-last-capture")
	for range 5 {
		id, err := r.snapshotForCommit(ctx, base.Commit)
		if err != nil || id != alias.ID {
			t.Fatalf("nondeterministic resolution: %s, %v", id, err)
		}
	}
	manager, err := r.manager(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.config.Root, "unregistered.txt"), []byte("unregistered"), 0600); err != nil {
		t.Fatal(err)
	}
	unregistered, err := manager.Capture(ctx, r.config.Root)
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"", base.ID, base.Commit[:12], "HEAD", "HEAD^{commit}", strings.Repeat("f", 40), unregistered.Commit} {
		if _, err := r.snapshotForCommit(ctx, invalid); err == nil {
			t.Errorf("accepted unretained or invalid commit %q", invalid)
		}
	}
	bad := alias
	bad.ID, bad.Tree = "conflicting-capture", strings.Repeat("0", len(base.Tree))
	retainAlias(t, r, bad, bad.ID)
	if _, err := r.snapshotForCommit(ctx, base.Commit); err == nil || !strings.Contains(err.Error(), "disagree") {
		t.Fatalf("inconsistent retained trees: %v", err)
	}
	if err := r.update(ctx, func(s *State) error { delete(s.Snapshots, bad.ID); return nil }); err != nil {
		t.Fatal(err)
	}
	bad.Commit, bad.Tree = base.Tree, base.Tree
	retainAlias(t, r, bad, "tree-is-not-commit")
	if _, err := r.snapshotForCommit(ctx, bad.Commit); err == nil || !strings.Contains(err.Error(), "commit object") {
		t.Fatalf("accepted tree as commit: %v", err)
	}
	bad.Commit, bad.Tree = strings.Repeat("e", len(base.Commit)), base.Tree
	retainAlias(t, r, bad, "missing-object")
	if _, err := r.snapshotForCommit(ctx, bad.Commit); err == nil {
		t.Fatal("accepted missing Git object")
	}
	if err := r.update(ctx, func(s *State) error { clear(s.Snapshots); return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := r.snapshotForCommit(ctx, base.Commit); err == nil {
		t.Fatal("resurrected a forgotten capture from Git")
	}
}

func TestCommitBaselineCompatibilityDoesNotWeakenSubmission(t *testing.T) {
	s, task := unchangedTaskState()
	alias := *s.Snapshots[task.StartingSnapshot]
	alias.ID, alias.Source = "alias", "/other-capture"
	s.Snapshots[alias.ID] = &alias
	s.Contexts["copy"].Checkout.Base = alias
	if err := assignTask(s, task, s.Members[task.Owner], s.Contexts["copy"], "new-execution"); err != nil {
		t.Fatal("equal captured baseline refused:", err)
	}
	if task.StartingSnapshot != alias.ID {
		t.Fatal("assignment lost its actual workspace provenance")
	}
	alias.Commit = "different-commit"
	if sameBaseline(s, "base", alias.ID) {
		t.Fatal("equal trees made different commits compatible")
	}
	alias.Commit, alias.Tree = s.Snapshots["base"].Commit, "different-tree"
	if sameBaseline(s, "base", alias.ID) {
		t.Fatal("mismatched trees accepted")
	}
	delete(s.Snapshots, alias.ID)
	if sameBaseline(s, alias.ID, alias.ID) {
		t.Fatal("missing identical record IDs accepted")
	}

	r, result, ref := noEditResult(t, false)
	ctx := context.Background()
	s, _ = r.read(ctx)
	submitted := *s.Snapshots[s.Tasks[ref.Task].Snapshot]
	submitted.Source = r.config.Root
	foreign := retainAlias(t, r, submitted, "foreign-same-commit")
	if err := r.Submit(ctx, result.Session, ref.Task, ref.Revision, "foreign", foreign.ID); err == nil {
		t.Fatal("equal commit bypassed source ownership")
	}
	if err := r.Submit(ctx, result.Session, ref.Task, ref.Revision-1, "stale", submitted.ID); err == nil {
		t.Fatal("equal commit bypassed task revision")
	}
}

func TestPublicCommitFollowupRetainedAndRestoredWorkspace(t *testing.T) {
	r := scratchRuntime(t, doneModel(), true)
	suspendAutoRelease(t, r)
	ctx := context.Background()
	first, err := r.Agent(ctx, "", AgentRequest{Label: "Inspect", Task: "inspect", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	admitParent(t, r)
	s, _ := r.read(ctx)
	base := *s.Snapshots[s.Tasks[first.Task].StartingSnapshot]
	retainAlias(t, r, base, "000-alias")
	h := &workflowHost{runtime: r}
	defer h.close()
	for _, release := range []bool{false, true} {
		if release {
			if err := r.Cleanup(ctx, first.Context); err != nil {
				t.Fatal(err)
			}
		}
		value, err := h.Call(ctx, workflow.Operation{Kind: "followup", Args: map[string]any{"task": first.Task, "question": "inspect again", "commit": base.Commit}})
		if err != nil {
			t.Fatal(err)
		}
		result := value.(AgentResult)
		admitParent(t, r)
		s, _ = r.read(ctx)
		if result.Session != first.Session || snapshotCommit(s, s.Tasks[result.Task].StartingSnapshot) != base.Commit {
			t.Fatal("follow-up changed worker or baseline")
		}
		if release && result.Context == first.Context {
			t.Fatal("released context was not recreated")
		}
	}
	// Reopen the runtime from its unchanged persisted snapshot records, then
	// resolve the public commit against that historical provenance.
	h.close()
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, err := New(r.config)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	suspendAutoRelease(t, recovered)
	reopened := &workflowHost{runtime: recovered}
	defer reopened.close()
	value, err := reopened.Call(ctx, workflow.Operation{Kind: "followup", Args: map[string]any{"task": first.Task, "question": "inspect after restart", "commit": base.Commit}})
	if err != nil {
		t.Fatal(err)
	}
	s, _ = recovered.read(ctx)
	result := value.(AgentResult)
	if result.Session != first.Session || snapshotCommit(s, s.Tasks[result.Task].StartingSnapshot) != base.Commit {
		t.Fatal("restart lost the historical baseline or worker")
	}
}

func TestPublicCommitUnbornAndNonGitResearch(t *testing.T) {
	for _, git := range []bool{false, true} {
		t.Run(map[bool]string{false: "non-git", true: "unborn"}[git], func(t *testing.T) {
			skipIfWindows(t)
			var prompt string
			r := scratchRuntime(t, modelFunc(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
				for _, message := range req.Messages {
					if message.Role == messages.MessageRoleSystem {
						prompt += message.Content
					}
				}
				return answer("inspected")
			}), false)
			if err := os.WriteFile(filepath.Join(r.config.Root, "untracked.txt"), []byte("untracked contents\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if git {
				cmd := exec.Command("git", "init", "-q")
				cmd.Dir = r.config.Root
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git init: %s %v", out, err)
				}
				if err := r.Close(); err != nil {
					t.Fatal(err)
				}
				var err error
				r, err = New(r.config)
				if err != nil {
					t.Fatal(err)
				}
				defer r.Close()
			}
			suspendAutoRelease(t, r)
			ctx := context.Background()
			h := &workflowHost{runtime: r}
			defer h.close()
			value, err := h.Call(ctx, workflow.Operation{Kind: "agent", Args: map[string]any{"label": "Inspect files", "task": "inspect", "readOnly": true}})
			if err != nil {
				t.Fatal(err)
			}
			result := value.(AgentResult)
			value, err = h.Call(ctx, workflow.Operation{Kind: "task", Args: map[string]any{"op": "read", "task": result.Task}})
			if err != nil {
				t.Fatal(err)
			}
			task := value.(*TaskView)
			if !git {
				if task.BaseCommit != "" || task.ResultCommit != "" || strings.Contains(prompt, "Assigned baseline commit:") {
					t.Fatal("non-Git research invented a capture")
				}
				return
			}
			id, err := r.snapshotForCommit(ctx, task.BaseCommit)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(prompt, "Assigned baseline commit: "+task.BaseCommit) || strings.Contains(prompt, id) {
				t.Fatal("worker guidance omitted the commit or exposed its record ID")
			}
			state, _ := r.read(ctx)
			cmd := exec.Command("git", "show", task.BaseCommit+":untracked.txt")
			cmd.Dir = state.Contexts[result.Context].Root
			if out, err := cmd.CombinedOutput(); err != nil || string(out) != "untracked contents\n" {
				t.Fatalf("captured untracked file: %s %v", out, err)
			}
			cmd = exec.Command("git", "rev-parse", "--verify", "HEAD")
			cmd.Dir = r.config.Root
			if out, err := cmd.CombinedOutput(); err == nil {
				t.Fatalf("capture created a parent HEAD: %s", out)
			}
		})
	}
}

func TestPublicCommitArgumentsRejectLegacyAndContextOverride(t *testing.T) {
	r, base, _ := integrateFixture(t)
	retainAlias(t, r, base, base.ID)
	r.RegisterParentTools(r.config.Registry)
	ctx := context.Background()
	h := &workflowHost{runtime: r}
	defer h.close()
	before, _ := r.read(ctx)
	for _, key := range []string{"snapshot", "Snapshot", "SNAPSHOT"} {
		for _, kind := range []string{"agent", "context", "followup"} {
			_, err := h.Call(ctx, workflow.Operation{Kind: kind, Args: map[string]any{key: base.ID}})
			if err == nil || !strings.Contains(err.Error(), "no longer supported") || !strings.Contains(err.Error(), "commit") {
				t.Fatalf("%s/%s: %v", kind, key, err)
			}
		}
		for _, name := range []string{"spawn_agent", "swarm_publish"} {
			_, err := execParentTool(t, r, name, map[string]any{key: base.ID, "text": "finding", "task_name": "inspect", "message": "inspect"})
			if err == nil || !strings.Contains(err.Error(), "no longer supported") {
				t.Fatalf("%s/%s: %v", name, key, err)
			}
		}
	}
	for _, invalid := range []any{nil, 4, "", "HEAD", base.ID, base.Commit[:12]} {
		_, err := h.Call(ctx, workflow.Operation{Kind: "agent", Args: map[string]any{"commit": invalid, "label": "Invalid", "task": "inspect", "readOnly": true}})
		if err == nil {
			t.Fatalf("accepted %v", invalid)
		}
	}
	after, _ := r.read(ctx)
	if len(after.Tasks) != len(before.Tasks) || len(after.Members) != len(before.Members) || len(after.Contexts) != len(before.Contexts) {
		t.Fatal("invalid selectors allocated work")
	}
	for _, name := range []string{"spawn_agent", "swarm_publish"} {
		tool, _, _ := r.config.Registry.GetIfAllowed(name)
		text := tools.Result(tool.GetSchema())
		if !strings.Contains(text, `"commit"`) || strings.Contains(text, `"snapshot"`) {
			t.Fatal(text)
		}
	}
	r.config.Client = doneModel()
	value, err := h.Call(ctx, workflow.Operation{Kind: "agent", Args: map[string]any{"commit": base.Commit, "label": "Valid", "task": "inspect", "readOnly": true}})
	if err != nil {
		t.Fatal(err)
	}
	member := value.(AgentResult)
	_, err = h.Call(ctx, workflow.Operation{Kind: "agent", Args: map[string]any{"session": member.Session, "commit": base.Commit, "task": "override"}})
	if err == nil || !strings.Contains(err.Error(), "inherits") {
		t.Fatalf("continuation authority: %v", err)
	}
}

func TestPublicCommitConflictRepairWithDuplicateCaptures(t *testing.T) {
	r, base, _ := integrateFixture(t)
	ctx := context.Background()
	a := submittedInput(t, r, base, map[string]string{"a.txt": "first\n"})
	b := submittedInput(t, r, base, map[string]string{"a.txt": "second\n"})
	candidate, err := r.PrepareIntegration(ctx, []TaskReference{a, b}, "tree")
	if err != nil || len(candidate.Conflicts) == 0 {
		t.Fatalf("conflict: %+v %v", candidate, err)
	}
	alias := retainAlias(t, r, candidate.Merged, "000-conflict-alias")
	r.config.Client = modelFunc(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		readOnly := false
		for _, m := range req.Messages {
			readOnly = readOnly || m.Role == messages.MessageRoleSystem && strings.Contains(m.Content, "This context is read-only")
		}
		if readOnly {
			for _, m := range req.Messages {
				if m.ToolName == "bash" {
					return answer("Reviewed both contributions")
				}
			}
			return iterationTool("review", "bash", `{"command":"printf 'first,second\\n' | cmp -s - a.txt"}`)
		}
		for _, m := range req.Messages {
			if m.ToolName == "write_file" {
				return answer("Resolved both contributions")
			}
		}
		return iterationTool("repair", "write_file", `{"path":"a.txt","content":"first,second\n"}`)
	})
	if _, err := r.config.Registry.LoadToolAuto("write_file"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.config.Registry.LoadToolAuto("bash"); err != nil {
		t.Fatal(err)
	}
	source := `polly.defineWorkflow({name:"repair",inputSchema:polly.schema.object({id:polly.schema.string()}),async run({id}){
 const candidate=await polly.integration.read(id);
 const repair=await polly.agent({commit:candidate.merged.commit,label:"Resolve conflict",task:"Preserve both contributions"});
 const task=await polly.tasks.read(repair.task);
 const successor=await polly.integration.revise(id,{task:task.id,revision:task.revision});
 const review=await polly.agent({commit:successor.merged.commit,label:"Review repaired candidate",task:"Check that both contributions are present",readOnly:true});
 if(review.value!=="Reviewed both contributions") polly.fail("review did not complete");
 const context=await polly.context({commit:successor.merged.commit});
 const versions=[candidate.parent,candidate.merged,successor.parent,successor.merged,successor.plan.parent,successor.plan.merged,
   ...candidate.inputs.flatMap(input=>[input.base,input.submitted]),...candidate.conflicts.flatMap(conflict=>[conflict.base,conflict.ours,conflict.theirs])];
 for(const version of versions){
   if(!version.commit||version.id!==undefined) polly.fail("invalid public capture",version);
   await polly.exec("git cat-file -e "+version.commit+"^{commit}",{context});
 }
 await polly.exec("printf 'first,second\\n' | cmp -s - a.txt",{context});
 return {repair:task,result:await polly.integrate({candidate:successor.id})};
 }});`
	report, err := r.RunWorkflow(ctx, source, map[string]any{"id": candidate.ID})
	if err != nil {
		t.Fatal(err)
	}
	output := report.Output.(map[string]any)
	repair := output["repair"].(map[string]any)
	s, _ := r.read(ctx)
	if s.Tasks[repair["id"].(string)].StartingSnapshot != alias.ID {
		t.Fatal("test did not exercise duplicate capture resolution")
	}
	if output["result"].(map[string]any)["status"] != "applied" {
		t.Fatal(output)
	}
	data, err := os.ReadFile(filepath.Join(r.config.Root, "a.txt"))
	if err != nil || string(data) != "first,second\n" {
		t.Fatalf("integration: %q %v", data, err)
	}
}
