package swarm

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"encoding/json"
	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/subagent"
	"github.com/alexschlessinger/pollytool/tools"
)

func runningExecutions(s *State) int {
	n := 0
	for _, e := range s.Executions {
		if e.Status == "running" {
			n++
		}
	}
	return n
}

// A parent parked during a background workflow wakes once, when the report
// turns terminal, and finds the workflow's one notice waiting.
func TestParentWaitSleepsThroughRunningWorkflow(t *testing.T) {
	release := []chan struct{}{make(chan struct{}), make(chan struct{})}
	var calls atomic.Int32
	r := runtimeTest(t, modelFunc(func(ctx context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		if n := int(calls.Add(1)) - 1; n < len(release) {
			select {
			case <-release[n]:
			case <-ctx.Done():
			}
		}
		return answer("done")
	}), 2, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id, err := r.StartWorkflow(ctx, `polly.defineWorkflow({name:"two",inputSchema:polly.schema.object({}),async run(){await polly.agent({label:"Test agent",task:"one",readOnly:true});return await polly.agent({label:"Test agent",task:"two",readOnly:true});}})`, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	awaitState(t, r, ctx, func(s *State) bool { return len(s.Executions) == 1 && runningExecutions(s) == 1 })
	woke := make(chan error, 1)
	go func() { woke <- r.waitParent(ctx) }()
	close(release[0])
	awaitState(t, r, ctx, func(s *State) bool { return len(s.Executions) == 2 && runningExecutions(s) == 1 })
	select {
	case err := <-woke:
		t.Fatalf("parent woke on workflow-internal progress: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(release[1])
	select {
	case err := <-woke:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("parent never woke for the finished workflow")
	}
	s := waitWorkflowIdle(t, r)
	if s.Workflows[id].Status != "completed" {
		t.Fatalf("workflow status = %s", s.Workflows[id].Status)
	}
	if pending := inbox(s, r.ID, true); len(pending) != 1 || pending[0].From != id {
		t.Fatalf("pending parent mail = %+v, want the workflow notice alone", pending)
	}
	for _, mail := range s.Messages {
		if _, member := s.Members[mail.From]; member {
			t.Fatalf("workflow member mailed the parent: %s", mail.Text)
		}
	}
}

// A workflow member's request still reaches a parked parent immediately, and
// the reply resumes the member inside the running workflow.
func TestWorkflowMemberRequestWakesParent(t *testing.T) {
	var r *Runtime
	var calls atomic.Int32
	r = runtimeTest(t, modelFunc(func(ctx context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		if calls.Add(1) == 1 {
			return messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: "ask", Name: "send_message", Arguments: tools.Result(map[string]any{"target": r.ID, "message": "which option?"})}, {ID: "wait", Name: "wait_agent", Arguments: `{}`}}}
		}
		return answer("completed")
	}), 1, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id, err := r.StartWorkflow(ctx, `polly.defineWorkflow({name:"ask",inputSchema:polly.schema.object({}),async run(){return await polly.agent({label:"Test agent",task:"ask parent",readOnly:true});}})`, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.waitParent(ctx); err != nil {
		t.Fatal(err)
	}
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.Workflows[id].Status != "running" {
		t.Fatalf("workflow finished before the parent answered: %s", s.Workflows[id].Status)
	}
	pending := inbox(s, r.ID, true)
	if len(pending) != 1 || pending[0].Kind != "info" {
		t.Fatalf("pending parent mail = %+v, want the member's request", pending)
	}
	member := pending[0].From
	if _, err := r.Send(ctx, r.ID, member, "info", "", "choose A"); err != nil {
		t.Fatal(err)
	}
	s = waitWorkflowIdle(t, r)
	if s.Workflows[id].Status != "completed" {
		t.Fatalf("workflow status = %s", s.Workflows[id].Status)
	}
	kinds := map[string]int{}
	for _, mail := range inbox(s, r.ID, true) {
		kinds[mail.Kind]++
		if mail.From == member && mail.Task != "" {
			t.Fatalf("workflow member posted completion mail: %s", mail.Text)
		}
	}
	if kinds["info"] != 2 || len(kinds) != 1 {
		t.Fatalf("pending parent mail kinds = %v, want one request and the workflow notice", kinds)
	}
}

// Work no workflow controls keeps waking the parent while a workflow runs.
func TestDirectSpawnStillWakesParentDuringWorkflow(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	r := runtimeTest(t, modelFunc(func(ctx context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		if calls.Add(1) == 1 {
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		return answer("done")
	}), 2, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id, err := r.StartWorkflow(ctx, `polly.defineWorkflow({name:"hold",inputSchema:polly.schema.object({}),async run(){return await polly.agent({label:"Test agent",task:"inspect",readOnly:true});}})`, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	awaitState(t, r, ctx, func(s *State) bool { return runningExecutions(s) == 1 })
	woke := make(chan error, 1)
	go func() { woke <- r.waitParent(ctx) }()
	child, err := r.Spawn(ctx, subagent.Request{Label: "Test agent", Task: "direct", ReadOnly: true, Background: true})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-woke:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("direct spawn did not wake the parent")
	}
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.Workflows[id].Status != "running" {
		t.Fatalf("workflow status = %s, want running", s.Workflows[id].Status)
	}
	select {
	case <-child.Done:
	case <-ctx.Done():
		t.Fatal("direct child never finished")
	}
	awaitState(t, r, ctx, func(s *State) bool {
		for _, mail := range s.Messages {
			if mail.From == child.Session && mail.Task != "" && mail.Execution != "" {
				return true
			}
		}
		return false
	})
	close(release)
	waitWorkflowIdle(t, r)
}

func TestWorkflowBackgroundResultNamesSwarmWait(t *testing.T) {
	r := runtimeTest(t, nilModel(), 1, 1)
	ctx := context.Background()
	r.RegisterParentTools(r.config.Registry)
	wait, _, _ := r.config.Registry.GetIfAllowed("wait_agent")
	if desc := wait.GetSchema().Description(); !strings.Contains(desc, "terminal status") || !strings.Contains(desc, "instead of sleeping") {
		t.Fatalf("parent wait_agent description: %s", desc)
	}
	start, _, _ := r.config.Registry.GetIfAllowed("workflow_run")
	if desc := start.GetSchema().Description(); !strings.Contains(desc, "wait_agent") || strings.Contains(desc, "coordinate until") {
		t.Fatalf("workflow_run description: %s", desc)
	}
	out, err := start.Execute(ctx, map[string]any{"source": `polly.defineWorkflow({name:"noop",inputSchema:polly.schema.object({}),async run(){return 1}})`, "input": "{}", "background": true})
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	id, _ := result["id"].(string)
	next, _ := result["next"].(string)
	if result["status"] != "started" || id == "" || !strings.Contains(next, "wait_agent") || !strings.Contains(next, "output delivered") {
		t.Fatalf("workflow_run result: %s", out)
	}
	waitWorkflowIdle(t, r)
}
