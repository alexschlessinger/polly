package swarm

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

// A member that parks keeps its session's lease for parkedSessionHold, so a
// prompt wake needs no new one, and then gives it up; a later wake acquires
// the session again.
func TestParkedMemberGivesUpItsSessionAfterTheHold(t *testing.T) {
	t.Parallel()
	r := runtimeTest(t, modelFunc(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		for _, m := range req.Messages {
			if strings.HasPrefix(m.Content, "<peer_messages>") {
				return answer("woke")
			}
		}
		return iterationTool("wait", "wait_agent", `{}`)
	}), 1, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	i, err := r.start(ctx, "", AgentRequest{TaskName: "waiter", Label: "Waiter", Task: "wait", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	awaitState(t, r, ctx, func(s *State) bool { return s.Executions[i.id].Status == "waiting" })
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	name := s.Members[i.member].Name
	options := sessions.AcquireOptions{ExpectedID: i.member, ExistingOnly: true}
	// Acquire waits for a held lease, so a short wait fails while the member
	// holds it and a longer one succeeds once the hold has passed.
	brief, stop := context.WithTimeout(ctx, parkedSessionHold/5)
	held, err := r.config.Store.Acquire(brief, name, options)
	stop()
	if err == nil {
		held.Close()
		t.Fatal("a member that had just parked no longer held its session")
	}
	patient, stop := context.WithTimeout(ctx, parkedSessionHold+3*time.Second)
	session, err := r.config.Store.Acquire(patient, name, options)
	stop()
	if err != nil {
		t.Fatalf("a parked member still held its session after the hold: %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Send(ctx, r.ID, i.member, "info", "", "wake up"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-i.done:
	case <-ctx.Done():
		t.Fatal("the member did not finish after its session was released and it was woken")
	}
	if i.err != nil || i.result.Value != "woke" {
		t.Fatalf("result %v, error %v", i.result.Value, i.err)
	}
}
