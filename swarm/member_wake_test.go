package swarm

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestQueuedPeerWakeRechecksPendingMailUnderLaunchLock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var calls atomic.Int32
	r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
		calls.Add(1)
		return answer("done")
	}), 1, 5)
	initial, err := r.Agent(ctx, "", AgentRequest{Task: "initial task", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	// An explicit parent launch can own launchMu while a peer notification
	// queues a wake behind it. That launch may consume all pending input.
	r.launchMu.Lock()
	if _, err := r.Send(ctx, r.ID, initial.Session, "request", "", "Please reply once"); err != nil {
		r.launchMu.Unlock()
		t.Fatal(err)
	}
	service, err := r.startLocked(ctx, "", AgentRequest{Session: initial.Session, Task: "Handle pending messages"})
	if err != nil {
		r.launchMu.Unlock()
		t.Fatal(err)
	}
	select {
	case <-service.done:
	case <-ctx.Done():
		r.launchMu.Unlock()
		t.Fatal(ctx.Err())
	}
	s, err := r.State(ctx)
	if err != nil {
		r.launchMu.Unlock()
		t.Fatal(err)
	}
	if hasWakeMail(s, initial.Session) {
		r.launchMu.Unlock()
		t.Fatal("service did not admit the request")
	}
	task := s.Tasks[s.Members[initial.Session].Task]
	if err := r.Review(ctx, task.ID, task.Revision, true, ""); err != nil {
		r.launchMu.Unlock()
		t.Fatal(err)
	}
	r.launchMu.Unlock()
	// Exercise a delayed wake synchronously as well, so the assertion does
	// not depend on how quickly the queued goroutine is scheduled.
	r.wakeIdleMember(initial.Session)
	s, err = r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || len(s.Executions) != 2 || s.Tasks[task.ID].Status != "done" || s.Tasks[task.ID].AcceptedRevision != task.Revision {
		t.Fatalf("stale notification spent a start or reopened accepted work: calls=%d executions=%d task=%+v", calls.Load(), len(s.Executions), s.Tasks[task.ID])
	}
}
