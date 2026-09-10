package main

import (
	"fmt"
	"slices"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/swarm"
)

func swarmMemberActivity(s *swarm.State, member *swarm.Member) swarm.AgentPresentation {
	return swarm.MemberState(s, member)
}

// listingLabel is the row text: the approval overlay outranks the swarm's
// own label, which is derived, never persisted.
func listingLabel(p swarm.AgentPresentation, approval bool) string {
	if approval {
		return "approval needed"
	}
	return p.Display
}

// Swarm executions, rather than UI tabs or the result of the spawn tool,
// supply liveness. A background tool may finish while its member is running.
// Caller holds m.mu (or owns an unpublished display model).
func (m *replModel) hydrateSwarmAgents(s *swarm.State) {
	byCall := make(map[string][]string)
	for _, execution := range s.Executions {
		if execution.Request.CallID != "" {
			byCall[execution.Request.CallID] = append(byCall[execution.Request.CallID], execution.Member)
		}
	}
	counts := map[string]int{}
	for _, record := range m.toolDisclosures.all() {
		for _, row := range record.rows {
			if row.agent != nil {
				counts[row.callID]++
			}
		}
	}
	for _, record := range m.toolDisclosures.all() {
		for i := range record.rows {
			row := &record.rows[i]
			if row.agent != nil && row.agent.viewID == "" && counts[row.callID] == 1 && len(byCall[row.callID]) == 1 {
				row.agent.viewID = byCall[row.callID][0]
			}
		}
	}
	m.projectSwarmAgents(s)
	for _, record := range m.toolDisclosures.all() {
		changed := false
		for i := range record.rows {
			row := &record.rows[i]
			a := row.agent
			if a == nil {
				continue
			}
			id := a.viewID
			member := s.Members[id]
			if member == nil {
				continue
			}
			now := swarmMemberActivity(s, member)
			approval := m.memberNeedsApproval(member.ID)
			label := a.label
			if row.isProjectedAgent() {
				label = style.SanitizeImageText(spawnLabel(member.Label, member.Name))
			}
			in, out := 0, 0
			if execution := s.Executions[member.Execution]; execution != nil {
				if execution.Usage.InputTokens != nil {
					in = *execution.Usage.InputTokens
				}
				if execution.Usage.OutputTokens != nil {
					out = *execution.Usage.OutputTokens
				}
			}
			if a.label != label || a.session != member.Name || a.state != now || a.approval != approval || !a.attached || a.inputTokens != in || a.outputTokens != out {
				// The cue fires on the machine facts: work went from busy to
				// settled with something for the parent to look at.
				task := s.Tasks[member.Task]
				settled := task != nil && (task.Status == "done" || task.Status == "awaiting_review" && !now.Deferred)
				if a.state.Busy && !now.Busy && settled {
					m.noteAgentCompletion(record.id)
				}
				a.label, a.viewID, a.session = label, id, member.Name
				a.state, a.approval, a.local, a.attached = now, approval, "", true
				a.inputTokens, a.outputTokens = in, out
				changed = true
			}
		}
		if changed {
			m.refreshAgentRecord(record)
		}
	}
}

func (m *replModel) memberNeedsApproval(id string) bool {
	if m.approval != nil && m.approval.requester == id {
		return true
	}
	for _, a := range m.approvalQueue {
		if a.requester == id {
			return true
		}
	}
	return false
}

// Never read SQLite on the paint path. Poll at most twice a second while a
// frame is otherwise needed, and fence delayed results by tab and runtime ID.
func (r *managedREPL) refreshSwarmActivities() {
	if r.work == nil {
		return
	}
	for _, tab := range r.tabs {
		if tab.state == nil || tab.swarmLoading || time.Now().Before(tab.swarmRefreshAt) {
			continue
		}
		state, runtime, id := tab.state, tab.state.swarm, tab.viewID()
		viewStore, canView := state.sessionStore.(sessions.CoordinationViewStore)
		if runtime == nil && (!canView || r.rootTab(tab) != tab || id == "") {
			continue
		}
		tab.swarmLoading = true
		tab.swarmRefreshAt = time.Now().Add(500 * time.Millisecond)
		if !r.background(func() {
			var s *swarm.State
			var err error
			if runtime != nil {
				s, err = runtime.State(r.work.ctx)
			} else {
				s, err = tab.swarmView.ReadView(r.work.ctx, viewStore, id)
			}
			r.postUI(r.work.ctx, func() {
				tab.swarmLoading = false
				if err != nil || !slices.Contains(r.tabs, tab) || tab.viewID() != id || tab.state != state || tab.state.swarm != runtime {
					return
				}
				tab.swarmSnapshot = s
				tab.swarmActive = false
				for _, member := range s.Members {
					tab.swarmActive = tab.swarmActive || swarmMemberActivity(s, member).Busy
				}
				tab.model.mu.Lock()
				tab.model.hydrateSwarmAgents(s)
				if runtime != nil {
					r.announceSwarmCompletions(tab, s)
				}
				tab.model.mu.Unlock()
			})
		}) {
			tab.swarmLoading = false
		}
	}
}

// Typed launches also retain a concise completion notice. This is display-only,
// never another parent input or a second owner of the member's execution.
func (r *managedREPL) announceSwarmCompletions(tab *replTab, s *swarm.State) {
	for id, previous := range tab.swarmAnnounced {
		member := s.Members[id]
		if member == nil {
			continue
		}
		p := swarmMemberActivity(s, member)
		e := s.Executions[member.Execution]
		if p.Busy || e == nil {
			continue
		}
		key := fmt.Sprintf("%s:%d:%s", e.ID, e.Generation, e.Status)
		if previous == key {
			continue
		}
		tab.swarmAnnounced[id] = key
		tab.model.appendNoticeLine(member.Name + " · " + p.Display)
	}
}
