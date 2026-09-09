package main

import (
	"context"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/swarm"
)

func TestMemberCallbacksRouteApprovalsWithoutAParentTurn(t *testing.T) {
	calls := []messages.ChatMessageToolCall{{ID: "1", Name: "bash"}}
	state := &conversationState{}
	member := swarm.Member{ID: "m1"}

	// No turn and no screen: --confirm refuses instead of auto-approving.
	cb := memberCallbacks(&Config{Confirm: true}, state)(context.Background(), member)
	if cb == nil || cb.ApproveToolCalls == nil || cb.ApproveToolCalls(calls)[0] {
		t.Fatal("member with nobody to ask ran unattended under --confirm")
	}
	if memberCallbacks(&Config{}, state)(context.Background(), member) != nil {
		t.Fatal("without --confirm the configured auto-approval stands")
	}

	// The host screen takes the approval when a command or wake launched the member.
	screen := &denyingTurnUI{}
	state.setMemberUI(screen)
	cb = memberCallbacks(&Config{Confirm: true}, state)(context.Background(), member)
	if cb == nil || cb.ApproveToolCalls(calls)[0] || screen.approvals != 1 {
		t.Fatalf("host screen not consulted: approvals=%d", screen.approvals)
	}

	// A running turn's UI serves as the fallback where no screen is bound.
	turn := &denyingTurnUI{}
	state.setMemberUI(nil)
	state.setTurnUI(turn)
	cb = memberCallbacks(&Config{Confirm: true}, state)(context.Background(), member)
	cb.ApproveToolCalls(calls)
	if turn.approvals != 1 {
		t.Fatalf("turn UI not consulted: approvals=%d", turn.approvals)
	}

	// A parent turn stamped on the context still wins over both.
	parent := &denyingTurnUI{}
	cb = memberCallbacks(&Config{Confirm: true}, state)(withParentTurnUI(context.Background(), parent), member)
	cb.ApproveToolCalls(calls)
	if parent.approvals != 1 || turn.approvals != 1 {
		t.Fatalf("parent turn UI bypassed: parent=%d turn=%d", parent.approvals, turn.approvals)
	}
}
