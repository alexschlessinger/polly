package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/swarm"
	ui "github.com/metaspartan/gotui/v5"
)

func waitApprovalQueue(t *testing.T, m *replModel, active string, queued int) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		m.mu.Lock()
		ok := m.approval != nil && m.approval.requester == active && len(m.approvalQueue) == queued
		m.mu.Unlock()
		if ok {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("approval queue did not reach active=%q queued=%d", active, queued)
		case <-time.After(time.Millisecond):
		}
	}
}

func startMemberApproval(r *managedREPL, ctx context.Context, id string) <-chan []bool {
	result := make(chan []bool, 1)
	cb := memberCallbacks(r.config, r.state)(ctx, swarm.Member{ID: id})
	go func() { result <- cb.ApproveToolCalls([]messages.ChatMessageToolCall{{ID: id, Name: "bash"}}) }()
	return result
}

func assertApprovalResult(t *testing.T, result <-chan []bool, want bool) {
	t.Helper()
	select {
	case got := <-result:
		if len(got) != 1 || got[0] != want {
			t.Fatalf("approval=%v want [%v]", got, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("approval did not return")
	}
}

func TestConcurrentMemberApprovalsAreQueued(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "parent-work")
	r.config.Confirm = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := startMemberApproval(r, ctx, "one")
	waitApprovalQueue(t, r.model, "one", 0)
	second := startMemberApproval(r, ctx, "two")
	waitApprovalQueue(t, r.model, "one", 1)
	r.model.mu.Lock()
	if !strings.Contains(r.model.approvalPrompt(120), "one") {
		t.Error("approval does not identify its member")
	}
	r.model.handleApprovalAnswer('y')
	r.model.mu.Unlock()
	assertApprovalResult(t, first, true)
	waitApprovalQueue(t, r.model, "two", 0)
	r.model.mu.Lock()
	r.model.handleApprovalAnswer('n')
	r.model.mu.Unlock()
	assertApprovalResult(t, second, false)
}

func TestMemberApprovalCancellationRemovesOnlyItsRequest(t *testing.T) {
	for _, cancelActive := range []bool{false, true} {
		t.Run(map[bool]string{false: "queued", true: "active"}[cancelActive], func(t *testing.T) {
			r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "parent-work")
			r.config.Confirm = true
			firstCtx, cancelFirst := context.WithCancel(context.Background())
			defer cancelFirst()
			secondCtx, cancelSecond := context.WithCancel(context.Background())
			defer cancelSecond()
			first := startMemberApproval(r, firstCtx, "one")
			waitApprovalQueue(t, r.model, "one", 0)
			second := startMemberApproval(r, secondCtx, "two")
			waitApprovalQueue(t, r.model, "one", 1)
			if cancelActive {
				cancelFirst()
				assertApprovalResult(t, first, false)
				waitApprovalQueue(t, r.model, "two", 0)
				r.model.mu.Lock()
				r.model.handleApprovalAnswer('y')
				r.model.mu.Unlock()
				assertApprovalResult(t, second, true)
			} else {
				cancelSecond()
				assertApprovalResult(t, second, false)
				waitApprovalQueue(t, r.model, "one", 0)
				r.model.mu.Lock()
				r.model.handleApprovalAnswer('y')
				r.model.mu.Unlock()
				assertApprovalResult(t, first, true)
			}
		})
	}
}

func TestReleaseApprovalsClosesQueueAndRefusesLateRequests(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "parent-work")
	r.config.Confirm = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := startMemberApproval(r, ctx, "one")
	waitApprovalQueue(t, r.model, "one", 0)
	second := startMemberApproval(r, ctx, "two")
	waitApprovalQueue(t, r.model, "one", 1)
	r.releaseApprovals()
	assertApprovalResult(t, first, false)
	assertApprovalResult(t, second, false)
	assertApprovalResult(t, startMemberApproval(r, ctx, "late"), false)
	r.model.mu.Lock()
	defer r.model.mu.Unlock()
	if r.model.approval != nil || len(r.model.approvalQueue) != 0 {
		t.Fatal("shutdown retained approvals")
	}
}

func TestCanceledQueuedApprovalCannotBeGranted(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "parent-work")
	r.config.Confirm = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := &approvalState{ctx: ctx, calls: []messages.ChatMessageToolCall{{ID: "one"}}, reply: make(chan []bool, 1)}
	b := &approvalState{calls: []messages.ChatMessageToolCall{{ID: "two"}}, reply: make(chan []bool, 1)}
	r.model.mu.Lock()
	r.model.approval = a
	r.model.approvalQueue = []*approvalState{b}
	cancel()
	r.model.handleApprovalAnswer('y')
	r.model.resolveApprovalLocked(a, []bool{false}) // Late cancellation is harmless.
	r.model.handleApprovalAnswer('y')
	r.model.mu.Unlock()
	assertApprovalResult(t, a.reply, false)
	assertApprovalResult(t, b.reply, true)
}

