package swarm

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/subagent"
	"github.com/alexschlessinger/pollytool/tools"
)

func TestDelegationNamesAndRemovedArguments(t *testing.T) {
	r := runtimeTest(t, doneModel(), 1, 5)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r.RegisterParentTools(r.config.Registry)
	spawn, _, _ := r.config.Registry.GetIfAllowed("spawn_agent")
	for _, a := range []map[string]any{
		{"task": "legacy", "label": "old"},
		{"task_name": "../escape", "message": "bad"},
		{"task_name": "valid", "message": "bad", "session": "old"},
		{"task_name": "valid", "message": "bad", "background": false},
	} {
		if _, err := spawn.Execute(ctx, a); err == nil {
			t.Fatalf("accepted removed or invalid arguments: %v", a)
		}
	}
	args := map[string]any{"task_name": "cache_audit", "message": "Inspect cache", "read_only": true}
	callCtx := subagent.WithCallID(ctx, "spawn-cache")
	text, err := spawn.Execute(callCtx, args)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatal(err)
	}
	id := out["member"].(string)
	awaitIdle(t, r, ctx)
	s, _ := r.State(ctx)
	if agentName(s.Members[id]) != "/root/cache_audit" || out["task_name"] != "/root/cache_audit" {
		t.Fatal(out)
	}
	for _, actor := range []string{r.ID, id} {
		for _, target := range []string{id, "cache_audit", "/root/cache_audit"} {
			if got, err := r.resolveTarget(s, actor, target); err != nil || got != id {
				t.Fatalf("target %s: %s %v", target, got, err)
			}
		}
	}
	for _, target := range []string{"/foreign/cache_audit", "../cache_audit", "unknown"} {
		if _, err := r.resolveTarget(s, r.ID, target); err == nil {
			t.Fatal(target)
		}
	}
	if _, err := spawn.Execute(ctx, args); err == nil {
		t.Fatal("duplicate name accepted")
	}
	// Repeated delivery of the same call is not a second launch.
	if _, err := spawn.Execute(callCtx, args); err != nil {
		t.Fatal(err)
	}
	read, _, _ := r.config.Registry.GetIfAllowed("swarm_read")
	if _, err := read.Execute(ctx, map[string]any{"view": "agents"}); err == nil {
		t.Fatal("removed agents view accepted")
	}
	control, _, _ := r.config.Registry.GetIfAllowed("swarm_control")
	for _, action := range []string{"stop", "resume"} {
		if _, err := control.Execute(ctx, map[string]any{"action": action, "id": id}); err == nil {
			t.Fatal("removed control accepted")
		}
	}
	send, _, _ := r.config.Registry.GetIfAllowed("send_message")
	if _, err := send.Execute(ctx, map[string]any{"to": id, "kind": "request", "text": "legacy"}); err == nil {
		t.Fatal("legacy send accepted")
	}
	if _, err := send.Execute(ctx, map[string]any{"target": "cache_audit", "message": "Information only"}); err != nil {
		t.Fatal(err)
	}
	r.wakeIdleMember(id)
	s, _ = r.State(ctx)
	if len(s.Executions) != 1 {
		t.Fatal("send started idle worker")
	}
}

