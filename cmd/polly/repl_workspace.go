package main

import (
	"fmt"
	"slices"
	"strings"

	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/swarm"
)

// workspaceTabs lists the workspace roots in tab order: the navigation list
// that shortcuts, the sessions picker, and closing count through. tabs
// remains the execution registry; observing a child adds a tab but never a
// workspace.
func (r *managedREPL) workspaceTabs() []*replTab {
	var workspaces []*replTab
	for _, tab := range r.tabs {
		if tab.workspaceRoot {
			workspaces = append(workspaces, tab)
		}
	}
	return workspaces
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
	workspaces := r.workspaceTabs()
	visible := slices.Index(workspaces, r.visibleTab())
	if n, ok := tabShortcut(id, visible, len(workspaces)); ok {
		return r.tabIndexOfModel(workspaces[n].model), true
	}
	return 0, false
}

// parentPresentation is the root's own swarm lifecycle. It is an in-memory
// read over the event-loop-owned snapshot, safe on the paint path and current
// between polls. Tabs without a runtime (read-only roots) report nothing.
func (r *managedREPL) parentPresentation(tab *replTab) (swarm.AgentPresentation, bool) {
	if tab == nil || tab.state == nil || tab.state.swarm == nil {
		return swarm.AgentPresentation{}, false
	}
	return tab.state.swarm.ParentState(tab.swarmSnapshot), true
}

// tabHasSwarm reports whether the root ever delegated work. Every root owns
// a runtime, so a parent outcome only means something once a run exists.
func tabHasSwarm(tab *replTab) bool {
	return tab != nil && tab.swarmSnapshot != nil && len(tab.swarmSnapshot.Runs) > 0
}

// parentInformative says whether a parent line adds anything to a listing.
func parentInformative(p swarm.AgentPresentation) bool {
	return p.Detail != "" || p.Lifecycle != swarm.LifecycleIdle
}

// tabActivityBusy reads the tab vocabulary: anything but idle and the settled
// outcomes means a turn is in progress.
func tabActivityBusy(status string) bool {
	return status != "" && status != "done" && status != "failed" && status != "incomplete"
}

func joinStatus(parts ...string) string {
	var kept []string
	for _, part := range parts {
		if part != "" {
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, " · ")
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
	if owner.swarmSnapshot != nil {
		ids := make([]string, 0, len(owner.swarmSnapshot.Members))
		for id := range owner.swarmSnapshot.Members {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		for _, id := range ids {
			if swarm.MemberState(owner.swarmSnapshot, owner.swarmSnapshot.Members[id]).Attention {
				return id
			}
		}
	}
	return ""
}

func (r *managedREPL) inspectAgent(source *replModel, parent viewTarget, link agentLink) bool {
	record := source.toolDisclosures.get(link.recordID)
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
