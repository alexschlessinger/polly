package swarm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/workflow"
)

func TestWorkflowBlockInterruptsParkedSiblings(t *testing.T) {
	t.Parallel()
	for _, background := range []bool{false, true} {
		t.Run(map[bool]string{false: "foreground", true: "background"}[background], func(t *testing.T) {
			var r *Runtime
			block := make(chan struct{})
			model := modelFunc(func(ctx context.Context, req *llm.CompletionRequest) messages.ChatMessage {
				for _, msg := range req.Messages {
					if msg.Role == messages.MessageRoleUser && strings.Contains(msg.Content, "blocker assignment") {
						select {
						case <-block:
						case <-ctx.Done():
							return answer("canceled")
						}
						s, err := r.State(ctx)
						if err != nil {
							t.Error(err)
							return answer("state unavailable")
						}
						for _, m := range s.Members {
							if m.Label == "blocker" {
								task := s.Tasks[m.Task]
								return iterationTool("block", "swarm_block", tools.Result(map[string]any{"task": task.ID, "revision": task.Revision, "reason": "needs integrated sibling code"}))
							}
						}
					}
				}
				return iterationTool("park", "wait_agent", `{}`)
			})
			r = runtimeTest(t, model, 2, 2)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			source := `polly.workflow("blocked wave", polly.schema.obj({}), async () => {
 return polly.parallel(["parked", "blocker"], label => polly.research(label, label + " assignment"), {errors:"throw_after_all"});
});`
			run, err := r.launchWorkflow(ctx, source, map[string]any{}, background)
			if err != nil {
				t.Fatal(err)
			}
			defer r.CancelWorkflow(run.id)
			awaitState(t, r, ctx, func(s *State) bool {
				for _, m := range s.Members {
					if e := s.Executions[m.Execution]; m.Label == "parked" && e != nil && e.Status == "waiting" {
						return true
					}
				}
				return false
			})
			close(block)
			select {
			case <-run.done:
			case <-time.After(5 * time.Second):
				t.Fatal("explicit blocker did not return control while its sibling waited indefinitely")
			}
			var failure *workflow.Error
			if !errors.As(run.err, &failure) || failure.Code != "workflow_blocked" || run.report.Status != "interrupted" {
				t.Fatalf("blocker result: %+v, %v", run.report, run.err)
			}
			if r.HasActive() {
				t.Fatal("workflow returned before its workers settled")
			}
			s, err := r.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range s.Members {
				e, task := s.Executions[m.Execution], s.Tasks[m.Task]
				if e.Status != "paused" || m.Control == MemberControlStopped || task.Result != nil || task.AcceptedRevision != 0 {
					t.Fatalf("unfinished worker was stopped or accepted: %+v, %+v, %+v", m, e, task)
				}
				if m.Label == "blocker" {
					detail := run.report.Error.Result.(map[string]any)
					if failure.Session != m.ID || task.Status != "blocked" || task.Feedback != "needs integrated sibling code" || detail["task"] != task.ID || detail["execution"] != e.ID || detail["revision"] != float64(task.Revision) || detail["reason"] != task.Feedback {
						t.Fatalf("blocker provenance lost: %+v, %+v", run.report.Error, task)
					}
				}
			}
			assertWorkflowDelivery(t, r, false, false)
		})
	}
}