func TestRuntimeStopReleasesMemberApproval(t *testing.T) {
	for _, action := range []string{"stop", "close"} {
		t.Run(action, func(t *testing.T) {
			r := newSwarmTestREPL(t, integrationModel(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
				return spawnTestToolCall("list_agents", `{}`)
			}), nil)
			r.config.Confirm = true
			defer r.releaseApprovals()
			r.runTabCommand("/spawn --read-only inspect")
			runUITask(t, r)
			deadline := time.After(3 * time.Second)
			var id string
			for id == "" {
				r.model.mu.Lock()
				if r.model.approval != nil {
					id = r.model.approval.requester
				}
				r.model.mu.Unlock()
				select {
				case <-deadline:
					t.Fatal("member did not request approval")
				default:
				}
				if id == "" {
					time.Sleep(time.Millisecond)
				}
			}
			done := make(chan error, 1)
			go func() {
				if action == "stop" {
					done <- r.state.swarm.StopMember(context.Background(), id)
				} else {
					done <- r.state.swarm.Close()
				}
			}()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("runtime blocked on approval")
			}
			waitSwarmIdle(t, r.state.swarm)
			r.model.mu.Lock()
			defer r.model.mu.Unlock()
			if r.model.approval != nil || len(r.model.approvalQueue) != 0 {
				t.Fatal("stopped member retained approval")
			}
		})
	}
}

func TestParentAndMemberApprovalsStayOnHiddenWorkspace(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "parent-work", "other-work")
	r.config.Confirm = true
	parent := r.tabs[0]
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cb := memberCallbacks(r.config, parent.state)(ctx, swarm.Member{ID: "member"})
	member := make(chan []bool, 1)
	go func() { member <- cb.ApproveToolCalls([]messages.ChatMessageToolCall{{ID: "member", Name: "bash"}}) }()
	waitApprovalQueue(t, parent.model, "member", 0)
	result := make(chan []bool, 1)
	go func() {
		result <- approveToolCalls(ctx, parent.state.hostTurnUI(), "", []messages.ChatMessageToolCall{{ID: "parent", Name: "bash"}})
	}()
	waitApprovalQueue(t, parent.model, "member", 1)
	r.model.mu.Lock()
	if r.model.approval != nil {
		t.Error("hidden workspace approval reached the visible workspace")
	}
	r.model.mu.Unlock()
	r.showWorkspace(1)
	r.model.mu.Lock()
	r.model.handleApprovalAnswer('y')
	r.model.handleApprovalAnswer('n')
	r.model.mu.Unlock()
	assertApprovalResult(t, member, true)
	assertApprovalResult(t, result, false)
}

func TestInlineApprovalRequiresRenderedRequest(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "parent-work")
	r.config.Confirm = true
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	secondCtx, cancelSecond := context.WithCancel(context.Background())
	defer cancelSecond()
	first := startMemberApproval(r, firstCtx, "one")
	waitApprovalQueue(t, r.model, "one", 0)
	r.model.mu.Lock()
	r.model.renderInputForTerminal(6, 120)
	r.model.mu.Unlock()
	second := startMemberApproval(r, secondCtx, "two")
	waitApprovalQueue(t, r.model, "one", 1)
	cancelFirst()
	assertApprovalResult(t, first, false)
	waitApprovalQueue(t, r.model, "two", 0)
	// The cancellation wakeup and an answer to the old prompt can both be
	// waiting on the event loop. Handling the key first must not approve two.
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "y"})
	select {
	case got := <-second:
		t.Fatalf("unrendered request received the old prompt's answer: %v", got)
	default:
	}
	r.model.mu.Lock()
	if r.model.approval == nil || r.model.approval.requester != "two" {
		r.model.mu.Unlock()
		t.Fatal("unrendered request was resolved")
	}
	r.model.renderInputForTerminal(6, 120)
	r.model.mu.Unlock()
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "y"})
	assertApprovalResult(t, second, true)
}

func TestInlineApprovalRequiresRenderedBatchIndex(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "parent-work")
	a := &approvalState{calls: []messages.ChatMessageToolCall{{ID: "one", Name: "bash"}, {ID: "two", Name: "bash"}}, reply: make(chan []bool, 1)}
	r.model.mu.Lock()
	r.model.approval = a
	r.model.renderInputForTerminal(6, 120)
	r.model.mu.Unlock()
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "y"})
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "a"})
	r.model.mu.Lock()
	if r.model.approval != a || a.index != 1 {
		r.model.mu.Unlock()
		t.Fatal("unrendered batch index was approved")
	}
	r.model.renderInputForTerminal(6, 120)
	r.model.mu.Unlock()
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "y"})
	select {
	case got := <-a.reply:
		if len(got) != 2 || !got[0] || !got[1] {
			t.Fatalf("rendered batch approvals = %v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("rendered batch did not complete")
	}
}
