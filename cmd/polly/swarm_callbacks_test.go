package main

import (
	"context"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/swarm"
)

func TestMemberCallbacksRouteApprovalsWithoutAParentTurn(t *testing.T) {
	calls := []messages.ChatMessageToolCall{{ID: "1", Name: "bash"}}
	state := &conversationState{}
	member := swarm.Member{ID: "m1"}

	checkApproval := func(cb *llm.AgentCallbacks) []bool {
		t.Helper()
		if cb == nil || cb.ApproveToolCalls == nil {
			t.Fatal("missing approval callback")
		}
		decisions, err := cb.ApproveToolCalls(context.Background(), calls)
		if err != nil {
			t.Fatal(err)
		}
		return decisions
	}

	// No turn and no screen: --confirm refuses instead of auto-approving.
	cb := memberCallbacks(&Config{Confirm: true}, state)(context.Background(), member)
	if cb == nil || cb.ApproveToolCalls == nil || checkApproval(cb)[0] {
		t.Fatal("member with nobody to ask ran unattended under --confirm")
	}
	if memberCallbacks(&Config{}, state)(context.Background(), member) != nil {
		t.Fatal("without --confirm the configured auto-approval stands")
	}

	// The host screen takes the approval when a command or wake launched the member.
	screen := &denyingTurnUI{}
	state.setMemberUI(screen)
	cb = memberCallbacks(&Config{Confirm: true}, state)(context.Background(), member)
	if cb == nil || checkApproval(cb)[0] || screen.approvals != 1 {
		t.Fatalf("host screen not consulted: approvals=%d", screen.approvals)
	}

	// A running turn's UI serves as the fallback where no screen is bound.
	turn := &denyingTurnUI{}
	state.setMemberUI(nil)
	state.setTurnUI(turn)
	cb = memberCallbacks(&Config{Confirm: true}, state)(context.Background(), member)
	checkApproval(cb)
	if turn.approvals != 1 {
		t.Fatalf("turn UI not consulted: approvals=%d", turn.approvals)
	}

	// A parent turn stamped on the context still wins over both.
	parent := &denyingTurnUI{}
	cb = memberCallbacks(&Config{Confirm: true}, state)(withParentTurnUI(context.Background(), parent), member)
	checkApproval(cb)
	if parent.approvals != 1 || turn.approvals != 1 {
		t.Fatalf("parent turn UI bypassed: parent=%d turn=%d", parent.approvals, turn.approvals)
	}
}