func TestWorkflowBlockDrainsRunningWorkers(t *testing.T) {
	t.Parallel()
	started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	r := runtimeTest(t, modelFunc(func(ctx context.Context, _ *llm.CompletionRequest) messages.ChatMessage {
		close(started)
		<-ctx.Done()
		close(canceled)
		<-release
		return answer("unfinished work")
	}), 1, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Release the model before cleanup joins the runtime, including on failure.
	defer close(release)
	run, err := r.launchWorkflow(ctx, `polly.workflow("drain",polly.schema.obj({}),async()=>
 polly.parallel(["busy","queued"], label=>polly.research(label,"work"), {errors:"throw_after_all"}));`, map[string]any{}, false)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("worker did not start")
	}
	s := awaitState(t, r, ctx, func(s *State) bool { return len(s.Executions) == 2 })
	var task *Task
	for _, m := range s.Members {
		if s.Executions[m.Execution].Status == "running" {
			task = s.Tasks[m.Task]
		}
	}
	if err := r.BlockTask(ctx, task.Owner, task.ID, task.Revision, "parent decision needed"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-canceled:
	case <-ctx.Done():
		t.Fatal("worker was not canceled")
	}
	if err := r.CancelWorkflow(run.id); err != nil {
		t.Fatal(err)
	}
	select {
	case <-run.done:
		t.Fatal("terminal report returned while worker was still executing")
	case <-time.After(30 * time.Millisecond):
	}
	s, _ = r.State(ctx)
	if s.Workflows[run.id].Status != "running" {
		t.Fatal("terminal report was persisted before draining")
	}
	// The deferred release deliberately keeps the worker active until all the
	// assertions above have observed shutdown waiting for it.
	t.Cleanup(func() {
		select {
		case <-run.done:
		case <-time.After(5 * time.Second):
			t.Fatal("workflow failed to finish after worker drained")
		}
		if run.report.Error.Code != "workflow_blocked" || run.report.Error.Session != task.Owner || !strings.Contains(run.report.Error.Message, "parent decision needed") {
			t.Fatalf("later cancellation replaced blocker: %+v", run.report.Error)
		}
		for _, step := range run.report.Steps {
			if step.Error == nil || step.Error.Session == "" {
				t.Fatalf("canceled running or queued worker lost its session: %+v", step)
			}
		}
	})
}

func TestAgentDrainPreservesCallerDeadline(t *testing.T) {
	t.Parallel()
	r := runtimeTest(t, modelFunc(func(ctx context.Context, _ *llm.CompletionRequest) messages.ChatMessage {
		<-ctx.Done()
		return answer("interrupted")
	}), 1, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := r.Agent(ctx, "", AgentRequest{Label: "deadline", Task: "work until deadline", ReadOnly: true})
	if !errors.Is(err, context.DeadlineExceeded) || result.Session == "" || r.HasActive() {
		t.Fatalf("draining changed caller deadline or left work active: %+v, %v", result, err)
	}
}

func TestWorkflowBlockDeliveryThroughParentLoop(t *testing.T) {
	t.Parallel()
	for _, background := range []bool{false, true} {
		t.Run(map[bool]string{false: "foreground", true: "background"}[background], func(t *testing.T) {
			var r *Runtime
			r = runtimeTest(t, modelFunc(func(ctx context.Context, _ *llm.CompletionRequest) messages.ChatMessage {
				s, err := r.State(ctx)
				if err != nil {
					t.Error(err)
					return answer("state unavailable")
				}
				for _, task := range s.Tasks {
					return iterationTool("block", "swarm_block", tools.Result(map[string]any{"task": task.ID, "revision": task.Revision, "reason": "missing prerequisite"}))
				}
				t.Error("no assigned task")
				return answer("no task")
			}), 1, 1)
			r.RegisterParentTools(r.config.Registry)
			calls := 0
			parent := modelFunc(func(ctx context.Context, req *llm.CompletionRequest) messages.ChatMessage {
				calls++
				if calls == 1 {
					return messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{workflowCall("blocked-workflow", `polly.workflow("blocked",polly.schema.obj({}),async()=>polly.research("worker","inspect"));`, background)}}
				}
				s, err := r.State(ctx)
				if err != nil {
					t.Error(err)
					return answer("state unavailable")
				}
				for _, w := range s.Workflows {
					if w.Status == "running" {
						return iterationTool("wait", "wait_agent", `{}`)
					}
					if !w.Acknowledged {
						return iterationTool("retain", "swarm_control", tools.Result(map[string]any{"action": "acknowledge_workflow", "id": w.ID, "defer": true, "note": "Preserve work until prerequisites are available"}))
					}
				}
				return answer("blocked work retained")
			})
			agent := llm.NewAgent(parent, r.config.Registry, llm.AgentConfig{MaxIterations: 6, ArtifactStore: r.config.Parent.ArtifactStore()})
			defer agent.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if _, err := r.RunParent(ctx, agent, &llm.CompletionRequest{}, nil, nil); err != nil {
				t.Fatal(err)
			}
			assertWorkflowDelivery(t, r, true, true)
			history, err := r.config.Parent.GetHistory(ctx)
			if err != nil {
				t.Fatal(err)
			}
			deliveries := 0
			for _, m := range history {
				if (m.Role == messages.MessageRoleTool || m.Role == messages.MessageRoleUser) && strings.Contains(m.Content, "workflow_blocked") {
					deliveries++
					if !strings.Contains(m.Content, "missing prerequisite") {
						t.Fatal("blocker reason missing from delivered result")
					}
				}
			}
			if deliveries != 1 {
				t.Fatalf("blocker delivered %d times", deliveries)
			}
		})
	}
}

