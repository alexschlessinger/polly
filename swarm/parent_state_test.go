package swarm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/subagent"
	"github.com/alexschlessinger/pollytool/tools"
)

// parentAgent gives a runtime a parent model with the parent tools bound.
func parentAgent(t *testing.T, r *Runtime, model llm.LLM, limit int) *llm.Agent {
	t.Helper()
	r.RegisterParentTools(r.config.Registry)
	agent := llm.NewAgent(model, r.config.Registry, llm.AgentConfig{MaxIterations: limit})
	t.Cleanup(func() { agent.Close() })
	return agent
}

func awaitParent(t *testing.T, r *Runtime, ctx context.Context, want Lifecycle) AgentPresentation {
	t.Helper()
	for {
		p := r.ParentState(nil)
		if p.Lifecycle == want {
			return p
		}
		select {
		case <-ctx.Done():
			t.Fatalf("parent never reached %s: %+v", want, p)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestRunParentOutcomeTable(t *testing.T) {
	malformed := messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse}
	for _, tc := range []struct {
		name     string
		task     bool
		limit    int
		cancel   bool
		reply    messages.ChatMessage
		display  string
		sentinel error
	}{
		{name: "success", limit: 3, reply: answer("hello"), display: "idle"},
		{name: "interrupted", limit: 3, cancel: true, reply: answer("late"), display: "paused · interrupted"},
		{name: "error", limit: 3, reply: malformed, display: "paused · error"},
		{name: "blocked", task: true, limit: 5, reply: answer("done"), display: "paused · blocked", sentinel: errSettlementBlocked},
		{name: "iteration limit", task: true, limit: 1, reply: answer("done"), display: "paused · iteration limit", sentinel: llm.ErrMaxIterations},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runtimeTest(t, idleModel(), 1, 2)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if tc.task {
				if _, err := r.CreateTask(ctx, "pending work", "review", nil, ""); err != nil {
					t.Fatal(err)
				}
			}
			runCtx, stop := context.WithCancel(ctx)
			defer stop()
			started := make(chan struct{}, 1)
			model := modelFunc(func(ctx context.Context, _ *llm.CompletionRequest) messages.ChatMessage {
				if tc.cancel {
					started <- struct{}{}
					<-ctx.Done()
				}
				return tc.reply
			})
			agent := parentAgent(t, r, model, tc.limit)
			if tc.cancel {
				go func() {
					<-started
					stop()
				}()
			}
			if p := r.ParentState(nil); p.Lifecycle != LifecycleIdle || p.Busy {
				t.Fatalf("fresh runtime: %+v", p)
			}
			_, err := r.RunParent(runCtx, agent, &llm.CompletionRequest{}, &llm.AgentCallbacks{}, nil)
			if tc.display == "idle" && err != nil {
				t.Fatal(err)
			}
			if tc.display != "idle" && err == nil {
				t.Fatal("turn succeeded")
			}
			if tc.sentinel != nil && !errors.Is(err, tc.sentinel) {
				t.Fatalf("err = %v, want %v", err, tc.sentinel)
			}
			if p := r.ParentState(nil); p.Display != tc.display || p.Busy {
				t.Fatalf("outcome %+v (err=%v)", p, err)
			}
		})
	}
}

func TestParentTurnSettledDowngradesIdleOnly(t *testing.T) {
	r := runtimeTest(t, idleModel(), 1, 2)
	ctx := context.Background()
	agent := parentAgent(t, r, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return answer("ok") }), 3)
	if _, err := r.RunParent(ctx, agent, &llm.CompletionRequest{}, nil, nil); err != nil {
		t.Fatal(err)
	}
	r.ParentTurnSettled(nil)
	if p := r.ParentState(nil); p.Display != "idle" {
		t.Fatalf("settled success: %+v", p)
	}
	r.ParentTurnSettled(errors.New("disk full"))
	if p := r.ParentState(nil); p.Display != "paused · error" {
		t.Fatalf("persistence failure: %+v", p)
	}
	if _, err := r.CreateTask(ctx, "pending work", "review", nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RunParent(ctx, agent, &llm.CompletionRequest{}, nil, nil); !errors.Is(err, errSettlementBlocked) {
		t.Fatalf("blocked turn: %v", err)
	}
	r.ParentTurnSettled(errors.New("later failure"))
	if p := r.ParentState(nil); p.Display != "paused · blocked" {
		t.Fatalf("a paused outcome was rewritten: %+v", p)
	}
}

