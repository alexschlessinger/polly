package main

import (
	"fmt"
	"strings"

	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/swarm"
)

func (r *managedREPL) inspectLaunchedAgent(parent viewTarget, callID string) {
	if r.state == nil {
		return
	}
	store := r.state.sessionStore
	owner, generation := r.visibleTab(), r.workspace().inspector.generation
	r.background(func() {
		reader, ok := store.(sessions.ViewStore)
		if !ok {
			return
		}
		parentID := parent.session.ID
		var err error
		if parentID == "" {
			var parentInfo *sessions.SessionView
			parentInfo, err = readSessionHeader(r.work.ctx, reader, parent.session)
			if err == nil {
				parentID = parentInfo.ID
			}
		}
		var info *sessions.SessionSummary
		if err == nil {
			info, err = launchedAgentSummary(r.work.ctx, store, parentID, callID)
		}
		r.postUI(r.work.ctx, func() {
			if r.visibleTab() != owner || r.workspace().inspector.generation != generation {
				return
			}
			r.model.mu.Lock()
			defer r.model.mu.Unlock()
			if err != nil {
				r.model.appendErrorLine("agent: " + err.Error())
				return
			}
			r.inspect(viewTarget{session: sessions.ViewTarget{ID: info.ID, Name: info.Metadata.Name}})
		})
	})
}

// heldElsewhere reports whether the inspected saved session is leased by
// another polly: its view says it is in use, no tab here owns it, and it
// is not a member of this workspace's swarm. Such a view is read-only until
// the lease ends; the periodic re-read keeps the answer fresh.
func (r *managedREPL) heldElsewhere(target viewTarget) bool {
	v := r.workspace().inspector.current
	if v == nil || v.info == nil || !v.info.InUse || target.kind != conversationViewKind {
		return false
	}
	return r.inspectionTab(target) == nil && r.inspectedSwarm(target) == nil
}

func (r *managedREPL) stopInspectedAgent(target viewTarget) {
	if runtime := r.inspectedSwarm(target); runtime != nil {
		model := r.model
		r.background(func() {
			err := runtime.StopMember(r.work.ctx, target.session.ID)
			model.mu.Lock()
			defer model.mu.Unlock()
			if err != nil {
				model.appendNoticeLine(err.Error())
			}
		})
		return
	}
	tab := r.inspectionTab(target)
	if tab == nil || tab == r.visibleTab() {
		return
	}
	m := tab.model
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.busy || m.canceling {
		return
	}
	m.canceling = true
	m.finishAssistantBlock("")
	r.cancelTurn(tab)
	r.armCancelDetach(tab)
	m.denyApprovalLocked()
}

func (r *managedREPL) resumeInspectedAgent(target viewTarget) {
	if runtime := r.inspectedSwarm(target); runtime != nil {
		model := r.model
		r.background(func() {
			if err := runtime.Resume(r.work.ctx, target.session.ID, 0); err != nil {
				model.mu.Lock()
				model.appendNoticeLine(err.Error())
				model.mu.Unlock()
			}
		})
	}
}

func (r *managedREPL) inspectedSwarm(target viewTarget) *swarm.Runtime {
	if r.state == nil || r.state.swarm == nil {
		return nil
	}
	v := r.workspace().inspector.current
	if v == nil || v.info == nil || v.info.Metadata == nil || v.info.ID != target.session.ID || v.info.Metadata.SwarmID != r.state.swarm.ID {
		return nil
	}
	return r.state.swarm
}

func (r *managedREPL) reviewAgentApproval(target viewTarget) {
	owner := r.visibleTab()
	r.workspaceActions = append(r.workspaceActions, func() {
		if r.visibleTab() == owner {
			r.openAgentApproval(target)
		}
	})
}

func (r *managedREPL) openAgentApproval(target viewTarget) {
	tab := r.inspectionTab(target)
	member := r.inspectedSwarm(target) != nil
	if member {
		tab = r.visibleTab()
	}
	if tab == nil {
		r.model.mu.Lock()
		r.model.appendNoticeLine("No approval pending for this agent")
		r.model.mu.Unlock()
		return
	}
	m := tab.model
	m.mu.Lock()
	request := m.approval
	if member {
		request = m.memberApproval(target.session.ID)
	}
	if request == nil {
		m.mu.Unlock()
		return
	}
	index := request.index
	call := request.calls[index]
	remaining := len(request.calls) - index
	m.mu.Unlock()
	r.model.mu.Lock()
	defer r.model.mu.Unlock()
	// The call itself sits above the choices, so nothing needs a "view" step.
	const width = 88
	items := []replModalItem{{label: "Deny", value: "n"}, {label: "Allow this call", value: "y"}}
	if remaining > 1 {
		items = append(items, replModalItem{label: "Allow remaining calls in this batch", value: "a"})
	}
	r.openModal(&replModal{
		title: fmt.Sprintf("Approval · %s · %s", target.session.Name, call.Name), width: width,
		body:  approvalCallBlock(call, strings.Split(expandToolCall(call), "\n"), width-4, approvalPromptMaxRows),
		items: items,
		onSubmit: func(answer string) {
			r.workspaceActions = append(r.workspaceActions, func() {
				m.mu.Lock()
				if m.hasApprovalRequest(request) && request.index == index {
					m.answerApprovalRequest(request, answer[0])
				}
				more := m.hasApprovalRequest(request)
				m.mu.Unlock()
				if more {
					r.model.mu.Lock()
					r.reviewAgentApproval(target)
					r.model.mu.Unlock()
				}
			})
		},
	})
}
