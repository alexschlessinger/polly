package swarm

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

func TestPublicationWakesWaitingRunTeammates(t *testing.T) {
	t.Parallel()
	const broadcast = "one broadcast for four waiting receivers"
	receivers := []string{"receiver_a", "receiver_b", "receiver_c", "receiver_d"}
	model := modelFunc(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		var task string
		var peer strings.Builder
		called := map[string]bool{}
		waits := 0
		for _, m := range req.Messages {
			switch {
			case m.Role == messages.MessageRoleUser && strings.HasPrefix(m.Content, "<peer_messages>"):
				peer.WriteString(m.Content)
			case m.Role == messages.MessageRoleUser && task == "":
				task = strings.SplitN(m.Content, "\n\nCompletion: ", 2)[0]
			case m.Role == messages.MessageRoleTool:
				called[m.ToolName] = true
				if m.ToolName == "wait_agent" {
					waits++
				}
			}
		}
		if task == "publisher" {
			if !called["swarm_publish"] {
				return iterationTool("publish", "swarm_publish", tools.Result(map[string]any{"text": broadcast}))
			}
			if strings.Contains(peer.String(), broadcast) {
				t.Error("publisher received its own broadcast")
			}
			for _, name := range receivers {
				if !strings.Contains(peer.String(), "ACK "+name) {
					return iterationTool(fmt.Sprintf("wait-%d", waits), "wait_agent", `{}`)
				}
			}
			return answer("all four acknowledged")
		}
		if !called["wait_agent"] {
			return iterationTool("wait", "wait_agent", `{}`)
		}
		if n := strings.Count(peer.String(), broadcast); n != 1 {
			t.Errorf("%s received the broadcast %d times, want once", task, n)
			return answer("missing or duplicate broadcast")
		}
		if !called["send_message"] {
			return iterationTool("ack", "send_message", tools.Result(map[string]any{"target": "/root/publisher", "message": "ACK " + task}))
		}
		return answer("received")
	})
	// A single slot proves that parked receivers yield capacity to the publisher.
	r := runtimeTest(t, model, 1, 5)
	r.UpdateDefaults(r.config.Request, llm.AgentConfig{MaxIterations: 10}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var readers []*invocation
	for _, name := range receivers {
		i, err := r.start(ctx, "", AgentRequest{TaskName: name, Label: name, Task: name, ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		awaitState(t, r, ctx, func(s *State) bool { return s.Executions[i.id].Status == "waiting" && len(r.slots) == 0 })
		readers = append(readers, i)
	}
	publisher, err := r.start(ctx, "", AgentRequest{TaskName: "publisher", Label: "Publisher", Task: "publisher", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range append(readers, publisher) {
		select {
		case <-i.done:
			if i.err != nil {
				t.Fatal(i.err)
			}
		case <-ctx.Done():
			t.Fatal("one publication did not wake all four receivers and collect their ACKs")
		}
	}
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Publications) != 1 || len(s.Executions) != 5 {
		t.Fatalf("broadcast created %d publications and %d executions, want one and five", len(s.Publications), len(s.Executions))
	}
	var pub *Publication
	for _, p := range s.Publications {
		pub = p
	}
	if pub.Author != publisher.member || pub.Text != broadcast || len(publicationReaders(s, pub)) != 4 {
		t.Fatalf("broadcast author, text or readership changed: %+v", pub)
	}
	for _, i := range readers {
		m := s.Members[i.member]
		e := s.Executions[i.id]
		if m.Execution != i.id || e.Generation != 1 || e.Status != "completed" || m.Publications.ID != pub.ID {
			t.Fatalf("receiver did not resume its original execution and save the receipt: member=%+v execution=%+v", m, e)
		}
		acks := 0
		for _, mail := range s.Messages {
			if mail.From == r.ID && mail.To == i.member {
				t.Fatal("receiver needed a parent message to wake")
			}
			if mail.From == i.member && mail.To == publisher.member {
				acks++
				if !mail.Delivered || mail.Start {
					t.Fatalf("ACK was not admitted as ordinary peer mail: %+v", mail)
				}
			}
		}
		if acks != 1 {
			t.Fatalf("receiver sent %d ACKs, want one", acks)
		}
	}
	if !s.Members[publisher.member].Publications.IsZero() {
		t.Fatal("publisher acquired a receipt for its own publication")
	}
	if run := s.Runs[pub.Run]; run.Starts != 5 {
		t.Fatalf("publication wake spent another logical execution: %+v", run)
	}
}

func TestPublicationArrivingBeforeParkIsNotLost(t *testing.T) {
	t.Parallel()
	const broadcast = "published before the wait loop subscribes"
	var calls atomic.Int32
	r := runtimeTest(t, modelFunc(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		if calls.Add(1) == 1 {
			return iterationTool("wait", "wait_agent", `{}`)
		}
		seen := 0
		for _, m := range req.Messages {
			if strings.HasPrefix(m.Content, "<peer_messages>") {
				seen += strings.Count(m.Content, broadcast)
			}
		}
		if seen != 1 {
			t.Errorf("resumed reader saw the publication %d times", seen)
		}
		return answer("received")
	}), 1, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	waiting, published := make(chan struct{}), make(chan struct{})
	r.config.OnEvent = func(e Event) {
		if e.Kind == "waiting" {
			close(waiting)
			select {
			case <-published:
			case <-ctx.Done():
			}
		}
	}
	i, err := r.start(ctx, "", AgentRequest{TaskName: "reader", Label: "Reader", Task: "wait", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-waiting:
	case <-ctx.Done():
		t.Fatal("reader never reached the parking boundary")
	}
	if _, err := r.Publish(ctx, r.ID, Publication{Text: broadcast}); err != nil {
		t.Fatal(err)
	}
	close(published)
	select {
	case <-i.done:
		if i.err != nil || calls.Load() != 2 {
			t.Fatalf("publication did not resume the same execution once: calls=%d err=%v", calls.Load(), i.err)
		}
	case <-ctx.Done():
		t.Fatal("publication arriving before parking was lost")
	}
}

func TestPublicationWakeRespectsExecutionVisibility(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		own        bool
		read       bool
		otherRun   bool
		host       bool
		shouldWake bool
	}{
		{name: "same-run finding", shouldWake: true},
		{name: "self-authored", own: true},
		{name: "already read", read: true},
		{name: "other-run finding", otherRun: true},
		{name: "other-run host fact", otherRun: true, host: true, shouldWake: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			resumed := make(chan struct{})
			r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
				if calls.Add(1) == 1 {
					return iterationTool("wait", "wait_agent", `{}`)
				}
				close(resumed)
				return answer("received")
			}), 1, 1)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			i, err := r.start(ctx, "", AgentRequest{TaskName: "reader", Label: "Reader", Task: "wait", ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			awaitState(t, r, ctx, func(s *State) bool { return s.Executions[i.id].Status == "waiting" && len(r.slots) == 0 })
			// Seed the publication and notify in one transaction. Changing the
			// current run also ensures wakeup uses this execution's original run.
			if err := r.update(ctx, func(s *State) error {
				run := s.Executions[i.id].Run
				s.Runs[run].Status = "completed"
				s.Runs["later-run"] = &Run{ID: "later-run", Status: "running", Limit: 1}
				p := &Publication{ID: "publication", Author: r.ID, Run: run, Text: "visible input", Posted: time.Now().UTC()}
				if tc.own {
					p.Author = i.member
				}
				if tc.otherRun {
					p.Run = "later-run"
				}
				if tc.host {
					p.Kind = PublicationKindHost
				}
				s.Publications[p.ID] = p
				if tc.read {
					return advancePublicationMark(s, i.member, p)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if tc.shouldWake {
				select {
				case <-i.done:
					if i.err != nil || calls.Load() != 2 {
						t.Fatalf("visible publication failed to resume reader: calls=%d err=%v", calls.Load(), i.err)
					}
				case <-ctx.Done():
					t.Fatal("visible publication did not wake reader")
				}
			} else {
				select {
				case <-resumed:
					t.Fatal("invisible or already-read publication woke reader")
				case <-time.After(100 * time.Millisecond):
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				s, err := r.State(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if e := s.Executions[i.id]; e.Status != "waiting" || calls.Load() != 1 {
					t.Fatalf("reader did not remain parked: execution=%+v calls=%d", e, calls.Load())
				}
			}
		})
	}
}

func TestPublishNotifiesReadersAfterCommit(t *testing.T) {
	t.Parallel()
	r := runtimeTest(t, nilModel(), 1, 1)
	ctx := context.Background()
	if err := r.prepare(ctx); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"invalid", PublicationKindFinding} {
		r.mu.Lock()
		notify := r.notify
		r.mu.Unlock()
		_, err := r.Publish(ctx, r.ID, Publication{Text: "new input", Kind: kind})
		if (err == nil) != (kind == PublicationKindFinding) {
			t.Fatalf("publish %q: %v", kind, err)
		}
		select {
		case <-notify:
			if err != nil {
				t.Fatal("failed publication notified readers")
			}
		default:
			if err == nil {
				t.Fatal("committed publication did not notify parked readers")
			}
		}
	}
}