func TestParentStateIdleDispositions(t *testing.T) {
	r := runtimeTest(t, idleModel(), 1, 1)
	accepted := &Task{ID: "a", Status: "awaiting_review", Revision: 2, AcceptedRevision: 2, Snapshot: "snap"}
	for name, tc := range map[string]struct {
		s    *State
		want string
	}{
		"nil state":            {nil, "idle"},
		"submitted":            {&State{Tasks: map[string]*Task{"t": {ID: "t", Status: "awaiting_review", Revision: 1}}}, "idle · awaiting review"},
		"accepted":             {&State{Tasks: map[string]*Task{"a": accepted}}, "idle · integration pending"},
		"review outranks":      {&State{Tasks: map[string]*Task{"a": accepted, "t": {ID: "t", Status: "awaiting_review", Revision: 1}}}, "idle · awaiting review"},
		"all done":             {&State{Runs: map[string]*Run{"r": {ID: "r", Status: "completed"}}, Tasks: map[string]*Task{"t": {ID: "t", Status: "done"}}}, "idle · done"},
		"work still assigned":  {&State{Runs: map[string]*Run{"r": {ID: "r", Status: "running"}}, Tasks: map[string]*Task{"t": {ID: "t", Status: "running"}}}, "idle"},
		"nothing ever started": {&State{}, "idle"},
	} {
		if p := r.ParentState(tc.s); p.Display != tc.want || p.Busy || p.Attention {
			t.Errorf("%s: %+v", name, p)
		}
	}
}

// A parked wait_agent beside a running tool keeps the parent active; once the
// tool ends only the wait remains and the parent shows waiting.
func TestParentStateWaitingOnlyWhenEveryInflightCallWaits(t *testing.T) {
	memberRelease, holdStarted, holdRelease := make(chan struct{}), make(chan struct{}), make(chan struct{})
	member := modelFunc(func(ctx context.Context, _ *llm.CompletionRequest) messages.ChatMessage {
		select {
		case <-memberRelease:
		case <-ctx.Done():
		}
		return answer("member done")
	})
	r := runtimeTest(t, member, 1, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	r.config.Registry.Register(&tools.Func{Name: "hold", Run: func(ctx context.Context, _ tools.Args) (string, error) {
		close(holdStarted)
		select {
		case <-holdRelease:
		case <-ctx.Done():
		}
		return "held", nil
	}})
	if _, err := r.Spawn(ctx, subagent.Request{Label: "Test agent", Task: "long work", ReadOnly: true, Review: true, Background: true}); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	parent := modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
		if calls.Add(1) == 1 {
			return messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: "hold", Name: "hold", Arguments: `{}`}, {ID: "wait", Name: "wait_agent", Arguments: `{}`}}}
		}
		return answer("done")
	})
	agent := parentAgent(t, r, parent, 4)
	done := make(chan error, 1)
	go func() {
		_, err := r.RunParent(ctx, agent, &llm.CompletionRequest{}, nil, nil)
		done <- err
	}()
	select {
	case <-holdStarted:
	case <-ctx.Done():
		t.Fatal("hold never started")
	}
	if p := r.ParentState(nil); p.Lifecycle != LifecycleActive {
		t.Fatalf("parent with a running tool: %+v", p)
	}
	close(holdRelease)
	awaitParent(t, r, ctx, LifecycleWaiting)
	close(memberRelease)
	select {
	case err := <-done:
		if !errors.Is(err, errSettlementBlocked) {
			t.Fatalf("turn ended with %v", err)
		}
	case <-ctx.Done():
		t.Fatal("parent turn never ended")
	}
	if p := r.ParentState(nil); p.Busy || p.Display != "paused · blocked" {
		t.Fatalf("after the turn: %+v", p)
	}
}