func TestWorkflowBlockRejectsInvalidWritesAndLeavesOtherWorkRunning(t *testing.T) {
	t.Parallel()
	var failed *armedFailSession
	r := runtimeTestWithParent(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
		return iterationTool("park", "wait_agent", `{}`)
	}), 2, 2, func(s sessions.Session) sessions.Session {
		failed = &armedFailSession{countingSession: newCountingSession(s)}
		return failed
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	script := `polly.workflow("parked",polly.schema.obj({}),async()=>polly.research("parked","work"));`
	runs := make([]*workflowInvocation, 2)
	for n := range runs {
		var err error
		runs[n], err = r.launchWorkflow(ctx, script, map[string]any{}, false)
		if err != nil {
			t.Fatal(err)
		}
	}
	s := awaitState(t, r, ctx, func(s *State) bool {
		waiting := 0
		for _, e := range s.Executions {
			if e.Status == "waiting" {
				waiting++
			}
		}
		return waiting == 2
	})
	var task *Task
	for _, e := range s.Executions {
		if e.Workflow == runs[0].id {
			task = s.Tasks[s.Members[e.Member].Task]
		}
	}
	for _, test := range []struct {
		actor, reason string
		revision      int
		failWrite     bool
	}{
		{task.Owner, "stale", task.Revision + 1, false},
		{r.ID, "wrong owner", task.Revision, false},
		{task.Owner, " ", task.Revision, false},
		{task.Owner, "failed write", task.Revision, true},
	} {
		if test.failWrite {
			failed.remaining.Store(1)
		}
		if err := r.BlockTask(ctx, test.actor, task.ID, test.revision, test.reason); err == nil {
			t.Fatalf("invalid blocker accepted: %+v", test)
		}
		after, _ := r.State(ctx)
		if after.Tasks[task.ID].Revision != task.Revision || after.Tasks[task.ID].Status != "running" {
			t.Fatal("rejected blocker changed its task")
		}
		for _, run := range runs {
			select {
			case <-run.done:
				t.Fatal("rejected blocker interrupted a workflow")
			default:
			}
		}
	}
	if err := r.BlockTask(ctx, task.Owner, task.ID, task.Revision, "valid blocker"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runs[0].done:
	case <-ctx.Done():
		t.Fatal("blocked workflow did not return")
	}
	after, _ := r.State(ctx)
	if after.Workflows[runs[1].id].Status != "running" {
		t.Fatal("unrelated workflow was interrupted")
	}
	for _, e := range after.Executions {
		if e.Workflow == runs[1].id && e.Status != "waiting" {
			t.Fatal("ordinary indefinite wait was changed")
		}
	}
}

