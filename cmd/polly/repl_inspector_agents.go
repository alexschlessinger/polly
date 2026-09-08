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
		parentInfo, err := reader.ReadView(r.work.ctx, parent.session, "")
		var info *sessions.SessionView
		if err == nil {
			info, err = reader.ReadView(r.work.ctx, sessions.ViewTarget{Parent: parentInfo.Metadata.Name, SpawnCallID: callID}, "")
			// Name resolution can race a rename/deletion. A successful child
			// lookup must still belong to the exact parent we inspected.
			if err == nil && info.ParentID != parentInfo.ID {
				err = sessions.ErrSessionNotFound
			}
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

func (r *managedREPL) messageInspectedAgent() {
	w, owner := r.workspace(), r.visibleTab()
	target := w.inspector.target
	r.workspaceActions = append(r.workspaceActions, func() {
		if r.visibleTab() == owner {
			r.openAgentEditor(w, target)
		}
	})
}

func (r *managedREPL) openAgentEditor(w *sessionWorkspace, target viewTarget) {
	if target.kind != conversationViewKind {
		return
	}
	if w.agentDrafts == nil {
		w.agentDrafts = make(map[string]string)
	}
	key := target.key()
	expectedDraft := ""
	if tab := r.inspectionTab(target); tab != nil {
		tab.model.mu.Lock()
		expectedDraft = tab.model.ed.text()
		if !tab.viewOpening {
			if submitted, ok := w.agentSubmissions[key]; ok && expectedDraft == "" && w.agentDrafts[key] == submitted {
				delete(w.agentDrafts, key)
			}
			delete(w.agentSubmissions, key)
		}
		if _, ok := w.agentDrafts[key]; !ok {
			w.agentDrafts[key] = tab.model.ed.text()
		}
		tab.model.mu.Unlock()
	}
	m := &replModal{title: "Message " + target.session.Name, inputMode: true, width: 76, helper: "Enter send · Ctrl-J newline · Esc keep draft"}
	m.input.setText(w.agentDrafts[key])
	m.onDraft = func(text string) { w.agentDrafts[key] = text }
	m.onSubmit = func(text string) {
		if strings.TrimSpace(text) == "" {
			return
		}
		w.agentDrafts[key] = text
		r.workspaceActions = append(r.workspaceActions, func() { r.sendInspectorMessage(w, target, text, expectedDraft) })
	}
	r.model.mu.Lock()
	r.openModal(m)
	r.model.mu.Unlock()
}

// Called outside model locks. A saved view gains an execution owner only on
// this explicit submit; it is never acquired simply to paint a pane.
func (r *managedREPL) sendInspectorMessage(w *sessionWorkspace, target viewTarget, text, expectedDraft string) {
	if runtime := r.inspectedSwarm(target); runtime != nil {
		model := r.model
		r.background(func() {
			_, err := runtime.Send(r.work.ctx, runtime.ID, target.session.ID, "request", "", text)
			r.postUI(r.work.ctx, func() {
				model.mu.Lock()
				defer model.mu.Unlock()
				if err != nil {
					model.appendNoticeLine("could not send agent request: " + err.Error())
				} else {
					delete(w.agentDrafts, target.key())
					model.appendNoticeLine("request sent; paused members require /swarm resume ID")
				}
			})
		})
		return
	}
	tab := r.inspectionTab(target)
	if tab == nil {
		v := w.inspector.current
		if v == nil || v.info == nil || v.model == nil || v.target.session.ID != target.session.ID {
			return
		}
		// A live parent tab keeps the workspace chain honest for a grandchild;
		// the visible root only stands in when the parent has no runtime.
		parent := r.visibleTab()
		for _, candidate := range r.tabs {
			if v.info.ParentID != "" && candidate.viewID() == v.info.ParentID {
				parent = candidate
				break
			}
		}
		tab = &replTab{name: v.info.Metadata.Name, model: childDisplayCopy(v.model), childView: v.info, viewTarget: sessions.ViewTarget{ID: v.info.ID}, parent: parent, parentName: v.info.Metadata.Parent, delivered: true}
		tab.state = r.childViewState(r.state.sessionStore, v.info)
		tab.model.artifactStore = v.info.Artifacts
		r.tabs = append(r.tabs, tab)
	}
	mainModel, mainState := r.model, r.state
	m := tab.model
	m.mu.Lock()
	if !m.ed.empty() && m.ed.text() != expectedDraft {
		m.mu.Unlock()
		mainModel.mu.Lock()
		mainModel.appendNoticeLine("Agent has a draft · message kept in the inspector")
		mainModel.mu.Unlock()
		return
	}
	m.ed.setText(text)
	r.model, r.state = m, tab.state
	r.submitComposerLocked()
	r.model, r.state = mainModel, mainState
	if tab.viewOpening {
		if w.agentSubmissions == nil {
			w.agentSubmissions = make(map[string]string)
		}
		w.agentSubmissions[target.key()] = text
	}
	if m.ed.empty() {
		delete(w.agentDrafts, target.key())
	}
	m.mu.Unlock()
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
	if tab == nil {
		r.model.mu.Lock()
		r.model.appendNoticeLine("No approval pending for this agent")
		r.model.mu.Unlock()
		return
	}
	m := tab.model
	m.mu.Lock()
	request := m.approval
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
		title: fmt.Sprintf("Approval · %s · %s", tab.name, call.Name), width: width,
		body:  approvalCallBlock(call, strings.Split(expandToolCall(call), "\n"), width-4, approvalPromptMaxRows),
		items: items,
		onSubmit: func(answer string) {
			r.workspaceActions = append(r.workspaceActions, func() {
				m.mu.Lock()
				if m.approval == request && request.index == index {
					m.handleApprovalAnswer(answer[0])
				}
				more := m.approval != nil
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
