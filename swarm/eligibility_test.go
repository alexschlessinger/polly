package swarm

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/subagent"
)

func TestWakeEligibilityTable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                        string
		control                     MemberControl
		controller, execution, kind string
		want                        bool
	}{
		{"completed with a request", "", "", "completed", "request", false},
		{"completed with a reply", "", "", "completed", "reply", false},
		{"never ran with a request", "", "", "", "request", false},
		{"informational mail", "", "", "completed", "info", false},
		{"stopped", MemberControlStopped, "", "completed", "request", false},
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

// A launch is refused while an execution is active, for a paused execution
// without an explicit resume, for a failed one without a resume or a retry
// by its own workflow, and for a stopped member without a resume.
func TestLaunchRefusalTable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, control, execution string
		resume, retry            bool
		want                     string
	}{
		{"idle", "", "", false, false, ""},
		{"completed", "", "completed", false, false, ""},
		{"running", "", "running", false, false, "already has an active execution"},
		{"running despite resume", "", "running", true, false, "already has an active execution"},
		{"paused", "", "paused", false, false, "member is paused"},
		{"paused retry", "", "paused", false, true, "member is paused"},
		{"paused resume", "", "paused", true, false, ""},
		{"failed", "", "failed", false, false, "last execution failed"},
		{"failed retry", "", "failed", false, true, ""},
		{"failed resume", "", "failed", true, false, ""},
		{"stopped", string(MemberControlStopped), "completed", false, false, "member is stopped"},
		{"stopped retry", string(MemberControlStopped), "failed", false, true, "member is stopped"},
		{"stopped resume", string(MemberControlStopped), "completed", true, false, ""},
	} {
		s, m := memberFixture(tc.control, tc.execution, "running", "")
		err := launchRefusal(s, m, tc.resume, tc.retry)
		if (err == nil) != (tc.want == "") || err != nil && !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: launchRefusal = %v, want %q", tc.name, err, tc.want)
		}
	}
	if err := launchRefusal(&State{}, nil, true, true); err == nil || !strings.Contains(err.Error(), "unknown member") {
		t.Errorf("nil member: %v", err)
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

// Neither information nor addressed requests restart an idle member.
func TestInformationalMailCannotRestartMember(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	r := runtimeTest(t, countingModel(&calls), 1, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := r.Spawn(ctx, subagent.Request{Label: "Test agent", Task: "work", ReadOnly: true, Review: true})
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
	if calls.Load() != 1 {
		t.Fatal("ordinary request started idle worker")
	}
	if _, err := r.FollowupTask(ctx, result.Session, "one more thing", ""); err != nil {
		t.Fatal(err)
	}
	awaitIdle(t, r, ctx)
	s = awaitState(t, r, ctx, func(s *State) bool { return len(s.Executions) == 2 })
	if calls.Load() != 2 || MemberState(s, s.Members[result.Session]).Lifecycle != LifecycleIdle {
		t.Fatalf("request did not wake the member once: calls=%d", calls.Load())
	}
}

func TestStopMemberIsIdempotent(t *testing.T) {
	t.Parallel()
	r := runtimeTest(t, idleModel(), 1, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := r.Spawn(ctx, subagent.Request{Label: "Test agent", Task: "work", ReadOnly: true, Review: true})
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

}

func TestStoppedMemberIsNotWokenAndResumeClearsStop(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	r := runtimeTest(t, countingModel(&calls), 1, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := r.Spawn(ctx, subagent.Request{Label: "Test agent", Task: "work", ReadOnly: true, Review: true})
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
