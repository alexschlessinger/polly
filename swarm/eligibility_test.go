package swarm

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/subagent"
)

func TestWakeEligibilityTable(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		control                     MemberControl
		controller, execution, kind string
		want                        bool
	}{
		{"completed with a request", "", "", "completed", "request", true},
		{"completed with a reply", "", "", "completed", "reply", true},
		{"never ran with a request", "", "", "", "request", true},
		{"informational mail", "", "", "completed", "info", false},
		{"stopped", MemberControlStopped, "", "completed", "request", false},
		{"retired", MemberControlRetired, "", "completed", "request", false},
		{"reserved by a workflow", "", "wf", "completed", "request", false},
		{"paused execution", "", "", "paused", "request", false},
		{"failed execution", "", "", "failed", "request", false},
		{"parked execution", "", "", "waiting", "request", false},
	} {
		s, m := memberFixture(string(tc.control), tc.execution, "running", "")
		m.Controller = tc.controller
		s.Messages = map[string]*Mail{"m1": {ID: "m1", From: "parent", To: m.ID, Kind: tc.kind}}
		if got := wakeEligible(s, m); got != tc.want {
			t.Errorf("%s: wakeEligible = %v, want %v", tc.name, got, tc.want)
		}
	}
	if wakeEligible(&State{}, nil) {
		t.Error("nil member is wakeable")
	}
}

func countingModel(calls *atomic.Int32) llm.LLM {
	return modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
		calls.Add(1)
		return answer("done")
	})
}

func awaitIdle(t *testing.T, r *Runtime, ctx context.Context) {
	t.Helper()
	for r.HasActive() {
		select {
		case <-ctx.Done():
			t.Fatal("runtime never settled")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// Information never restarts a finished member; an addressed request does.
func TestInformationalMailCannotRestartMember(t *testing.T) {
	var calls atomic.Int32
	r := runtimeTest(t, countingModel(&calls), 1, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := r.Spawn(ctx, subagent.Request{Task: "work", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Send(ctx, r.ID, result.Session, "info", "", "for your information"); err != nil {
		t.Fatal(err)
	}
	r.wakeIdleMember(result.Session)
	awaitIdle(t, r, ctx)
	s, err := r.State(ctx)
	if err != nil || len(s.Executions) != 1 || calls.Load() != 1 {
		t.Fatalf("info mail restarted the member: executions=%d calls=%d err=%v", len(s.Executions), calls.Load(), err)
	}
	if _, err := r.Send(ctx, r.ID, result.Session, "request", "", "one more thing"); err != nil {
		t.Fatal(err)
	}
	r.wakeIdleMember(result.Session)
	awaitIdle(t, r, ctx)
	s = awaitState(t, r, ctx, func(s *State) bool { return len(s.Executions) == 2 })
	if calls.Load() != 2 || MemberState(s, s.Members[result.Session]).Lifecycle != LifecycleIdle {
		t.Fatalf("request did not wake the member once: calls=%d", calls.Load())
	}
}

func TestStopMemberRefusesRetiredAndIsIdempotent(t *testing.T) {
	r := runtimeTest(t, idleModel(), 1, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := r.Spawn(ctx, subagent.Request{Task: "work", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := r.StopMember(ctx, result.Session); err != nil {
			t.Fatal(err)
		}
	}
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p := MemberState(s, s.Members[result.Session]); p.Control != MemberControlStopped || p.Display != "paused · stopped · awaiting review" {
		t.Fatalf("stopped member: %+v", p)
	}
	if err := r.update(ctx, func(s *State) error {
		s.Members[result.Session].Control = MemberControlRetired
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.StopMember(ctx, result.Session); err == nil {
		t.Fatal("stop overwrote a retirement")
	}
	s, _ = r.State(ctx)
	if p := MemberState(s, s.Members[result.Session]); p.Control != MemberControlRetired || p.Display != "idle · retired · awaiting review" {
		t.Fatalf("retired member after stop: %+v", p)
	}
}

func TestStoppedMemberIsNotWokenAndResumeClearsStop(t *testing.T) {
	var calls atomic.Int32
	r := runtimeTest(t, countingModel(&calls), 1, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := r.Spawn(ctx, subagent.Request{Task: "work", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.StopMember(ctx, result.Session); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Send(ctx, r.ID, result.Session, "request", "", "please continue"); err != nil {
		t.Fatal(err)
	}
	r.wakeIdleMember(result.Session)
	awaitIdle(t, r, ctx)
	if s, _ := r.State(ctx); len(s.Executions) != 1 || calls.Load() != 1 {
		t.Fatalf("stopped member was woken by mail: executions=%d calls=%d", len(s.Executions), calls.Load())
	}
	if err := r.Resume(ctx, result.Session, 0); err != nil {
		t.Fatal(err)
	}
	awaitIdle(t, r, ctx)
	s := awaitState(t, r, ctx, func(s *State) bool { return len(s.Executions) == 2 })
	if p := MemberState(s, s.Members[result.Session]); p.Control != MemberControlEnabled || calls.Load() != 2 || p.Lifecycle != LifecycleIdle {
		t.Fatalf("resume did not clear the stop: %+v calls=%d", p, calls.Load())
	}
}
