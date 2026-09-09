package main

import (
	"fmt"
	"slices"
	"strings"

	"github.com/alexschlessinger/pollytool/sessions"
)

// tabs remains the execution registry. workspaces is the independent, ordered
// navigation list; observing a child never appends to this list.
func (r *managedREPL) syncWorkspaces() {
	for _, tab := range r.tabs {
		if !tab.workspaceRoot {
			continue
		}
		if !slices.Contains(r.workspaces, tab) {
			r.workspaces = append(r.workspaces, tab)
		}
	}
	r.workspaces = slices.DeleteFunc(r.workspaces, func(t *replTab) bool { return !slices.Contains(r.tabs, t) || !t.workspaceRoot })
}

func (r *managedREPL) rootTab(tab *replTab) *replTab {
	seen := make(map[*replTab]bool)
	for tab != nil && !seen[tab] {
		if tab.detachedWorkspace {
			return tab
		}
		seen[tab] = true
		parent := tab.parent
		if parent == nil && tab.parentName != "" {
			if n := r.tabIndexOf(tab.parentName); n >= 0 {
				parent = r.tabs[n]
			}
		}
		if parent == nil {
			return tab
		}
		tab = parent
	}
	return nil
}

func (r *managedREPL) workspaceShortcut(id string) (int, bool) {
	r.syncWorkspaces()
	visible := slices.Index(r.workspaces, r.visibleTab())
	if n, ok := tabShortcut(id, visible, len(r.workspaces)); ok {
		return r.tabIndexOfModel(r.workspaces[n].model), true
	}
	return 0, false
}

// peekTabActivity reads what a tab's turn is doing without blocking: the
// visible model is already locked by the caller, and another runtime that is
// busy under its own lock reports a generic activity label.
func (r *managedREPL) peekTabActivity(tab *replTab) string {
	if tab.model == r.model {
		return modelTabActivity(tab.model)
	}
	if tab.model.mu.TryLock() {
		defer tab.model.mu.Unlock()
		return modelTabActivity(tab.model)
	}
	return "working"
}

// workspaceActivity describes a workspace for the sessions picker: its own
// turn state, then how many of its agents are running or waiting on an
// approval. Caller holds the visible model's lock.
func (r *managedREPL) workspaceActivity(tab *replTab) string {
	var parts []string
	if activity := r.peekTabActivity(tab); activity != "" {
		parts = append(parts, activity)
	}
	running, approvals := r.agentCountsFor(tab)
	if running > 0 {
		parts = append(parts, turnAgentLabel(running)+" running")
	}
	if approvals > 0 {
		parts = append(parts, fmt.Sprintf("%d need approval", approvals))
	}
	return strings.Join(parts, " · ")
}

// hasLiveAgents reports whether any agent of the workspace runs in this polly.
func (r *managedREPL) hasLiveAgents(tab *replTab) bool {
	if tab.state != nil && tab.state.swarm != nil && tab.state.swarm.HasActive() {
		return true
	}
	for _, child := range r.tabs {
		if child != tab && r.rootTab(child) == tab {
			return true
		}
	}
	return false
}

// attentionAgentName is the agent of the visible workspace that waits on an
// approval, so Ctrl-G can open the picker on it; empty when none does.
func (r *managedREPL) attentionAgentName() string {
	owner := r.visibleTab()
	if owner.swarmSnapshot != nil {
		if a := owner.model.approval; a != nil {
			if member := owner.swarmSnapshot.Members[a.requester]; member != nil {
				return member.ID
			}
		}
		for _, a := range owner.model.approvalQueue {
			if member := owner.swarmSnapshot.Members[a.requester]; member != nil {
				return member.ID
			}
		}
	}
	for _, tab := range r.tabs {
		if tab != owner && r.rootTab(tab) == owner && r.peekTabActivity(tab) == "approval needed" {
			return tab.name
		}
	}
	return ""
}

func (r *managedREPL) inspectAgent(source *replModel, parent viewTarget, link agentLink) bool {
	record := source.toolDisclosures[link.recordID]
	if record == nil || link.rowIndex >= len(record.rows) {
		return false
	}
	row := record.rows[link.rowIndex]
	if row.agent == nil || row.agent.session == "" {
		return false
	}
	target := viewTarget{session: sessions.ViewTarget{ID: row.agent.viewID, Name: row.agent.session, Parent: parent.session.Name, SpawnCallID: row.callID}}
	r.inspect(target)
	return true
}