// Settling after a provisional final is a coordination wait too.
func TestParentStateSettlingIsWaiting(t *testing.T) {
	memberRelease := make(chan struct{})
	member := modelFunc(func(ctx context.Context, _ *llm.CompletionRequest) messages.ChatMessage {
		select {
		case <-memberRelease:
		case <-ctx.Done():
		}
		return answer("member done")
	})
	r := runtimeTest(t, member, 1, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := r.Spawn(ctx, subagent.Request{Label: "Test agent", Task: "long work", ReadOnly: true, Review: true, Background: true}); err != nil {
		t.Fatal(err)
	}
	agent := parentAgent(t, r, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return answer("done") }), 4)
	done := make(chan error, 1)
	go func() {
		_, err := r.RunParent(ctx, agent, &llm.CompletionRequest{}, nil, nil)
		done <- err
	}()
	awaitParent(t, r, ctx, LifecycleWaiting)
	close(memberRelease)
	select {
	case err := <-done:
		if !errors.Is(err, errSettlementBlocked) {
			t.Fatalf("turn ended with %v", err)
		}
	case <-ctx.Done():
		t.Fatal("parent turn never ended")
	}
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p := r.ParentState(s); p.Display != "paused · blocked" {
		t.Fatalf("after the turn: %+v", p)
	}
}

