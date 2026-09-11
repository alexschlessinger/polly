package swarm

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/workflow"
)

func TestDeliveredRequestBlocksSettlementUntilReply(t *testing.T) {
	var r *Runtime
	var calls atomic.Int32
	model := modelFunc(func(ctx context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		if calls.Add(1) == 1 {
			return messages.ChatMessage{
				Role:       messages.MessageRoleAssistant,
				StopReason: messages.StopReasonToolUse,
				ToolCalls: []messages.ChatMessageToolCall{{
					ID: "ask", Name: "send_message",
					Arguments: tools.Result(map[string]any{"to": r.ID, "kind": "request", "text": "Should the next pass cover option A or B?"}),
				}},
			}
		}
		return answer("The current research is complete; the follow-up question still needs a reply.")
	})
	r = runtimeTest(t, model, 1, 5)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := r.Agent(ctx, "", AgentRequest{Task: "Research and ask a follow-up question", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	assertReplyBlock := func() {
		t.Helper()
		var blocked *workflow.Error
		if err := r.Settle(ctx); !errors.As(err, &blocked) || blocked.Code != "blocked" || blocked.Message != "a member is waiting for a parent reply" {
			t.Fatalf("unanswered parent request must block settlement: %v", err)
		}
	}
	assertReplyBlock()
	admitParent(t, r)
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var request *Mail
	for _, m := range s.Messages {
		if m.To == r.ID && m.Kind == "request" {
			request = m
		}
	}
	if request == nil || !request.Delivered || request.ReplyID != "" || s.Tasks[result.Task].Status != "done" {
		t.Fatalf("request=%+v task=%+v", request, s.Tasks[result.Task])
	}
	if hasWakeMail(s, r.ID) {
		t.Fatal("admitted request must not trigger another wake")
	}
	decisions := Present(s, r.ID, r.ID).Decisions
	if len(decisions) != 1 || decisions[0].Kind != KindMail || decisions[0].ID != request.ID {
		t.Fatalf("expected one reply decision, got %+v", decisions)
	}
	assertReplyBlock()

	// Keep the reply from starting an unrelated member continuation so this
	// assertion isolates the parent obligation, not the next execution's result.
	if err := r.StopMember(ctx, result.Session); err != nil {
		t.Fatal(err)
	}
	reply, err := r.Send(ctx, r.ID, result.Session, "reply", request.ID, "Use option A on the next pass.")
	if err != nil {
		t.Fatal(err)
	}
	s, err = r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.Messages[request.ID].ReplyID != reply.ID || s.Tasks[result.Task].Status != "done" {
		t.Fatalf("reply did not resolve the request independently of the completed task: %+v", s.Messages[request.ID])
	}
	if p := Present(s, r.ID, r.ID); len(p.Decisions) != 0 {
		t.Fatalf("answered request still needs a decision: %+v", p.Decisions)
	}
	if err := r.Settle(ctx); err != nil {
		t.Fatalf("answered request still blocks settlement: %v", err)
	}
}

func TestSettlementMailObligationsAreIndependentOfWake(t *testing.T) {
	cases := []struct {
		name                string
		mail                Mail
		wantBlock, wantWake bool
	}{
		{"new request", Mail{To: "parent", Kind: "request"}, true, true},
		{"admitted request", Mail{To: "parent", Kind: "request", Delivered: true}, true, false},
		{"answered request before admission", Mail{To: "parent", Kind: "request", ReplyID: "answer"}, false, true},
		{"answered request after admission", Mail{To: "parent", Kind: "request", ReplyID: "answer", Delivered: true}, false, false},
		{"unread reply", Mail{To: "parent", Kind: "reply", ReplyTo: "question"}, true, true},
		{"admitted reply", Mail{To: "parent", Kind: "reply", ReplyTo: "question", Delivered: true}, false, false},
		{"informational mail", Mail{To: "parent", Kind: "info"}, false, false},
		{"teammate request", Mail{To: "teammate", Kind: "request"}, false, false},
		{"teammate reply", Mail{To: "teammate", Kind: "reply"}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mail := tc.mail
			mail.ID, mail.From = "message", "worker"
			s := &State{Messages: map[string]*Mail{mail.ID: &mail}}
			f := deriveFacts(s, "parent")
			blockers := settlementBlockers(f)
			if got := len(blockers) != 0; got != tc.wantBlock {
				t.Fatalf("settlement blocked=%v, want %v: %+v", got, tc.wantBlock, blockers)
			}
			if tc.wantBlock && (len(blockers) != 1 || blockers[0].kind != KindMail) {
				t.Fatalf("expected only a mail obligation: %+v", blockers)
			}
			if got := hasWakeMail(s, "parent"); got != tc.wantWake {
				t.Fatalf("mail wake=%v, want %v", got, tc.wantWake)
			}
		})
	}
}
