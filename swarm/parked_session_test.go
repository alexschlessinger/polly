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

// A parked member has no handle on its session: when the runtime says the
// member is waiting, the store reports the session free. Another may take
// it, and the member takes it again for the slice that follows its wake.
func TestParkedMemberLeavesItsSessionToBeTaken(t *testing.T) {
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
	// The runtime says a member is waiting once the slice that parked it
	// has returned.
	inUse := make(chan bool, 1)
	r.config.OnEvent = func(e Event) {
		if e.Kind != "waiting" {
			return
		}
		summaries, err := r.config.Store.ListSummaries(ctx)
		if err != nil {
			t.Error(err)
		}
		for _, summary := range summaries {
			if summary.ID == e.Member {
				select {
				case inUse <- summary.InUse:
				default:
				}
			}
		}
	}
	i, err := r.start(ctx, "", AgentRequest{TaskName: "waiter", Label: "Waiter", Task: "wait", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case held := <-inUse:
		if held {
			t.Fatal("a member that had just parked still had its session open")
		}
	case <-ctx.Done():
		t.Fatal("the member never parked")
	}
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	name := s.Members[i.member].Name
	session, err := r.config.Store.Acquire(ctx, name, sessions.AcquireOptions{ExpectedID: i.member, ExistingOnly: true})
	if err != nil {
		t.Fatalf("a parked member's session could not be taken: %v", err)
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
		t.Fatal("the member did not finish after its session was taken and given back")
	}
	if i.err != nil || i.result.Value != "woke" {
		t.Fatalf("result %v, error %v", i.result.Value, i.err)
	}
}
