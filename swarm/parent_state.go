package swarm

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
)

// parentTracker follows the root session's own turn in memory. Nothing about
// the parent persists: an archived view omits parent activity rather than
// infer it.
type parentTracker struct {
	mu       sync.Mutex
	running  bool
	turn     uint64
	inflight map[string]*inflightCall // this turn's tool calls, by call ID
	settling bool                     // inside Settle after a provisional final
	outcome  Lifecycle                // "" or idle or paused: state after the last turn
	detail   string
}

// inflightCall counts the coordination waits a tool call is inside and the
// other operations it is running; a workflow call can await several agents
// while a parallel step still executes a tool.
type inflightCall struct{ waits, work int }

func (t *parentTracker) begin() (uint64, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.running {
		return 0, errors.New("parent turn already running")
	}
	t.running, t.turn = true, t.turn+1
	t.inflight, t.settling = map[string]*inflightCall{}, false
	t.outcome, t.detail = LifecycleIdle, ""
	return t.turn, nil
}

func (t *parentTracker) callStart(turn uint64, id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.running && turn == t.turn && id != "" {
		t.inflight[id] = &inflightCall{}
	}
}

// callEnd also fires for denial stubs that never started: a missing key is
// not an error.
func (t *parentTracker) callEnd(turn uint64, id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.running && turn == t.turn {
		delete(t.inflight, id)
	}
}

func (t *parentTracker) setSettling(on bool) {
	t.mu.Lock()
	t.settling = on
	t.mu.Unlock()
}

// beginWait marks a coordination wait inside a known tool call. A wait with
// no live call (outside a parent turn, or a background workflow's agent
// await) marks nothing rather than fabricate an in-flight call.
func (t *parentTracker) beginWait(id string) (end func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	call := t.inflight[id]
	if !t.running || id == "" || call == nil {
		return func() {}
	}
	turn := t.turn
	call.waits++
	return func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.running && t.turn == turn {
			if call := t.inflight[id]; call != nil {
				call.waits--
			}
		}
	}
}

// beginWork marks an operation inside a known tool call that is not a
// coordination wait, such as a workflow step executing a tool; while any is
// running the call is active even if a sibling step awaits an agent.
func (t *parentTracker) beginWork(id string) (end func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	call := t.inflight[id]
	if !t.running || id == "" || call == nil {
		return func() {}
	}
	turn := t.turn
	call.work++
	return func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.running && t.turn == turn {
			if call := t.inflight[id]; call != nil {
				call.work--
			}
		}
	}
}

func (t *parentTracker) finish(turn uint64, lifecycle Lifecycle, detail string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if turn != t.turn {
		return
	}
	t.running, t.settling, t.inflight = false, false, nil
	t.outcome, t.detail = lifecycle, detail
}

func (t *parentTracker) settled(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.running && err != nil && t.outcome == LifecycleIdle {
		t.outcome, t.detail = LifecyclePaused, "error"
	}
}

// snapshot reports waiting only when every remaining operation of the turn
// is a coordination wait; concurrent model or tool work keeps it active,
// including work inside a call that is also awaiting an agent.
func (t *parentTracker) snapshot() (running, waiting bool, outcome Lifecycle, detail string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	waiting = t.settling
	if !waiting && len(t.inflight) > 0 {
		waiting = true
		for _, call := range t.inflight {
			if call.waits == 0 || call.work > 0 {
				waiting = false
				break
			}
		}
	}
	return t.running, waiting, t.outcome, t.detail
}

// errSettlementBlocked marks a parent turn that ended on an unresolved
// settlement blocker after the coordination nudge changed nothing.
var errSettlementBlocked = errors.New("parent turn ended on an unresolved settlement blocker")

type settlementBlockedError struct{ cause error }

func (e *settlementBlockedError) Error() string        { return e.cause.Error() }
func (e *settlementBlockedError) Unwrap() error        { return e.cause }
func (e *settlementBlockedError) Is(target error) bool { return target == errSettlementBlocked }