func TestPendingFollowupSurvivesRuntimeRestart(t *testing.T) {
	entered := make(chan struct{})
	var calls atomic.Int32
	r := runtimeTest(t, modelFunc(func(ctx context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		if calls.Add(1) == 1 {
			close(entered)
			<-ctx.Done()
			return answer("interrupted")
		}
		found := false
		for _, m := range req.Messages {
			found = found || strings.Contains(m.Content, "Durable follow-up evidence")
		}
		if !found {
			t.Error("restart lost pending follow-up")
		}
		return answer("checked")
	}), 1, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	i, err := r.start(ctx, "", AgentRequest{TaskName: "worker", Label: "Worker", Task: "inspect", ReadOnly: true, Review: true})
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	if _, err := r.FollowupTask(ctx, "worker", "Durable follow-up evidence", "pending"); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	s, _ := r.State(ctx)
	if !pendingFollowup(s, i.member) || s.Executions[i.id].Status != "paused" {
		t.Fatal("pending intent lost on shutdown")
	}
	// Promote and reopen the actual coordination store, not just a Go struct.
	db := t.TempDir() + "/swarm.db"
	if err := r.config.Store.(sessions.DurableStore).Promote(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := r.config.Parent.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.config.Store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := sessions.OpenStore(sessions.StoreConfig{Mode: sessions.ModeDisk, Path: db})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	parent, err := store.Acquire(ctx, "parent", sessions.AcquireOptions{ExpectedID: r.ID, ExistingOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	config := r.config
	config.Store, config.Parent = store, parent
	config.Agent.MaxIterations = 100
	recovered, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if _, err := recovered.FollowupTask(ctx, "worker", "Continue after restart", ""); err != nil {
		t.Fatal(err)
	}
	awaitIdle(t, recovered, ctx)
	s, _ = recovered.State(ctx)
	e := s.Executions[i.id]
	if len(s.Executions) != 1 || e.Request.MaxIterations != 5 || e.Status != "completed" || pendingFollowup(s, i.member) || agentName(s.Members[i.member]) != "/root/worker" {
		t.Fatalf("recovery changed identity or budget: %+v", e)
	}
}

func TestConcurrentInterruptThenFollowupKeepsWork(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	r := runtimeTest(t, modelFunc(func(_ context.Context, _ *llm.CompletionRequest) messages.ChatMessage {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		return answer("checked")
	}), 1, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	i, err := r.start(ctx, "", AgentRequest{TaskName: "worker", Label: "Worker", Task: "inspect", ReadOnly: true, Review: true})
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	interrupted := make(chan error, 1)
	go func() { _, err := r.InterruptAgent(ctx, "worker"); interrupted <- err }()
	for !i.interrupted.Load() {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	followed := make(chan error, 1)
	go func() { _, err := r.FollowupTask(ctx, "worker", "Continue after interruption", ""); followed <- err }()
	close(release)
	if err := <-interrupted; err != nil {
		t.Fatal(err)
	}
	if err := <-followed; err != nil {
		t.Fatal(err)
	}
	s := awaitState(t, r, ctx, func(s *State) bool {
		return s.Executions[i.id].Generation == 2 && s.Executions[i.id].Status == "completed"
	})
	awaitIdle(t, r, ctx)
	if len(s.Executions) != 1 || s.Executions[i.id].Generation != 2 || s.Executions[i.id].Status != "completed" || pendingFollowup(s, i.member) {
		t.Fatal("concurrent follow-up was lost or duplicated")
	}
}

func TestInterruptDoesNotHoldLaunchLockWhileJoiningCallback(t *testing.T) {
	var r *Runtime
	var calls atomic.Int32
	entered := make(chan struct{})
	r = runtimeTest(t, modelFunc(func(ctx context.Context, _ *llm.CompletionRequest) messages.ChatMessage {
		if calls.Add(1) == 1 {
			close(entered)
			<-ctx.Done()
			// A host callback may request work while cancellation settles.
			// It must not deadlock with the interrupt joining this invocation.
			if _, err := r.FollowupTask(context.WithoutCancel(ctx), "worker", "Continue from the callback", ""); err != nil {
				t.Error(err)
			}
		}
		return answer("checked")
	}), 1, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	i, err := r.start(ctx, "", AgentRequest{TaskName: "worker", Label: "Worker", Task: "inspect", ReadOnly: true, Review: true})
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	if _, err := r.InterruptAgent(ctx, "worker"); err != nil {
		t.Fatal(err)
	}
	s := awaitState(t, r, ctx, func(s *State) bool {
		return s.Executions[i.id].Generation == 2 && s.Executions[i.id].Status == "completed"
	})
	if len(s.Executions) != 1 || calls.Load() != 2 {
		t.Fatal("callback follow-up was lost or duplicated")
	}
}

func TestFollowupHonorsWorkflowReservationAndUncertainApply(t *testing.T) {
	r := runtimeTest(t, doneModel(), 1, 3)
	ctx := context.Background()
	first, err := r.Agent(ctx, "", AgentRequest{TaskName: "worker", Label: "Worker", Task: "inspect", ReadOnly: true, Review: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.update(ctx, func(s *State) error { s.Members[first.Session].Controller = "workflow"; return nil }); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	r.workflowCancels["workflow"] = func() {}
	r.mu.Unlock()
	if _, err := r.FollowupTask(ctx, "worker", "steal", ""); err == nil {
		t.Fatal("workflow reservation bypassed")
	}
	if _, err := r.InterruptAgent(ctx, "worker"); err == nil {
		t.Fatal("workflow reservation interrupted")
	}
	r.mu.Lock()
	delete(r.workflowCancels, "workflow")
	r.mu.Unlock()
	if err := r.update(ctx, func(s *State) error {
		s.Applies["uncertain"] = &ApplyRecord{ID: "uncertain", Status: "recovery_required", Tasks: []TaskReference{{Task: first.Task, Revision: 3}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.FollowupTask(ctx, "worker", "continue", ""); err == nil || !strings.Contains(err.Error(), "reconcile") {
		t.Fatalf("uncertain apply reopened: %v", err)
	}
}

func TestWaitAgentTimeouts(t *testing.T) {
	t.Run("parent", func(t *testing.T) {
		t.Parallel()
		entered := make(chan struct{})
		r := runtimeTest(t, modelFunc(func(ctx context.Context, _ *llm.CompletionRequest) messages.ChatMessage {
			close(entered)
			<-ctx.Done()
			return answer("interrupted")
		}), 1, 2)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, err := r.start(ctx, "", AgentRequest{Label: "Worker", Task: "wait", ReadOnly: true}); err != nil {
			t.Fatal(err)
		}
		<-entered
		r.RegisterParentTools(r.config.Registry)
		wait, _, _ := r.config.Registry.GetIfAllowed("wait_agent")
		for _, timeout := range []int{0, 9999, 3600001} {
			if _, err := wait.Execute(ctx, map[string]any{"timeout_ms": timeout}); err == nil {
				t.Fatalf("accepted timeout %d", timeout)
			}
		}
		out, err := wait.Execute(ctx, map[string]any{"timeout_ms": 10000})
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			TimedOut bool `json:"timed_out"`
		}
		if err := json.Unmarshal([]byte(out), &result); err != nil || !result.TimedOut {
			t.Fatalf("timeout result: %s %v", out, err)
		}
		cancelled, stop := context.WithCancel(ctx)
		stop()
		if _, err := wait.Execute(cancelled, nil); err == nil {
			t.Fatal("user cancellation ignored")
		}
	})
	t.Run("child releases slot", func(t *testing.T) {
		t.Parallel()
		var calls atomic.Int32
		r := runtimeTest(t, modelFunc(func(_ context.Context, _ *llm.CompletionRequest) messages.ChatMessage {
			if calls.Add(1) == 1 {
				return messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: "wait1", Name: "wait_agent", Arguments: `{"timeout_ms":10000}`}, {ID: "wait2", Name: "wait_agent", Arguments: `{"timeout_ms":10000}`}}}
			}
			return answer("timeout finished")
		}), 1, 2)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		i, err := r.start(ctx, "", AgentRequest{Label: "Worker", Task: "wait", ReadOnly: true, Review: true})
		if err != nil {
			t.Fatal(err)
		}
		awaitState(t, r, ctx, func(s *State) bool { return s.Executions[i.id].Status == "waiting" && len(r.slots) == 0 })
		if len(r.slots) != 0 {
			t.Fatal("waiting child retained execution slot")
		}
		awaitIdle(t, r, ctx)
		s, _ := r.State(ctx)
		if len(s.Executions) != 1 || s.Executions[i.id].Status != "completed" || calls.Load() != 2 {
			t.Fatal("timeout did not resume same execution")
		}
	})
}

func TestFollowupDuringFinalizationUsesSameExecution(t *testing.T) {
	for _, typed := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "typed"}[typed], func(t *testing.T) {
			var r *Runtime
			var calls atomic.Int32
			model := modelFunc(func(ctx context.Context, req *llm.CompletionRequest) messages.ChatMessage {
				if calls.Add(1) == 1 {
					if _, err := r.FollowupTask(ctx, "cache_audit", "Also verify eviction", "steer"); err != nil {
						t.Error(err)
					}
				} else {
					found := false
					for _, m := range req.Messages {
						found = found || strings.Contains(m.Content, "Also verify eviction")
					}
					if !found {
						t.Error("follow-up lost at final boundary")
					}
				}
				if typed {
					return completion(`{"answer":"verified"}`)
				}
				return answer("verified")
			})
			r = runtimeTest(t, model, 1, 2)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			req := AgentRequest{TaskName: "cache_audit", Label: "Cache audit", Task: "inspect", ReadOnly: true, Review: true}
			if typed {
				req.Schema = map[string]any{"type": "object", "properties": map[string]any{"answer": map[string]any{"type": "string"}}, "required": []string{"answer"}}
			}
			result, err := r.Agent(ctx, "", req)
			if err != nil {
				t.Fatal(err)
			}
			s, _ := r.State(ctx)
			task := s.Tasks[result.Task]
			if calls.Load() != 2 || len(s.Executions) != 1 || len(s.Tasks) != 1 || task.Status != "awaiting_review" || task.AcceptedRevision != 0 {
				t.Fatalf("calls=%d state=%+v", calls.Load(), task)
			}
			for _, mail := range s.Messages {
				if mail.Start && !mail.Delivered {
					t.Fatal("follow-up not checkpointed")
				}
			}
			if err := r.Review(ctx, task.ID, task.Revision, true, ""); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFollowupDuringToolBatchAdmitsOnce(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	r := runtimeTest(t, modelFunc(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		if calls.Add(1) == 1 {
			return iterationTool("hold", "hold", `{}`)
		}
		count := 0
		for _, m := range req.Messages {
			if m.Role == messages.MessageRoleUser && strings.Contains(m.Content, "Check the race") {
				count++
			}
		}
		if count != 1 {
			t.Errorf("admitted %d times", count)
		}
		return answer("checked")
	}), 1, 2)
	r.config.Registry.Register(contextIndependentHold{&tools.Func{Name: "hold", Run: func(ctx context.Context, _ tools.Args) (string, error) {
		close(entered)
		select {
		case <-release:
			return "held", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	i, err := r.start(ctx, "", AgentRequest{TaskName: "worker", Label: "Worker", Task: "inspect", ReadOnly: true, Review: true})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if _, err := r.FollowupTask(ctx, i.member, "Check the race", ""); err != nil {
		t.Fatal(err)
	}
	close(release)
	awaitIdle(t, r, ctx)
	s, _ := r.State(ctx)
	if len(s.Executions) != 1 || calls.Load() != 2 {
		t.Fatalf("executions=%d calls=%d", len(s.Executions), calls.Load())
	}
}

func TestInterruptPreservesAssignmentAndFencesLateFinal(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	r := runtimeTest(t, modelFunc(func(_ context.Context, _ *llm.CompletionRequest) messages.ChatMessage {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		return answer("result")
	}), 1, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	i, err := r.start(ctx, "", AgentRequest{TaskName: "worker", Label: "Worker", Task: "inspect", ReadOnly: true, Review: true})
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	interrupted := make(chan error, 1)
	go func() { _, err := r.InterruptAgent(ctx, "worker"); interrupted <- err }()
	for !i.interrupted.Load() {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	close(release)
	if err := <-interrupted; err != nil {
		t.Fatal(err)
	}
	s, _ := r.State(ctx)
	m := s.Members[i.member]
	taskID := m.Task
	if m.Control != MemberControlEnabled || s.Executions[i.id].Status != "paused" || s.Tasks[taskID].Status == "done" || s.Tasks[taskID].Status == "awaiting_review" {
		t.Fatalf("late final completed interrupted work: %+v", s.Tasks[taskID])
	}
	if _, err := r.FollowupTask(ctx, "worker", "Continue verification", ""); err != nil {
		t.Fatal(err)
	}
	awaitIdle(t, r, ctx)
	s, _ = r.State(ctx)
	if len(s.Executions) != 1 || s.Members[i.member].Task != taskID || s.Executions[i.id].Generation != 2 || s.Tasks[taskID].Status != "awaiting_review" {
		t.Fatalf("continuation lost identity: %+v", s.Executions[i.id])
	}
	if _, err := r.InterruptAgent(ctx, "worker"); err != nil {
		t.Fatal(err)
	}
	s, _ = r.State(ctx)
	if s.Executions[i.id].Status != "completed" {
		t.Fatal("idle interrupt changed result")
	}
}

func TestFollowupCannotBypassIterationAllowance(t *testing.T) {
	var r *Runtime
	r = runtimeTest(t, modelFunc(func(ctx context.Context, _ *llm.CompletionRequest) messages.ChatMessage {
		if _, err := r.FollowupTask(ctx, "worker", "One more check", "last-call"); err != nil {
			t.Error(err)
		}
		return answer("provisional")
	}), 1, 2)
	r.UpdateDefaults(r.config.Request, llm.AgentConfig{MaxIterations: 1}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := r.Agent(ctx, "", AgentRequest{TaskName: "worker", Label: "Worker", Task: "inspect", ReadOnly: true, Review: true})
	if !llm.IsIterationLimit(err) {
		t.Fatalf("limit bypassed: %v", err)
	}
	if _, err := r.FollowupTask(ctx, "worker", "Try again", ""); !llm.IsIterationLimit(err) {
		t.Fatalf("follow-up granted iterations: %v", err)
	}
	s, _ := r.State(ctx)
	if len(s.Executions) != 1 || s.Tasks[result.Task].Status != "blocked" {
		t.Fatal("exhausted work completed")
	}
}

func TestReviewFeedbackDoesNotRestartOldOwnerAfterReassignment(t *testing.T) {
	r := runtimeTest(t, doneModel(), 1, 6)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	first, err := r.Agent(ctx, "", AgentRequest{TaskName: "original", Label: "Original", Task: "inspect", ReadOnly: true, Review: true})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := r.State(ctx)
	task := s.Tasks[first.Task]
	if err := r.Review(ctx, task.ID, task.Revision, false, "Check the baseline"); err != nil {
		t.Fatal(err)
	}
	s, _ = r.State(ctx)
	task = s.Tasks[first.Task]
	if err := r.UpdateTask(ctx, task.ID, task.Revision, "", nil); err != nil {
		t.Fatal(err)
	}
	second, err := r.Agent(ctx, "", AgentRequest{TaskID: task.ID, TaskName: "replacement", Label: "Replacement", Task: "Check the baseline", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	r.wakeIdleMember(first.Session)
	awaitIdle(t, r, ctx)
	s, _ = r.State(ctx)
	if len(s.Tasks) != 1 || len(s.Executions) != 2 || second.Task != first.Task || s.Tasks[first.Task].Owner != second.Session {
		t.Fatal("feedback spawned an extra original-owner assignment")
	}
}
