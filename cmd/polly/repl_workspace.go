package main

import (
	"fmt"
	"slices"
	"strconv"

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

// Commands queue this snapshot until the composer lock has been released.
func (r *managedREPL) requestWorkspaceLines() []string {
	owner := r.visibleTab()
	r.workspaceActions = append(r.workspaceActions, func() {
		lines := r.workspaceLines()
		owner.model.mu.Lock()
		defer owner.model.mu.Unlock()
		for _, line := range lines {
			owner.model.appendNoticeLine(line)
		}
	})
	return nil
}

func (r *managedREPL) workspaceLines() []string {
	r.syncWorkspaces()
	lines := []string{fmt.Sprintf("tabs (%d):", len(r.workspaces))}
	for n, tab := range r.workspaces {
		line := fmt.Sprintf("  %d  %s", n+1, tab.name)
		if activity := r.tabActivity(tab); activity != "" {
			line += "  " + activity
		}
		running, approvals := 0, 0
		for _, child := range r.tabs {
			if child == tab || r.rootTab(child) != tab {
				continue
			}
			m := child.model
			m.mu.Lock()
			if m.busy {
				running++
			}
			if m.approval != nil {
				approvals++
			}
			m.mu.Unlock()
		}
		if running > 0 {
			line += fmt.Sprintf(" · %d agents running", running)
		}
		if approvals > 0 {
			line += fmt.Sprintf(" · %d need approval", approvals)
		}
		if tab.model == r.model {
			line += "  current"
		}
		lines = append(lines, line)
	}
	return lines
}

func (r *managedREPL) resolveWorkspace(arg string) (int, error) {
	r.syncWorkspaces()
	if n, err := strconv.Atoi(arg); err == nil {
		if n < 1 || n > len(r.workspaces) {
			return -1, fmt.Errorf("no tab %d (%d open)", n, len(r.workspaces))
		}
		return r.tabIndexOfModel(r.workspaces[n-1].model), nil
	}
	for _, tab := range r.workspaces {
		if tab.name == arg {
			return r.tabIndexOfModel(tab.model), nil
		}
	}
	return -1, fmt.Errorf("no tab named %s", arg)
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
	for _, tab := range r.tabs {
		if tab.agentActivity == row.agent || tab.agentActivity != nil && tab.agentActivity == row.agent.origin {
			target = tabViewTarget(tab)
			break
		}
	}
	r.inspect(target)
	return true
}
