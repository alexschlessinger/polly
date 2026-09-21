package main

import (
	"strings"

	"github.com/alexschlessinger/pollytool/subagent"
	"github.com/alexschlessinger/pollytool/swarm"
)

// spawnRequest is a /spawn typed on a tab, applied by the event loop.
type spawnRequest struct {
	parent *replTab
	req    subagent.Request
}

// signalName names the tab in notices for the visible tab, placing a child
// under its parent.
func (t *replTab) signalName() string {
	if t.parentName != "" {
		return t.name + " (agent of " + t.parentName + ")"
	}
	return t.name
}

// liveParent is child's parent tab while it is open here, else nil.
func (r *managedREPL) liveParent(child *replTab) *replTab {
	if child.parent != nil && r.tabIndexOfModel(child.parent.model) >= 0 {
		return child.parent
	}
	return nil
}

// closeSpentChild releases a delivered agent's hidden tab. Drafts, queued
// input, and follow-up conversations belong to the user and keep it open.
// Runs on the event loop with no model lock held.
func (r *managedREPL) closeSpentChild(tab *replTab) bool {
	if tab.parentName == "" || !tab.delivered || tab.keepOpen || tab.turnDone != nil || tab.viewOpening || tab.model == r.model || r.runningDescendants(tab) > 0 {
		return false
	}
	m := tab.model
	m.mu.Lock()
	// An untouched retry draft restored after a failed initial run is not
	// user input. Editing it makes it an ordinary draft.
	draft := !m.ed.empty() && (m.restoredDraft == nil || m.ed.text() != m.restoredDraft.displayText)
	keep := m.busy || draft || len(m.queue) > 0 || m.pasting || m.clipboardCapture
	m.mu.Unlock()
	if keep {
		return false
	}
	i := r.tabIndexOfModel(tab.model)
	if i < 0 {
		return false
	}
	r.removeTab(i)
	r.closeTabState(tab)
	return true
}

func (r *managedREPL) closeSpentTabs() {
	for i := len(r.tabs) - 1; i >= 0; i-- {
		r.closeSpentChild(r.tabs[i])
	}
}

// requestSpawnLocked starts a background child of the visible tab on brief;
// the event loop applies it. Caller must hold r.model.mu.
func (r *managedREPL) requestSpawnLocked(req subagent.Request) {
	if r.visibleTabIndex() < 0 || r.state == nil || r.state.swarm == nil {
		r.model.appendNoticeLine("Spawn from a parent session; inspected agents cannot start agents")
		return
	}
	req.Background = true
	r.spawnRequests = append(r.spawnRequests, spawnRequest{parent: r.visibleTab(), req: req})
}

// applySpawnRequests performs the /spawn requests handlers recorded. Runs on
// the event loop with no model lock held.
func (r *managedREPL) applySpawnRequests() bool {
	requests := r.spawnRequests
	r.spawnRequests = nil
	if r.quitting {
		return false
	}
	for _, sr := range requests {
		parent := sr.parent
		if r.tabIndexOfModel(parent.model) < 0 || parent.state == nil || parent.state.swarm == nil {
			continue
		}
		runtime, store := parent.state.swarm, parent.state.sessionStore
		probe := parent.state.sandboxProbe
		parent.model.mu.Lock()
		settings := parent.state.settings.clone()
		req := createCompletionRequest(r.config, &settings, nil, parent.state.toolRegistry, nil, nil)
		updateSwarmDefaults(parent.state, req, settings)
		parent.model.mu.Unlock()
		// Worktree capture and tool binding can take time. The runtime owns the
		// member from launch onward, independently of the visible tab.
		r.background(func() {
			var res subagent.Result
			err := parent.state.waitWorkspaceChanges(r.work.ctx)
			if err == nil {
				err = probe.wait(r.work.ctx)
			}
			if err == nil {
				res, err = runtime.Spawn(r.work.ctx, sr.req)
			}
			// Spawn returns the stable member ID. Resolve the display handle off
			// the event loop without acquiring the member's execution lease.
			name := ""
			if err == nil {
				if summaries, listErr := store.ListSummaries(r.work.ctx); listErr == nil {
					for _, summary := range summaries {
						if summary.ID == res.Session && summary.Metadata != nil {
							name = summary.Metadata.Name
							break
						}
					}
				}
			}
			var snapshot *swarm.State
			if err == nil {
				snapshot, _ = runtime.State(r.work.ctx)
			}
			r.postUI(r.work.ctx, func() {
				if r.tabIndexOfModel(parent.model) < 0 || parent.state.swarm != runtime {
					return
				}
				parent.model.mu.Lock()
				defer parent.model.mu.Unlock()
				if err != nil {
					parent.model.appendErrorLine("could not spawn an agent: " + err.Error())
				} else {
					parent.swarmActive = true
					if parent.swarmAnnounced == nil {
						parent.swarmAnnounced = make(map[string]string)
					}
					parent.swarmAnnounced[res.Session] = ""
					if snapshot != nil {
						parent.swarmSnapshot = snapshot
						parent.model.swarmParent = parent.viewID()
						parent.model.hydrateSwarmAgents(snapshot)
					}
					notice := "Agent started"
					if name != "" {
						notice = "Agent " + name + " started"
					}
					parent.model.appendNoticeLine(notice + " · /sessions to inspect")
				}
			})
		})
	}
	return len(requests) > 0
}

// turnToolCallCount counts the tool calls the current turn has made. Caller
// must hold m.mu.
func (m *replModel) turnToolCallCount() int {
	n := 0
	for _, id := range m.turnToolDisclosureIDs {
		if record := m.toolDisclosures.get(id); record != nil {
			for _, row := range record.rows {
				if !row.isProjectedAgent() {
					n++
				}
			}
		}
	}
	return n
}

// tabShortcut maps Alt-1 to Alt-9 to a tab position and Alt-] and Alt-[ to
// the next and previous tab, wrapping around.
func tabShortcut(id string, visible, count int) (int, bool) {
	if count == 0 {
		return 0, false
	}
	switch id {
	case "<M-]>":
		return (visible + 1) % count, true
	case "<M-[>":
		return ((visible-1)%count + count) % count, true
	}
	if len(id) == 5 && strings.HasPrefix(id, "<M-") && id[3] >= '1' && id[3] <= '9' && id[4] == '>' {
		if n := int(id[3] - '1'); n < count {
			return n, true
		}
	}
	return 0, false
}