// RunParent runs one parent turn through the runtime. It binds mail
// admission, progressive persistence and settlement to a copy of the caller's
// callbacks, follows the turn's lifecycle, and records the outcome on every
// return, including checkpoint and projection failures. The host reports the
// verdict of its own later stages through ParentTurnSettled.
func (r *Runtime) RunParent(ctx context.Context, agent *llm.Agent, req *llm.CompletionRequest, cb *llm.AgentCallbacks, allowed func() bool) (*llm.AgentResponse, error) {
	turn, err := r.parentTurn.begin()
	if err != nil {
		return nil, err
	}
	local := llm.AgentCallbacks{}
	if cb != nil {
		local = *cb
	}
	r.bindParent(&local, allowed)
	priorContext := local.BeforeToolExecute
	local.BeforeToolExecute = func(ctx context.Context, call messages.ChatMessageToolCall, args map[string]any) context.Context {
		r.parentTurn.callStart(turn, call.ID)
		return priorContext(ctx, call, args)
	}
	priorEnd := local.OnToolEnd
	local.OnToolEnd = func(call messages.ChatMessageToolCall, result string, duration time.Duration, err error) {
		r.parentTurn.callEnd(turn, call.ID)
		if priorEnd != nil {
			priorEnd(call, result, duration, err)
		}
	}
	resp, runErr := agent.Run(ctx, req, &local)
	lifecycle, detail := parentOutcome(ctx, runErr)
	r.parentTurn.finish(turn, lifecycle, detail)
	return resp, runErr
}

func parentOutcome(ctx context.Context, err error) (Lifecycle, string) {
	switch {
	case err == nil:
		return LifecycleIdle, ""
	case ctx.Err() != nil, errors.Is(err, context.Canceled):
		return LifecyclePaused, "interrupted"
	case llm.IsIterationLimit(err):
		return LifecyclePaused, "iteration limit"
	case errors.Is(err, errSettlementBlocked):
		return LifecyclePaused, "blocked"
	}
	return LifecyclePaused, "error"
}

// ParentTurnSettled records the host's final verdict on the last parent turn
// once persistence and output settled. A failure there turns idle into
// paused · error; the next RunParent starts fresh.
func (r *Runtime) ParentTurnSettled(err error) { r.parentTurn.settled(err) }

// ParentState presents the root session's own lifecycle. s supplies the idle
// detail from the swarm's task disposition and may be nil.
func (r *Runtime) ParentState(s *State) AgentPresentation {
	running, waiting, outcome, detail := r.parentTurn.snapshot()
	p := AgentPresentation{}
	switch {
	case running && waiting:
		p.Lifecycle = LifecycleWaiting
	case running:
		p.Lifecycle = LifecycleActive
	case outcome == "":
		p.Lifecycle = LifecycleIdle
	default:
		p.Lifecycle, p.Detail = outcome, detail
	}
	if p.Lifecycle == LifecycleIdle && s != nil {
		p.Detail = parentDisposition(s)
	}
	p.Busy = p.Lifecycle == LifecycleActive || p.Lifecycle == LifecycleWaiting
	p.Display = DisplayLabel(p.Lifecycle, p.Detail, false)
	return p
}

// parentDisposition names what the swarm needs from an idle parent.
func parentDisposition(s *State) string {
	review, integration, halted, open, delivering := false, false, false, false, false
	for _, t := range s.Tasks {
		if TaskDeferred(s, t) {
			continue
		}
		switch {
		case deliveringTask(s, t):
			delivering = true
		case TaskStatusIn(s, t) == "integration halted":
			halted = true
		case TaskStatusIn(s, t) == "integration pending":
			integration = true
		case t.Status == "awaiting_review":
			review = true
		case t.Status != "done" && t.Status != "canceled":
			open = true
		}
	}
	for _, w := range s.Workflows {
		if w.Status == "completed" && !w.Acknowledged {
			delivering = true
		}
	}
	switch {
	case halted:
		return "integration halted"
	case review:
		return "awaiting review"
	case integration:
		return "integration pending"
	case delivering:
		return "delivering"
	case len(s.Runs) > 0 && !open:
		return "done"
	}
	return ""
}