func TestWorkflowBlockPreservesCandidatesEditsAndRecovery(t *testing.T) {
	t.Parallel()
	var resumed atomic.Int32
	model := modelFunc(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		candidate, wrote := false, false
		for _, m := range req.Messages {
			candidate = candidate || m.Role == messages.MessageRoleUser && strings.Contains(m.Content, "candidate assignment")
			wrote = wrote || m.ToolName == "write_file"
		}
		if !wrote {
			return iterationTool("write", "write_file", `{"path":"work.txt","content":"retained edits"}`)
		}
		if candidate {
			return answer("candidate ready")
		}
		return iterationTool("park", "wait_agent", `{}`)
	})
	r := scratchRuntime(t, model, true)
	if _, err := r.config.Registry.LoadToolAuto("write_file"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	source := `polly.workflow("retained",polly.schema.obj({}),async()=>{
 await polly.agent("candidate","candidate assignment");
 return polly.agent("unfinished","unfinished assignment");
});`
	run, err := r.launchWorkflow(ctx, source, map[string]any{}, false)
	if err != nil {
		t.Fatal(err)
	}
	s := awaitState(t, r, ctx, func(s *State) bool {
		for _, m := range s.Members {
			if e := s.Executions[m.Execution]; m.Label == "unfinished" && e != nil && e.Status == "waiting" {
				return true
			}
		}
		return false
	})
	var unfinished, candidate *Task
	for _, m := range s.Members {
		if m.Label == "unfinished" {
			unfinished = s.Tasks[m.Task]
		} else {
			candidate = s.Tasks[m.Task]
		}
	}
	workspace := s.Contexts[s.Members[unfinished.Owner].Context]
	scratchFile := filepath.Join(workspace.Scratch, "helper.txt")
	if err := os.WriteFile(scratchFile, []byte("private helper"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := r.BlockTask(ctx, unfinished.Owner, unfinished.ID, unfinished.Revision, "integration prerequisite"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-run.done:
	case <-ctx.Done():
		t.Fatal("workflow did not return")
	}
	r = rebuildRuntime(t, r, func(c *Config) {
		c.Client = modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
			resumed.Add(1)
			return answer("completed after parent intervention")
		})
	})
	s, _ = r.State(ctx)
	if r.HasActive() || resumed.Load() != 0 || len(s.Executions) != 2 || s.Workflows[run.id].Error.Code != "workflow_blocked" {
		t.Fatal("reopen replayed or lost interrupted workflow")
	}
	savedCandidate := s.Tasks[candidate.ID]
	if candidate.Snapshot == "" || s.Snapshots[candidate.Snapshot] == nil || savedCandidate.Snapshot != candidate.Snapshot || savedCandidate.Revision != candidate.Revision || savedCandidate.Result != "candidate ready" || savedCandidate.AcceptedRevision != 0 {
		t.Fatalf("completed candidate lost or accepted: %+v", savedCandidate)
	}
	for path, want := range map[string]string{filepath.Join(workspace.Root, "work.txt"): "retained edits", scratchFile: "private helper"} {
		if got, err := os.ReadFile(path); err != nil || string(got) != want {
			t.Fatalf("retained file %s = %q, %v", path, got, err)
		}
	}
	if _, err := os.Stat(filepath.Join(r.config.Root, "work.txt")); !os.IsNotExist(err) {
		t.Fatal("unfinished edits reached parent files")
	}
	if _, err := r.FollowupTask(ctx, unfinished.Owner, "finish using retained work", "recover-blocker"); err != nil {
		t.Fatal(err)
	}
	awaitIdle(t, r, ctx)
	s, _ = r.State(ctx)
	if resumed.Load() != 1 || s.Executions[unfinished.Execution].Status != "completed" || s.Tasks[unfinished.ID].Snapshot == "" || s.Tasks[unfinished.ID].AcceptedRevision != 0 {
		t.Fatalf("parent continuation failed: %+v", s.Tasks[unfinished.ID])
	}
}
