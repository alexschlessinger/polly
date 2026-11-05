package main

import (
	"fmt"
	"time"

	"github.com/alexschlessinger/pollytool/swarm"
)

const agentsFinishedDelay = 5 * time.Minute

// agentIdleClock is display state, not coordination state. The source changes
// on a new task or execution generation, even if it finishes between polls.
// Keep clocks on the root tab so closing the list does not restart the delay.
type agentIdleClock struct {
	source string
	since  time.Time
}

// refreshAgentIdle observes cached state only, on the event loop and without
// model locks held. Swarm polling calls it even while the list is closed.
func (r *managedREPL) refreshAgentIdle(root *replTab, now time.Time) {
	idle := map[string]agentIdleClock{}
	note := func(id, source string) {
		clock := root.agentIdle[id]
		if clock.since.IsZero() || clock.source != source {
			clock = agentIdleClock{source: source, since: now}
		}
		idle[id] = clock
	}
	root.model.mu.Lock()
	if s := root.swarmSnapshot; s != nil {
		for id, member := range s.Members {
			p := swarmMemberActivity(s, member)
			if p.Lifecycle != swarm.LifecycleIdle || p.Attention || p.Delivering || root.model.memberNeedsApproval(id) {
				continue
			}
			source := member.Execution + ":" + member.Task
			if e := s.Executions[member.Execution]; e != nil {
				source += fmt.Sprintf(":%d", e.Generation)
			}
			note(id, source)
		}
	}
	root.model.mu.Unlock()
	for _, tab := range r.tabs {
		if tab == root || r.rootTab(tab) != root {
			continue
		}
		id := tab.viewID()
		if id == "" {
			id = "name:" + tab.name
		}
		if s := root.swarmSnapshot; s != nil && s.Members[id] != nil {
			continue
		}
		m := tab.model
		m.mu.Lock()
		// unseenOutcome is only a badge: visiting the tab clears it. Use the
		// actual outcome so viewing a failed or paused turn cannot age it out.
		if !m.busy && m.approval == nil && len(m.approvalQueue) == 0 && (m.lastOutcome == turnOutcomeNone || m.lastOutcome == turnOutcomeDone) {
			note(id, fmt.Sprintf("tab:%d", m.turnID))
		}
		m.mu.Unlock()
	}
	root.agentIdle = idle
}

func (e agentsInspectorEntry) finished(now time.Time) bool {
	return e.history && !e.idleSince.IsZero() && now.Sub(e.idleSince) >= agentsFinishedDelay
}

func (list *agentsInspectorState) visible(id string) bool {
	e, exists := list.entries[id]
	return exists && (!e.finished(list.at) || list.finishedExpanded)
}
