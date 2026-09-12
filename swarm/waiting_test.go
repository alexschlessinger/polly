package swarm

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/subagent"
	"github.com/alexschlessinger/pollytool/tools"
)

// gatedParent holds the root session's coordination writes while a test
// inspects the state a member left behind. Reads and member sessions are
// never gated.
type gatedParent struct {
	sessions.Session
	coord sessions.CoordinationSession
	mu    sync.Mutex
	gate  chan struct{}
}

func newGatedParent(parent sessions.Session) *gatedParent {
	return &gatedParent{Session: parent, coord: parent.(sessions.CoordinationSession)}
}
func (g *gatedParent) ViewID() string { return g.coord.ViewID() }
func (g *gatedParent) ReadCoordination(ctx context.Context) (*sessions.CoordinationState, error) {
	return g.coord.ReadCoordination(ctx)
}
func (g *gatedParent) OpenPublishedArtifact(ctx context.Context, id string) (artifacts.Ref, io.ReadCloser, error) {
	return g.coord.OpenPublishedArtifact(ctx, id)
}
func (g *gatedParent) UpdateCoordination(ctx context.Context, fn func(*sessions.CoordinationState) error) error {
	g.mu.Lock()
	gate := g.gate
	g.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return g.coord.UpdateCoordination(ctx, fn)
}

// hold blocks parent writes until the returned release runs.
func (g *gatedParent) hold() (release func()) {
	ch := make(chan struct{})
	g.mu.Lock()
	g.gate = ch
	g.mu.Unlock()
	return sync.OnceFunc(func() {
		g.mu.Lock()
		g.gate = nil
		g.mu.Unlock()
		close(ch)
	})
}

func awaitState(t *testing.T, r *Runtime, ctx context.Context, ok func(*State) bool) *State {
	t.Helper()
	for {
		s, err := r.State(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if ok(s) {
			return s
		}
		select {
		case <-ctx.Done():
			t.Fatalf("state never settled: %v", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// A plain completion commits its final checkpoint as running; only finish
// records the outcome. Nothing in between may read as waiting.
func TestNormalFinalNeverWritesWaiting(t *testing.T) {
	var r *Runtime
	var gp *gatedParent
	type held struct {
		notify  chan struct{}
		release func()
	}
	committed := make(chan held, 1)
	model := modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
		// Hold finish; the member's own final checkpoint still commits and
		// wakes watchers, which is the moment the old code showed waiting.
		release := gp.hold()
		r.mu.Lock()
		committed <- held{r.notify, release}
		r.mu.Unlock()
		return answer("done")
	})
	r = runtimeTestWithParent(t, model, 1, 1, func(parent sessions.Session) sessions.Session {
		gp = newGatedParent(parent)
		return gp
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := r.Spawn(ctx, subagent.Request{Label: "Test agent", Task: "answer", ReadOnly: true, Background: true})
	if err != nil {
		t.Fatal(err)
	}
	var h held
	select {
	case h = <-committed:
	case <-ctx.Done():
		t.Fatal("model never called")
	}
	select {
	case <-h.notify:
	case <-ctx.Done():
		t.Fatal("final checkpoint never committed")
	}
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	e := s.Executions[s.Members[result.Session].Execution]
	if e == nil || e.Status != "running" {
		t.Fatalf("execution after the final checkpoint = %+v, want running", e)
	}
	h.release()
	select {
	case <-result.Done:
	case <-ctx.Done():
		t.Fatal("member never finished")
	}
	s = awaitState(t, r, ctx, func(s *State) bool { return s.Executions[e.ID].Status == "completed" })
	if p := MemberState(s, s.Members[result.Session]); p.Lifecycle != LifecycleIdle {
		t.Fatalf("member after completion: %+v", p)
	}
}

// A wake re-queues the parked execution: same ID and generation, queued while
// it waits for a slot, running once it has one, and no extra run start.
func TestYieldWakeReusesExecutionThroughQueued(t *testing.T) {
	var r *Runtime
	var calls atomic.Int32
	holderStarted := make(chan struct{})
	releaseHolder := make(chan struct{})
	model := modelFunc(func(ctx context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		switch calls.Add(1) {
		case 1:
			return messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: "ask", Name: "send_message", Arguments: tools.Result(map[string]any{"to": r.ID, "kind": "request", "text": "which option?"})}, {ID: "wait", Name: "swarm_wait", Arguments: `{}`}}}
		case 2:
			close(holderStarted)
			select {
			case <-releaseHolder:
			case <-ctx.Done():
			}
			return answer("held")
		default:
			return answer("resumed")
		}
	})
	r = runtimeTest(t, model, 1, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	parked, err := r.Spawn(ctx, subagent.Request{Label: "Test agent", Task: "ask parent", ReadOnly: true})
	if err != nil || !parked.Yielded {
		t.Fatalf("spawn: %+v %v", parked, err)
	}
	before := awaitState(t, r, ctx, func(s *State) bool {
		e := s.Executions[s.Members[parked.Session].Execution]
		return e != nil && e.Status == "waiting"
	})
	execution := *before.Executions[before.Members[parked.Session].Execution]
	holder, err := r.Spawn(ctx, subagent.Request{Label: "Test agent", Task: "hold the slot", ReadOnly: true, Background: true})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-holderStarted:
	case <-ctx.Done():
		t.Fatal("slot holder never started")
	}
	requests := inbox(before, r.ID, true)
	if len(requests) != 1 {
		t.Fatalf("requests %v", requests)
	}
	if _, err = r.Send(ctx, r.ID, parked.Session, "reply", requests[0].ID, "choose A"); err != nil {
		t.Fatal(err)
	}
	queued := awaitState(t, r, ctx, func(s *State) bool {
		return s.Executions[execution.ID].Status == "queued"
	})
	if e := queued.Executions[execution.ID]; e.Generation != execution.Generation || queued.Members[parked.Session].Execution != execution.ID {
		t.Fatalf("wake changed the execution: before=%+v after=%+v", execution, e)
	}
	close(releaseHolder)
	for _, done := range []<-chan struct{}{holder.Done, parked.Done} {
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal("members never finished")
		}
	}
	after := awaitState(t, r, ctx, func(s *State) bool { return s.Executions[execution.ID].Status == "completed" })
	e := after.Executions[execution.ID]
	if e.Generation != execution.Generation || e.Iterations != 2 {
		t.Fatalf("resumed execution %+v", e)
	}
	for _, run := range after.Runs {
		if run.Starts != 2 {
			t.Fatalf("wake spent a start: %+v", run)
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("model calls = %d, want 3", calls.Load())
	}
}
