package swarm

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/subagent"
)

type activityModel struct{ release, sent chan struct{} }

func (m activityModel) ChatCompletionStream(ctx context.Context, _ *llm.CompletionRequest, p llm.EventStreamProcessor) <-chan *messages.StreamEvent {
	ch := make(chan messages.ChatMessage)
	core := streaming.NewStreamingCore(ctx, ch, nil)
	go func() {
		defer close(ch)
		core.EmitReasoning("eight bytes of thinking")
		close(m.sent)
		select {
		case <-ctx.Done():
			return
		case <-m.release:
		}
		core.EmitContent("complete")
		core.SetStopReason(messages.StopReasonEndTurn)
		core.CompleteStream()
	}()
	return p.ProcessMessagesToEvents(ctx, ch)
}

func TestLiveActivityDuringUnfinishedResponse(t *testing.T) {
	model := activityModel{make(chan struct{}), make(chan struct{})}
	r := runtimeTest(t, model, 1, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var observed atomic.Int32
	r.config.Callbacks = func(context.Context, Member) *llm.AgentCallbacks {
		return &llm.AgentCallbacks{OnStreamActivity: func(int, int) { observed.Add(1) }}
	}
	result, err := r.Spawn(ctx, subagent.Request{Label: "Thinking agent", Task: "work", ReadOnly: true, Background: true})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-model.sent:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var activity LiveActivity
	for {
		activity = r.LiveActivities()[result.Session]
		if activity.Phase == "thinking" {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("thinking activity not observed")
		}
		time.Sleep(time.Millisecond)
	}
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	e := s.Executions[s.Members[result.Session].Execution]
	if activity.Execution != e.ID || activity.Generation != e.Generation || activity.RequestStarted.IsZero() || activity.LastData.IsZero() || !activity.RequestActive || activity.StreamedBytes != len("eight bytes of thinking") || observed.Load() == 0 {
		t.Fatalf("unfinished stream lacks live activity: %+v, execution=%+v", activity, e)
	}
	close(model.release)
	awaitIdle(t, r, ctx)
	if len(r.LiveActivities()) != 0 {
		t.Fatal("completed stream retained live activity")
	}
}

func TestActivityRetryAndContinuationDiscardStaleData(t *testing.T) {
	i := &invocation{}
	cb := &llm.AgentCallbacks{}
	i.bindActivity(cb, time.Minute, time.Hour)
	cb.OnModelRequest(0, 0)
	cb.OnStreamActivity(0, 0)
	cb.OnReasoning("partial")
	cb.OnModelRequest(0, 1)
	cb.OnStreamActivity(0, 0) // a late notification from the failed attempt
	a := i.activity.snapshot
	if a.Attempt != 2 || !a.LastData.IsZero() || a.StreamedBytes != 0 || a.StallTimeout != time.Minute || a.Deadline != time.Hour {
		t.Fatalf("retry retained old activity: %+v", a)
	}
	next := &llm.AgentCallbacks{}
	i.bindActivity(next, 0, 0)
	next.OnModelRequest(0, 0)
	cb.OnStreamActivity(0, 1)
	cb.OnContent("late text from previous slice")
	a = i.activity.snapshot
	if !a.LastData.IsZero() || a.StreamedBytes != 0 || a.Phase != "requesting" {
		t.Fatalf("old slice changed new request: %+v", a)
	}
}

func TestUserStopPrecedesParentRecoveryAndSurvivesRestore(t *testing.T) {
	var r *Runtime
	var member string
	var calls atomic.Int32
	recovery := make(chan error, 1)
	model := modelFunc(func(ctx context.Context, _ *llm.CompletionRequest) messages.ChatMessage {
		if calls.Add(1) == 1 {
			<-ctx.Done()
			_, err := r.FollowupTask(context.Background(), member, "resume now", "parent-recovery")
			recovery <- err
		}
		return answer("done")
	})
	r = runtimeTest(t, model, 1, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := r.Spawn(ctx, subagent.Request{Label: "Worker", Task: "work", ReadOnly: true, Background: true})
	if err != nil {
		t.Fatal(err)
	}
	member = result.Session
	for calls.Load() == 0 {
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
		time.Sleep(time.Millisecond)
	}
	if err := r.StopMember(ctx, member); err != nil {
		t.Fatal(err)
	}
	if err := <-recovery; err == nil || !strings.Contains(err.Error(), "stopped by the user") {
		t.Fatalf("parent recovery bypassed stop: %v", err)
	}
	r = rebuildRuntime(t, r, nil)
	for _, refresh := range []bool{false, true} {
		if _, err := r.followupTask(ctx, member, "try again", "retry", refresh); err == nil || !strings.Contains(err.Error(), "stopped by the user") {
			t.Fatalf("refresh=%v bypassed saved stop: %v", refresh, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("stopped agent restarted: %d calls", calls.Load())
	}
	if err := r.Resume(ctx, member, 0); err != nil {
		t.Fatal(err)
	}
	awaitIdle(t, r, ctx)
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	e := s.Executions[s.Members[member].Execution]
	if calls.Load() != 2 || s.Members[member].Control != MemberControlEnabled || e.ResumedBy != "user" || e.ResumedAt.IsZero() {
		t.Fatalf("explicit user resume failed: %+v calls=%d", e, calls.Load())
	}
}

func TestNonUserCancellationsRemainResumableByParent(t *testing.T) {
	for _, source := range []string{"execution context", "interrupt_agent", "shutdown", "provider cancellation", "provider deadline", "provider silence"} {
		t.Run(source, func(t *testing.T) {
			var calls atomic.Int32
			started := make(chan struct{})
			model := modelFunc(func(ctx context.Context, _ *llm.CompletionRequest) messages.ChatMessage {
				if calls.Add(1) != 1 {
					return answer("recovered")
				}
				close(started)
				reply := answer("")
				switch source {
				case "provider cancellation":
					reply.SetError(context.Canceled)
				case "provider deadline":
					reply.SetError(&streaming.DeadlineError{Deadline: time.Second})
				case "provider silence":
					reply.SetError(&streaming.StallError{Timeout: time.Second})
				default:
					<-ctx.Done()
					reply.SetError(ctx.Err())
				}
				return reply
			})
			r := runtimeTest(t, model, 1, 4)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, err := r.Spawn(ctx, subagent.Request{Label: "Worker", Task: "work", ReadOnly: true, Background: true})
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			switch source {
			case "execution context":
				r.mu.Lock()
				r.active[result.Session].cancel()
				r.mu.Unlock()
			case "interrupt_agent":
				if _, err := r.InterruptAgent(ctx, result.Session); err != nil {
					t.Fatal(err)
				}
			case "shutdown":
				r = rebuildRuntime(t, r, nil)
			}
			awaitIdle(t, r, ctx)
			s, err := r.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if s.Members[result.Session].Control != MemberControlEnabled {
				t.Fatal("non-user cancellation became a user stop")
			}
			if _, err := r.FollowupTask(ctx, result.Session, "continue after cancellation", "recover"); err != nil {
				t.Fatal(err)
			}
			awaitIdle(t, r, ctx)
			s, err = r.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			e := s.Executions[s.Members[result.Session].Execution]
			if calls.Load() != 2 || e.Status != "completed" || e.ResumedBy != "parent" || e.ResumedAt.IsZero() {
				t.Fatalf("parent could not recover: calls=%d execution=%+v", calls.Load(), e)
			}
		})
	}
}
