package main

import (
	"fmt"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
)

// All queue transitions run under model.mu. Only the owner of a pending
// request may reply to it; cancellation and an answer can therefore race
// without closing a channel twice or approving a different member's call.
func replyApproval(a *approvalState, results []bool) {
	if a.ctx != nil && a.ctx.Err() != nil {
		results = denyToolCalls(a.calls)
	}
	if a.reply != nil {
		select {
		case a.reply <- results:
		default:
		}
		close(a.reply)
	}
}

func (m *replModel) advanceApprovalLocked() {
	for m.approval == nil && len(m.approvalQueue) > 0 {
		a := m.approvalQueue[0]
		m.approvalQueue = m.approvalQueue[1:]
		if a.ctx != nil && a.ctx.Err() != nil {
			replyApproval(a, denyToolCalls(a.calls))
			continue
		}
		m.approval = a
		label := toolLabel(a.calls[0])
		if len(a.calls) > 1 {
			label += fmt.Sprintf(" +%d more", len(a.calls)-1)
		}
		if a.requester != "" {
			label = "agent " + a.requester + ": " + label
		}
		m.pushNotice("approval needed: " + style.Truncate(label, 80))
		m.signalHiddenLocked(signalApprovalNeeded, style.Truncate(label, 80))
	}
}

func (m *replModel) resolveApprovalLocked(a *approvalState, results []bool) {
	if m.approval == a {
		m.approval = nil
	} else {
		index := -1
		for i, pending := range m.approvalQueue {
			if pending == a {
				index = i
				break
			}
		}
		if index == -1 {
			return // An answer, cancellation, or shutdown already resolved it.
		}
		m.approvalQueue = append(m.approvalQueue[:index], m.approvalQueue[index+1:]...)
	}
	replyApproval(a, results)
	m.advanceApprovalLocked()
}

func (m *replModel) denyApprovalLocked() {
	if m.approval != nil {
		replyApproval(m.approval, denyToolCalls(m.approval.calls))
		m.approval = nil
	}
	for _, a := range m.approvalQueue {
		replyApproval(a, denyToolCalls(a.calls))
	}
	m.approvalQueue = nil
}

func (m *replModel) hasApprovalRequest(a *approvalState) bool {
	if a == nil {
		return false
	}
	if m.approval == a {
		return true
	}
	for _, pending := range m.approvalQueue {
		if pending == a {
			return true
		}
	}
	return false
}

func (m *replModel) memberApproval(id string) *approvalState {
	if m.approval != nil && m.approval.requester == id {
		return m.approval
	}
	for _, a := range m.approvalQueue {
		if a.requester == id {
			return a
		}
	}
	return nil
}
