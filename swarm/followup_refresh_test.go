package swarm

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/subagent"
	"github.com/alexschlessinger/pollytool/tools"
)

func followupTool(t *testing.T, r *Runtime, callID string, args map[string]any) (*FollowupView, string, error) {
	t.Helper()
	r.RegisterParentTools(r.config.Registry)
	tool, _, _ := r.config.Registry.GetIfAllowed("followup_task")
	text, err := tool.Execute(subagent.WithCallID(context.Background(), callID), args)
	if err != nil {
		return nil, text, err
	}
	var v FollowupView
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		t.Fatal(err)
	}
	return &v, text, nil
}

func writeRefreshFile(t *testing.T, root, name, value string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
}

func refreshGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s %v", args, out, err)
	}
	return strings.TrimSpace(string(out))
}

// Reproduces bright-finch: a tests-only worker continues its earlier result
// after the parent has added implementation, until refresh is explicit.
func TestRefreshFollowupBrightFinch(t *testing.T) {
	var r *Runtime
	var calls atomic.Int32
	model := modelFunc(func(ctx context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		s, err := r.read(ctx)
		if err != nil {
			t.Error(err)
			return answer("read failed")
		}
		var c *ExecutionContext
		for _, m := range s.Members {
			c = s.Contexts[m.Context]
		}
		n := calls.Add(1)
		if n == 1 {
			writeRefreshFile(t, c.Root, "tests.txt", "implementation must return 42\n")
		}
		_, statErr := os.Stat(filepath.Join(c.Root, "implementation.txt"))
		if n <= 2 && !os.IsNotExist(statErr) || n >= 3 && statErr != nil {
			t.Errorf("call %d saw wrong baseline files: %v", n, statErr)
		}
		if n == 3 {
			found := false
			for _, message := range req.Messages {
				found = found || strings.Contains(message.Content, "supersedes earlier file descriptions") && strings.Contains(message.Content, c.Checkout.Base.Commit)
			}
			if !found {
				t.Error("worker did not receive refresh provenance")
			}
		}
		return answer("checked")
	})
	r = scratchRuntime(t, model, true)
	suspendAutoRelease(t, r)
	ctx := context.Background()
	first, err := r.Agent(ctx, "", AgentRequest{TaskName: "tests", Label: "Tests", Task: "Write tests only", Tools: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := r.read(ctx)
	old := *s.Members[first.Session]
	integrateOK(t, r, IntegrateRequest{Tasks: []TaskReference{{Task: first.Task, Revision: first.Revision}}})
	if _, err := r.releaseWorkspace(ctx, first.Context); err != nil {
		t.Fatal(err)
	}
	s, _ = r.read(ctx)
	prior := *s.Tasks[first.Task]
	writeRefreshFile(t, r.config.Root, "implementation.txt", "42\n")
	writeRefreshFile(t, r.config.Root, "tests.txt", "parent's newer tests\n")
	ordinary, text, err := followupTool(t, r, "ordinary", map[string]any{"target": "tests", "message": "Inspect the implementation without changing files"})
	if err != nil {
		t.Fatal(err)
	}
	awaitIdle(t, r, ctx)
	s, _ = r.read(ctx)
	if ordinary.Operation != "new_task" || ordinary.BaseOrigin != "previous_result" || ordinary.BaseCommit != snapshotCommit(s, prior.Snapshot) || ordinary.Task == first.Task || strings.Contains(text, "Inspect the implementation") {
		t.Fatalf("ordinary provenance: %s", text)
	}
	integrateOK(t, r, IntegrateRequest{Tasks: []TaskReference{{Task: ordinary.Task, Revision: s.Tasks[ordinary.Task].Revision}}})
	// Leave the accepted workspace bound: refresh must release only this one.
	args := map[string]any{"target": "tests", "message": "Inspect current parent implementation", "refresh": true}
	refreshed, text, err := followupTool(t, r, "refresh", args)
	if err != nil {
		t.Fatal(err)
	}
	awaitIdle(t, r, ctx)
	s, _ = r.read(ctx)
	m, task := s.Members[first.Session], s.Tasks[refreshed.Task]
	c := s.Contexts[m.Context]
	if refreshed.Member != first.Session || refreshed.Operation != "new_task" || refreshed.BaseOrigin != "parent" || refreshed.BaseCommit == ordinary.BaseCommit || len(refreshed.BaseCommit) != 40 || refreshed.Execution != m.Execution {
		t.Fatalf("refresh provenance: %s", text)
	}
	if task.Status != "awaiting_review" || task.Follows != ordinary.Task || task.Requirement != prior.Requirement || !reflect.DeepEqual(*s.Tasks[first.Task], prior) {
		t.Fatalf("refresh changed settlement: %+v", task)
	}
	if m.Name != old.Name || m.AgentName != old.AgentName || m.Model != old.Model || m.ModelHost != old.ModelHost || m.ReadOnly != old.ReadOnly || !reflect.DeepEqual(m.Tools, old.Tools) || c.ID == first.Context {
		t.Fatalf("refresh changed authority: %+v", m)
	}
	if got := refreshGit(t, c.Root, "show", refreshed.BaseCommit+":implementation.txt"); got != "42" {
		t.Fatal(got)
	}
	if got, _ := os.ReadFile(filepath.Join(c.Root, "tests.txt")); string(got) != "parent's newer tests\n" {
		t.Fatalf("dirty tracked files omitted: %q", got)
	}
	mail := s.Messages[refreshed.Message]
	if mail.Task != "" || mail.Execution != "" || mail.Revision != 0 || strings.Contains(text, task.StartingSnapshot) || strings.Contains(text, args["message"].(string)) || len(text) > 1000 {
		t.Fatalf("launch metadata leaked or impersonated delivery: %s %+v", text, mail)
	}
	// A later assignment must not change the first call's receipt.
	integrateOK(t, r, IntegrateRequest{Tasks: []TaskReference{{Task: task.ID, Revision: task.Revision}}})
	if _, err := r.releaseWorkspace(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := followupTool(t, r, "later", map[string]any{"target": "tests", "message": "Inspect again"}); err != nil {
		t.Fatal(err)
	}
	awaitIdle(t, r, ctx)
	again, _, err := followupTool(t, r, "refresh", args)
	if err != nil || !reflect.DeepEqual(again, refreshed) || calls.Load() != 4 {
		t.Fatalf("replay followed latest assignment: %+v %v", again, err)
	}
	for _, changed := range []map[string]any{
		{"target": "tests", "message": args["message"], "refresh": false},
		{"target": "tests", "message": "different", "refresh": true},
	} {
		if _, _, err := followupTool(t, r, "refresh", changed); err == nil {
			t.Fatal("reused call ID with changed arguments")
		}
	}
}

func TestRefreshFollowupResearchSources(t *testing.T) {
	for _, mode := range []string{"git-bound", "git-released", "identical", "unborn", "live", "live-parent-became-git"} {
		t.Run(mode, func(t *testing.T) {
			skipIfWindows(t)
			r := scratchRuntime(t, doneModel(), strings.HasPrefix(mode, "git") || mode == "identical")
			suspendAutoRelease(t, r)
			ctx := context.Background()
			if mode == "unborn" {
				refreshGit(t, r.config.Root, "init", "-q")
			}
			source := ""
			if strings.HasPrefix(mode, "live") {
				source = canonicalPath(t, t.TempDir())
			}
			writeRefreshFile(t, r.config.Root, "version", "one")
			first, err := r.Agent(ctx, "", AgentRequest{Label: "Research", Task: "inspect", ReadOnly: true, Source: source, Tools: []string{}})
			if err != nil {
				t.Fatal(err)
			}
			admitParent(t, r)
			before, _ := r.read(ctx)
			prior, old := before.Tasks[first.Task], before.Contexts[first.Context]
			writeRefreshFile(t, old.Scratch, "old-cache", "previous assignment")
			if mode == "git-released" {
				if _, err := r.releaseWorkspace(ctx, first.Context); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "live-parent-became-git" {
				refreshGit(t, r.config.Root, "init", "-q")
			}
			if mode != "identical" {
				writeRefreshFile(t, r.config.Root, "version", "two")
			}
			v, text, err := followupTool(t, r, "refresh", map[string]any{"target": first.Session, "message": "inspect again", "refresh": true})
			if err != nil {
				t.Fatal(err)
			}
			awaitIdle(t, r, ctx)
			s, _ := r.read(ctx)
			c := s.Contexts[s.Members[first.Session].Context]
			if c.ID == old.ID || s.Tasks[v.Task].Follows != prior.ID || !deliveringTask(s, s.Tasks[v.Task]) {
				t.Fatalf("refresh did not replace workspace or prematurely settled: %s", text)
			}
			if _, err := os.Stat(filepath.Join(c.Scratch, "old-cache")); !os.IsNotExist(err) {
				t.Fatal("refresh reused old scratch contents")
			}
			if source != "" {
				if c.Checkout != nil || c.Root != source || v.Source != source || v.BaseOrigin != "live_source" || v.BaseCommit != "" || strings.Contains(text, "baseCommit") {
					t.Fatalf("live source changed: %+v %s", c, text)
				}
			} else {
				if c.Checkout == nil || v.BaseOrigin != "parent" || v.BaseCommit != c.Checkout.Base.Commit {
					t.Fatalf("Git source missing: %s", text)
				}
				want := "two"
				if mode == "identical" {
					want = "one"
					if c.Checkout.Base.Tree != s.Snapshots[prior.StartingSnapshot].Tree || c.Checkout.Base.ID == prior.StartingSnapshot {
						t.Fatal("identical-content capture did not retain distinct record identity")
					}
				}
				if got := refreshGit(t, c.Root, "show", v.BaseCommit+":version"); got != want {
					t.Fatal(got)
				}
			}
			admitParent(t, r)
		})
	}
}

func TestRefreshFollowupArgumentAndHistoricalContract(t *testing.T) {
	r := runtimeTest(t, doneModel(), 1, 4)
	suspendAutoRelease(t, r)
	ctx := context.Background()
	a, err := r.Agent(ctx, "", AgentRequest{Label: "Research", Task: "inspect", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	admitParent(t, r)
	for _, bad := range []any{nil, "true", 0, 1, []any{}, map[string]any{}} {
		if _, _, err := followupTool(t, r, "", map[string]any{"target": a.Session, "message": "inspect", "refresh": bad}); err == nil || !strings.Contains(err.Error(), "boolean") {
			t.Fatalf("accepted refresh %v: %v", bad, err)
		}
	}
	for _, key := range []string{"Refresh", "snapshot", "Snapshot", "SNAPSHOT", "commit", "unknown"} {
		if _, _, err := followupTool(t, r, "", map[string]any{"target": a.Session, "message": "inspect", key: true}); err == nil {
			t.Fatal("accepted", key)
		}
	}
	args := map[string]any{"target": a.Session, "message": "inspect again"}
	v, _, err := followupTool(t, r, "ordinary", args)
	if err != nil {
		t.Fatal(err)
	}
	awaitIdle(t, r, ctx)
	args["refresh"] = false
	again, _, err := followupTool(t, r, "ordinary", args)
	if err != nil || !reflect.DeepEqual(v, again) {
		t.Fatal("omitted refresh is not normalized to false")
	}
	if err := r.update(ctx, func(s *State) error { delete(s.Followups, v.Message); return nil }); err != nil {
		t.Fatal(err)
	}
	historical, text, err := followupTool(t, r, "ordinary", args)
	if err != nil || historical.Member != a.Session || historical.Message != v.Message || historical.Task != "" || historical.Execution != "" || historical.BaseOrigin != "" || strings.Contains(text, "baseCommit") {
		t.Fatalf("invented historical provenance: %s %v", text, err)
	}
}

func TestRefreshFollowupPreservesTypedCompletion(t *testing.T) {
	for _, toolFree := range []bool{false, true} {
		t.Run(map[bool]string{false: "typed-tool", true: "tool-free"}[toolFree], func(t *testing.T) {
			var calls atomic.Int32
			r := runtimeTest(t, modelFunc(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
				calls.Add(1)
				if toolFree {
					if len(req.Tools) != 0 || req.ResponseSchema == nil {
						t.Error("tool-free typed authority changed")
					}
					return answer("true")
				}
				found := false
				for _, tool := range req.Tools {
					found = found || tool.GetName() == completionToolName
				}
				if !found || req.ResponseSchema != nil {
					t.Error("typed completion lost its tool")
					return answer("true")
				}
				return completion("true")
			}), 1, 3)
			suspendAutoRelease(t, r)
			req := AgentRequest{Label: "Typed", Task: "inspect", ReadOnly: true, Review: true, Schema: boolResultSchema}
			if toolFree {
				req.Tools = []string{}
			}
			ctx := context.Background()
			a, err := r.Agent(ctx, "", req)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Review(ctx, a.Task, a.Revision, true, "accepted"); err != nil {
				t.Fatal(err)
			}
			v, _, err := followupTool(t, r, "typed-refresh", map[string]any{"target": a.Session, "message": "inspect again", "refresh": true})
			if err != nil {
				t.Fatal(err)
			}
			awaitIdle(t, r, ctx)
			s, _ := r.read(ctx)
			e := s.Executions[v.Execution]
			if calls.Load() != 2 || e.Completion == nil || e.Completion.Value != true || e.Result.Value != true || s.Tasks[v.Task].Status != "awaiting_review" || s.Tasks[v.Task].AcceptedRevision != 0 {
				t.Fatalf("typed refresh completion/acceptance: %+v", e)
			}
		})
	}
}

func TestFollowupSteerAndResumeProvenance(t *testing.T) {
	entered := make(chan struct{})
	var calls atomic.Int32
	r := scratchRuntime(t, modelFunc(func(ctx context.Context, _ *llm.CompletionRequest) messages.ChatMessage {
		if calls.Add(1) == 1 {
			close(entered)
			<-ctx.Done()
		}
		return answer("done")
	}), true)
	suspendAutoRelease(t, r)
	ctx := context.Background()
	i, err := r.start(ctx, "", AgentRequest{TaskName: "worker", Label: "Worker", Task: "inspect", ReadOnly: true, Review: true})
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	steer, text, err := followupTool(t, r, "steer", map[string]any{"target": "worker", "message": "Consider this extra evidence"})
	if err != nil || steer.Operation != "steer" || steer.Execution != i.id || steer.BaseOrigin != "existing_workspace" || len(steer.BaseCommit) != 40 || !strings.Contains(text, "worker edits beyond its baseline") {
		t.Fatalf("steer provenance: %s %v", text, err)
	}
	if _, err := r.InterruptAgent(ctx, "worker"); err != nil {
		t.Fatal(err)
	}
	resume, text, err := followupTool(t, r, "resume", map[string]any{"target": "worker", "message": "Finish the inspection"})
	if err != nil || resume.Operation != "resume" || resume.Execution != i.id || resume.Task != steer.Task || resume.BaseCommit != steer.BaseCommit {
		t.Fatalf("resume provenance: %s %v", text, err)
	}
	awaitIdle(t, r, ctx)
}

func TestRefreshOnlyReleasesSelectedWorker(t *testing.T) {
	var calls atomic.Int32
	entered := make(chan struct{})
	r := scratchRuntime(t, modelFunc(func(ctx context.Context, _ *llm.CompletionRequest) messages.ChatMessage {
		if calls.Add(1) == 2 {
			close(entered)
			<-ctx.Done()
		}
		return answer("done")
	}), true)
	suspendAutoRelease(t, r)
	a := settledRefreshWorker(t, r)
	ctx := context.Background()
	sibling, err := r.start(ctx, "", AgentRequest{TaskName: "sibling", Label: "Sibling", Task: "inspect", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	before, _ := r.read(ctx)
	other := *before.Members[sibling.member]
	mail, err := r.followupTask(ctx, a.Session, "refresh", "selected", true)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := r.read(ctx)
	if !reflect.DeepEqual(*after.Members[sibling.member], other) || after.Contexts[other.Context] == nil || after.Contexts[a.Context] != nil || after.Followups[mail.ID].Task == a.Task {
		t.Fatal("targeted refresh changed another worker")
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestFollowupDoesNotClaimTerminalExecutionWillSteer(t *testing.T) {
	r := runtimeTest(t, doneModel(), 1, 3)
	suspendAutoRelease(t, r)
	finished, release := make(chan struct{}), make(chan struct{})
	var count atomic.Int32
	r.config.OnEvent = func(e Event) {
		if e.Kind == "finished" && count.Add(1) == 1 {
			close(finished)
			<-release
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	i, err := r.start(ctx, "", AgentRequest{TaskName: "worker", Label: "Worker", Task: "inspect", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	<-finished
	_, err = r.FollowupTask(ctx, i.member, "inspect again", "finishing")
	close(release)
	if err == nil || !strings.Contains(err.Error(), "finishing") {
		t.Fatalf("issued a false steering receipt: %v", err)
	}
	awaitIdle(t, r, ctx)
	admitParent(t, r)
	v, _, err := followupTool(t, r, "finishing", map[string]any{"target": i.member, "message": "inspect again"})
	if err != nil || v.Execution == i.id || v.Operation != "new_task" {
		t.Fatalf("idle retry: %+v %v", v, err)
	}
	awaitIdle(t, r, ctx)
}

func TestRefreshProvenanceThroughParentCallbacksDoesNotSettle(t *testing.T) {
	r := scratchRuntime(t, doneModel(), true)
	suspendAutoRelease(t, r)
	first := settledRefreshWorker(t, r)
	r.RegisterParentTools(r.config.Registry)
	var calls, results int
	brief := "Inspect the parent's current implementation"
	parent := llm.NewAgent(modelFunc(func(_ context.Context, _ *llm.CompletionRequest) messages.ChatMessage {
		calls++
		if calls == 1 {
			return iterationTool("parent-refresh", "followup_task", tools.Result(map[string]any{"target": first.Session, "message": brief, "refresh": true}))
		}
		return answer("checked")
	}), r.config.Registry, llm.AgentConfig{MaxIterations: 4})
	defer parent.Close()
	cb := &llm.AgentCallbacks{OnToolResult: func(call messages.ChatMessageToolCall, result messages.ChatMessage) {
		if call.Name != "followup_task" {
			return
		}
		results++
		var v FollowupView
		if err := json.Unmarshal([]byte(result.Content), &v); err != nil {
			t.Error(err)
			return
		}
		awaitIdle(t, r, context.Background())
		s, err := r.read(context.Background())
		if err != nil {
			t.Error(err)
			return
		}
		task := s.Tasks[v.Task]
		if v.Member != first.Session || v.BaseOrigin != "parent" || task == nil || !deliveringTask(s, task) || task.Delivery != nil || task.AcceptedRevision != 0 {
			t.Errorf("provenance settled work: %+v %+v", v, task)
		}
		if mail := s.Messages[v.Message]; mail == nil || mail.Task != "" || mail.Execution != "" || strings.Contains(result.Content, brief) {
			t.Error("tool result confused launch and completion evidence")
		}
	}}
	if _, err := r.RunParent(context.Background(), parent, &llm.CompletionRequest{}, cb, nil); err != nil {
		t.Fatal(err)
	}
	if results != 1 {
		t.Fatalf("follow-up results = %d", results)
	}
	history, err := r.config.Parent.GetHistory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, message := range history {
		if message.Role == messages.MessageRoleTool && message.ToolName == "followup_task" {
			found = true
			if strings.Contains(message.Content, brief) || !strings.Contains(message.Content, "baseCommit") {
				t.Fatal("persisted result lost compact provenance")
			}
		}
	}
	if !found {
		t.Fatal("follow-up result was not persisted")
	}
}

func TestRefreshFollowupRefusalsPreserveAssignments(t *testing.T) {
	for _, hold := range []string{"active", "queued", "running", "waiting", "paused", "canceled", "missing", "ready", "blocked", "delivering", "changes_requested", "other_open", "pending_followup", "preparing", "reserved", "applying", "recovery_required", "budget", "retained", "extra_edits", "context_busy"} {
		t.Run(hold, func(t *testing.T) {
			r := scratchRuntime(t, doneModel(), true)
			suspendAutoRelease(t, r)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			a, err := r.Agent(ctx, "", AgentRequest{Label: "Research", Task: "inspect", ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			admitParent(t, r)
			if err := r.update(ctx, func(s *State) error {
				switch hold {
				case "queued", "running", "waiting", "paused":
					s.Executions[a.Execution].Status = hold
				case "canceled", "ready", "blocked", "delivering", "changes_requested":
					s.Tasks[a.Task].Status = hold
				case "missing":
					delete(s.Tasks, a.Task)
				case "other_open":
					s.Tasks["other"] = &Task{ID: "other", Owner: a.Session, Status: "pending"}
				case "pending_followup":
					s.Messages["queued"] = &Mail{ID: "queued", To: a.Session, Start: true}
				case "preparing":
					s.Followups["another"] = &FollowupCall{Refresh: true, Member: a.Session, Phase: "preparing"}
				case "reserved":
					s.Members[a.Session].Controller = "workflow"
				case "applying", "recovery_required":
					s.Applies["apply"] = &ApplyRecord{Status: hold, Tasks: []TaskReference{{Task: a.Task}}}
				case "budget":
					for _, run := range s.Runs {
						run.Starts = run.Limit
					}
				case "retained":
					s.Contexts[a.Context].Release, s.Contexts[a.Context].Reason = WorkspaceRetained, "keep my edits"
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if hold == "active" {
				r.mu.Lock()
				r.active[a.Session] = &invocation{id: "active"}
				r.mu.Unlock()
				defer func() { r.mu.Lock(); delete(r.active, a.Session); r.mu.Unlock() }()
			}
			if hold == "reserved" {
				r.mu.Lock()
				r.workflowCancels["workflow"] = func() {}
				r.mu.Unlock()
				defer func() { r.mu.Lock(); delete(r.workflowCancels, "workflow"); r.mu.Unlock() }()
			}
			before, _ := r.read(ctx)
			c := before.Contexts[a.Context]
			if hold == "extra_edits" {
				writeRefreshFile(t, c.Root, "extra", "preserve this")
			}
			if hold == "context_busy" {
				lock := r.contextMutex(c.ID)
				lock.Lock()
				defer lock.Unlock()
			}
			if _, err := r.followupTask(ctx, a.Session, "refresh", "refused", true); err == nil {
				t.Fatal("refresh accepted", hold)
			}
			after, _ := r.read(ctx)
			if !reflect.DeepEqual(before.Tasks, after.Tasks) || !reflect.DeepEqual(before.Executions, after.Executions) || pendingFollowup(after, a.Session) != pendingFollowup(before, a.Session) {
				t.Fatal("refusal changed assignments or queued work")
			}
			if after.Members[a.Session].Context != a.Context {
				t.Fatal("refusal removed the old workspace")
			}
			if hold == "extra_edits" {
				if got, _ := os.ReadFile(filepath.Join(c.Root, "extra")); string(got) != "preserve this" {
					t.Fatal("extra worker edits lost")
				}
			}
		})
	}
}