func TestListAgentsIncludesParentState(t *testing.T) {
	r := runtimeTest(t, idleModel(), 1, 1)
	out, err := r.inspectAgents(context.Background(), r.ID, tools.Args{})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"parentState":{"lifecycle":"idle"`) || !strings.Contains(string(data), `"parent":"`+r.ID+`"`) {
		t.Fatalf("list_agents page: %s", data)
	}
}

// The settlement nudge leads with the blocker and ends by asking for the
// restated answer; an unchanged state after it ends the turn blocked.
func TestParentNudgeLeadsWithBlockerAndAsksForTheAnswer(t *testing.T) {
	r := runtimeTest(t, idleModel(), 1, 2)
	ctx := context.Background()
	task, err := r.CreateTask(ctx, "pending work", "review", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	cb := &llm.AgentCallbacks{}
	r.bindParent(cb, nil)
	nudge, err := cb.ContinueAfterFinal(ctx, nil)
	if err != nil || len(nudge) != 1 {
		t.Fatalf("nudge: %+v %v", nudge, err)
	}
	synthetic, _ := nudge[0].Metadata[messages.MetadataKeyAgentSynthetic].(bool)
	text := nudge[0].Content
	if nudge[0].Role != messages.MessageRoleUser || !synthetic || !strings.HasPrefix(text, "Coordination is still outstanding: task ") || !strings.HasSuffix(text, "not a description of the coordination steps.") {
		t.Fatalf("nudge shape: %+v", nudge[0])
	}
	if !strings.Contains(text, "\n- task "+task.ID+" revision 1: pending. assign and run the task or cancel it\n\nYour previous answer was provisional.") {
		t.Fatalf("nudge does not list the decision with its action: %s", text)
	}
	if _, err := cb.ContinueAfterFinal(ctx, nil); !errors.Is(err, errSettlementBlocked) {
		t.Fatalf("unchanged coordination did not end the turn: %v", err)
	}
}

// The nudge lists at most ten decisions and points at swarm_read for the rest.
func TestParentNudgeCapsTheDecisionList(t *testing.T) {
	r := runtimeTest(t, idleModel(), 1, 2)
	ctx := context.Background()
	for i := 0; i < 12; i++ {
		if _, err := r.CreateTask(ctx, fmt.Sprintf("pending work %d", i), "review", nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	cb := &llm.AgentCallbacks{}
	r.bindParent(cb, nil)
	nudge, err := cb.ContinueAfterFinal(ctx, nil)
	if err != nil || len(nudge) != 1 {
		t.Fatalf("nudge: %+v %v", nudge, err)
	}
	text := nudge[0].Content
	if strings.Count(text, "\n- task ") != 10 || !strings.Contains(text, "\n… and 2 more; swarm_read lists them.\n\nYour previous answer was provisional.") || !strings.HasPrefix(text, "Coordination is still outstanding: 12 tasks unsettled; first: task ") {
		t.Fatalf("capped nudge: %s", text)
	}
}

// Work inside a call that also awaits an agent keeps the call active: a
// workflow running a tool in parallel with an agent await is not waiting.
func TestParentTrackerWorkInsideWaitingCallKeepsActive(t *testing.T) {
	tracker := &parentTracker{}
	turn, err := tracker.begin()
	if err != nil {
		t.Fatal(err)
	}
	tracker.callStart(turn, "wf")
	waiting := func() bool {
		_, waiting, _, _ := tracker.snapshot()
		return waiting
	}
	endWait := tracker.beginWait("wf")
	if !waiting() {
		t.Fatal("a call inside one await is not waiting")
	}
	endWork := tracker.beginWork("wf")
	if waiting() {
		t.Fatal("parallel work inside the awaiting call reported as waiting")
	}
	endWork()
	if !waiting() {
		t.Fatal("finished work left the awaiting call active")
	}
	endWait()
	if waiting() {
		t.Fatal("a call with no await is waiting")
	}
	if end := tracker.beginWork("unknown"); end == nil {
		t.Fatal("work outside a live call returned no end")
	}
	tracker.callEnd(turn, "wf")
}

// contextIndependentHold is an in-process tool that bound contexts inherit.
type contextIndependentHold struct{ *tools.Func }

func (contextIndependentHold) ContextIndependent() bool { return true }

// A foreground workflow that runs a tool step while another step awaits an
// agent keeps the parent active; it waits once only the await remains.
func TestParentStateActiveDuringParallelWorkflowTool(t *testing.T) {
	memberRelease, holdStarted, holdRelease := make(chan struct{}), make(chan struct{}), make(chan struct{})
	member := modelFunc(func(ctx context.Context, _ *llm.CompletionRequest) messages.ChatMessage {
		select {
		case <-memberRelease:
		case <-ctx.Done():
		}
		return answer("member done")
	})
	r := runtimeTest(t, member, 2, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	r.config.Registry.Register(contextIndependentHold{&tools.Func{Name: "hold", Desc: "hold", Run: func(ctx context.Context, _ tools.Args) (string, error) {
		close(holdStarted)
		select {
		case <-holdRelease:
		case <-ctx.Done():
		}
		return "held", nil
	}}})
	source := `polly.defineWorkflow({name:"parallel",inputSchema:polly.schema.object({}),async run(){
const context=await polly.context({readOnly:true,review:true});
const [a,b]=await Promise.all([polly.agent({label:"Test agent",task:"long work",readOnly:true,review:true}), polly.tool("hold",{},{context})]);
return b.text;}})`
	var calls atomic.Int32
	parent := modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
		if calls.Add(1) == 1 {
			return messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: "run", Name: "workflow_run", Arguments: tools.Result(map[string]any{"source": source, "input": "{}"})}}}
		}
		return answer("done")
	})
	agent := parentAgent(t, r, parent, 4)
	done := make(chan error, 1)
	go func() {
		_, err := r.RunParent(ctx, agent, &llm.CompletionRequest{}, nil, nil)
		done <- err
	}()
	select {
	case <-holdStarted:
	case <-ctx.Done():
		t.Fatal("hold never started")
	}
	awaitState(t, r, ctx, func(s *State) bool { return runningExecutions(s) == 1 })
	if p := r.ParentState(nil); p.Lifecycle != LifecycleActive {
		t.Fatalf("parent with a workflow tool running beside an agent await: %+v", p)
	}
	close(holdRelease)
	awaitParent(t, r, ctx, LifecycleWaiting)
	close(memberRelease)
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("parent turn never ended")
	}
	if p := r.ParentState(nil); p.Busy {
		t.Fatalf("after the turn: %+v", p)
	}
}
