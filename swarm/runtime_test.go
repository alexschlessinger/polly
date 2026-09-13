package swarm

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/subagent"
	"github.com/alexschlessinger/pollytool/tools"
)

type modelFunc func(context.Context, *llm.CompletionRequest) messages.ChatMessage

func (f modelFunc) ChatCompletionStream(ctx context.Context, req *llm.CompletionRequest, p llm.EventStreamProcessor) <-chan *messages.StreamEvent {
	ch := make(chan messages.ChatMessage, 1)
	ch <- f(ctx, req)
	close(ch)
	return p.ProcessMessagesToEvents(ch)
}
func answer(text string) messages.ChatMessage {
	return messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: text, StopReason: messages.StopReasonEndTurn}
}
func runtimeTest(t *testing.T, model llm.LLM, concurrent, starts int) *Runtime {
	t.Helper()
	return runtimeTestWithParent(t, model, concurrent, starts, nil)
}

// runtimeTestWithParent lets a test wrap the root session, for example to
// hold the parent's coordination writes while observing member state.
func runtimeTestWithParent(t *testing.T, model llm.LLM, concurrent, starts int, wrap func(sessions.Session) sessions.Session) *Runtime {
	t.Helper()
	ctx := context.Background()
	store, err := sessions.OpenStore(sessions.StoreConfig{Mode: sessions.ModeMemory})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := store.Acquire(ctx, "parent", sessions.AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	root := parent
	if wrap != nil {
		root = wrap(parent)
	}
	registry := tools.NewToolRegistry(nil, tools.WithUnsafeNoSandbox())
	r, err := New(Config{Store: store, Parent: root, Registry: registry, Client: model, Root: t.TempDir(), Directory: filepath.Join(t.TempDir(), "runtime"), MaxConcurrent: concurrent, MaxExecutions: starts, Agent: llm.AgentConfig{MaxIterations: 5}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close(); parent.Close(); registry.Close(); store.Close() })
	return r
}

func TestWaitResumesSameExecutionAndAdmitsOnce(t *testing.T) {
	var r *Runtime
	var calls atomic.Int32
	model := modelFunc(func(ctx context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		if calls.Add(1) == 1 {
			return messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: "ask", Name: "send_message", Arguments: tools.Result(map[string]any{"target": r.ID, "message": "which option?"})}, {ID: "wait", Name: "wait_agent", Arguments: `{}`}}}
		}
		if !strings.Contains(req.Messages[len(req.Messages)-1].Content, "choose A") {
			t.Error("reply not admitted before model request")
		}
		return answer("completed")
	})
	r = runtimeTest(t, model, 1, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := r.Spawn(ctx, subagent.Request{Label: "Test agent", Task: "ask parent", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Yielded {
		t.Fatalf("blocking parent did not yield: %+v", result)
	}
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	requests := inbox(s, r.ID, true)
	if len(requests) != 1 {
		t.Fatalf("requests %v", requests)
	}
	// A settings update cannot replace the parked invocation's original cap.
	r.UpdateDefaults(r.config.Request, llm.AgentConfig{MaxIterations: 1}, nil)
	if _, err = r.Send(ctx, r.ID, result.Session, "info", "", "choose A"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-result.Done:
	case <-ctx.Done():
		t.Fatal("member failed to resume")
	}
	s, err = r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range s.Runs {
		if run.Starts != 1 {
			t.Fatalf("wait spent budget: %+v", run)
		}
	}
	for _, e := range s.Executions {
		if e.Iterations != 2 || e.Status != "completed" {
			t.Fatalf("execution: %+v", e)
		}
	}
	for _, mail := range s.Messages {
		if mail.To == result.Session && !mail.Delivered {
			t.Fatal("reply receipt not saved")
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("duplicate admission/restart: %d", calls.Load())
	}
}

func TestSharedPoolAndLogicalBudget(t *testing.T) {
	var active, peak atomic.Int32
	r := runtimeTest(t, modelFunc(func(ctx context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		n := active.Add(1)
		defer active.Add(-1)
		for {
			old := peak.Load()
			if old >= n || peak.CompareAndSwap(old, n) {
				break
			}
		}
		select {
		case <-time.After(10 * time.Millisecond):
		case <-ctx.Done():
		}
		return answer("done")
	}), 2, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	var successes atomic.Int32
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.Agent(ctx, "", AgentRequest{Label: "Test agent", Task: "read", ReadOnly: true, Tools: []string{}})
			if err == nil {
				successes.Add(1)
			} else if !errors.Is(err, ErrBudget) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if peak.Load() > 2 || successes.Load() != 4 {
		t.Fatalf("pool/budget peak=%d success=%d", peak.Load(), successes.Load())
	}
}

func TestClaimsReviewAndNoTools(t *testing.T) {
	r := runtimeTest(t, modelFunc(func(ctx context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		if len(req.Tools) > 0 {
			t.Error("tools: [] exposed private built-ins")
		}
		return answer("done")
	}), 2, 4)
	ctx := context.Background()
	a, err := r.Agent(ctx, "", AgentRequest{Label: "Test agent", Task: "one", ReadOnly: true, Review: true, Tools: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.Agent(ctx, "", AgentRequest{Label: "Test agent", Task: "two", ReadOnly: true, Review: true, Tools: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	task, err := r.CreateTask(ctx, "claim me", "correct", nil, "", CreateTaskOptions{Review: true})
	if err != nil {
		t.Fatal(err)
	}
	var won atomic.Int32
	var owner string
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, id := range []string{a.Session, b.Session} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			if r.Claim(ctx, id, task.ID, task.Revision) == nil {
				won.Add(1)
				mu.Lock()
				owner = id
				mu.Unlock()
			}
		}(id)
	}
	wg.Wait()
	if won.Load() != 1 {
		t.Fatalf("claim winners=%d", won.Load())
	}
	s, _ := r.State(ctx)
	task = s.Tasks[task.ID]
	if err = r.Submit(ctx, owner, task.ID, task.Revision, "result", ""); err != nil {
		t.Fatal(err)
	}
	s, _ = r.State(ctx)
	task = s.Tasks[task.ID]
	if err = r.Review(ctx, task.ID, task.Revision, false, "more evidence"); err != nil {
		t.Fatal(err)
	}
	s, _ = r.State(ctx)
	task = s.Tasks[task.ID]
	if task.Feedback != "more evidence" || task.Owner != owner {
		t.Fatalf("rejection lost owner: %+v", task)
	}
}

func TestWorkflowUsesSameMemberAndPrivateReservation(t *testing.T) {
	var requests atomic.Int32
	r := runtimeTest(t, modelFunc(func(ctx context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		requests.Add(1)
		return answer(`{"ok":true}`)
	}), 1, 2)
	source := `polly.defineWorkflow({name:"test",inputSchema:polly.schema.object({}),async run(){const a=await polly.agent({label:"Test agent",task:"read",readOnly:true,tools:[],schema:polly.schema.object({ok:polly.schema.boolean()})}); const b=await polly.agent({session:a.session,task:"again",schema:polly.schema.object({ok:polly.schema.boolean()})});return [a,b]}});`
	report, err := r.RunWorkflow(context.Background(), source, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(report.Output)
	if requests.Load() != 2 || !strings.Contains(string(data), `"ok":true`) {
		t.Fatalf("not integrated: %s calls %d", data, requests.Load())
	}
	s, _ := r.State(context.Background())
	if len(s.Members) != 1 || len(s.Executions) != 2 {
		t.Fatalf("continuation started a new member: %+v", s)
	}
	for _, m := range s.Members {
		if m.Controller != "" {
			t.Fatal("completed workflow retained reservation")
		}
	}
}

func TestRestartResumesLogicalExecutionAndJournalsUncertainCalls(t *testing.T) {
	var calls atomic.Int32
	model := modelFunc(func(ctx context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		if calls.Add(1) == 1 {
			return messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: "park", Name: "wait_agent", Arguments: `{}`}}}
		}
		found := false
		for _, m := range req.Messages {
			if m.ToolCallID == "uncertain" && m.Content == llm.ToolInterruptedContent {
				found = true
			}
		}
		if !found {
			t.Error("uncertain tool intent was not recovered")
		}
		return answer("recovered")
	})
	r := runtimeTest(t, model, 1, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := r.Spawn(ctx, subagent.Request{Label: "Test agent", Task: "wait then recover", ReadOnly: true})
	if err != nil || !result.Yielded {
		t.Fatalf("%+v %v", result, err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.update(ctx, func(s *State) error {
		e := s.Executions[s.Members[result.Session].Execution]
		e.Intent = []messages.ChatMessage{{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: "uncertain", Name: "not_replayed", Arguments: `{}`}}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	recovered, err := New(r.config)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if err := recovered.Resume(ctx, result.Session, 0); err != nil {
		t.Fatal(err)
	}
	for {
		recovered.mu.Lock()
		i := recovered.active[result.Session]
		recovered.mu.Unlock()
		if i == nil {
			break
		}
		select {
		case <-i.done:
		case <-ctx.Done():
			t.Fatal("resume did not finish")
		}
	}
	s, err := recovered.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range s.Runs {
		if run.Starts != 1 {
			t.Fatalf("restart spent another start: %+v", run)
		}
	}
	e := s.Executions[s.Members[result.Session].Execution]
	if e.Status != "completed" || e.Iterations != 2 || len(e.Intent) != 0 {
		t.Fatalf("recovery state: %+v", e)
	}
	session, err := r.config.Store.Acquire(ctx, s.Members[result.Session].Name, sessions.AcquireOptions{ExistingOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	history, err := session.GetHistory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	briefs := 0
	for _, m := range history {
		if m.Role == messages.MessageRoleUser && strings.HasPrefix(m.Content, "wait then recover\n\nCompletion: ") {
			briefs++
		}
	}
	if briefs != 1 {
		t.Fatalf("assignment appended %d times", briefs)
	}
}

func TestSendNeverBlocksOnTheLaunchLock(t *testing.T) {
	var calls atomic.Int32
	model := modelFunc(func(ctx context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		calls.Add(1)
		return answer("done")
	})
	r := runtimeTest(t, model, 2, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := r.Spawn(ctx, subagent.Request{Label: "Test agent", Task: "idle afterwards", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	// A StopMember of another member, or a launch mid-checkout, holds launchMu.
	// The sender's tool goroutine must still finish its send.
	r.launchMu.Lock()
	sent := make(chan error, 1)
	go func() {
		_, err := r.Send(ctx, r.ID, result.Session, "request", "", "one more thing")
		sent <- err
	}()
	select {
	case err := <-sent:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		r.launchMu.Unlock()
		t.Fatal("send blocked behind the launch lock")
	}
	r.launchMu.Unlock()
	r.wakeIdleMember(result.Session)
	if calls.Load() != 1 {
		t.Fatal("ordinary send started an idle member")
	}
}

func TestParentWaitIgnoresAlreadyParkedMembers(t *testing.T) {
	var calls atomic.Int32
	model := modelFunc(func(ctx context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		if calls.Add(1) == 1 {
			return messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: "wait", Name: "wait_agent", Arguments: `{}`}}}
		}
		return answer("resumed and finished")
	})
	r := runtimeTest(t, model, 1, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := r.Spawn(ctx, subagent.Request{Label: "Test agent", Task: "park", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Yielded {
		t.Fatalf("member did not park: %+v", result)
	}
	// Nothing addressed to the parent has changed: its wait must block rather
	// than return on the member's steady parked state.
	short, stop := context.WithTimeout(ctx, 300*time.Millisecond)
	err = r.waitParent(short)
	stop()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("parent wait returned with nothing new: %v", err)
	}
	if _, err := r.Send(ctx, r.ID, result.Session, "request", "", "carry on"); err != nil {
		t.Fatal(err)
	}
	if err := r.waitParent(ctx); err != nil {
		t.Fatalf("parent wait missed the member finishing: %v", err)
	}
	select {
	case <-result.Done:
	case <-ctx.Done():
		t.Fatal("member never finished")
	}
}
