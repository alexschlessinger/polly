package main

import (
	"fmt"
	"reflect"
	"slices"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/swarm"
)

func swarmMemberActivity(view swarm.DisplayView, member *swarm.Member) swarm.AgentPresentation {
	if view == nil || member == nil {
		return swarm.AgentPresentation{}
	}
	if state, ok := view.(*swarm.State); ok {
		return swarm.MemberState(state, member)
	}
	return view.DisplaySnapshot("").MemberState(member.ID)
}

// Projected launches have no spawn tool row to supply the task brief.
// Recover it from coordination state and use the same label fallback.
func swarmMemberLabel(view swarm.DisplayView, member *swarm.Member) string {
	s := view.DisplaySnapshot("")
	brief := ""
	if execution := s.Executions[member.Execution]; execution != nil {
		brief = execution.Request.Task
	}
	if brief == "" {
		if task := s.Tasks[member.Task]; task != nil {
			brief = task.Description
		}
	}
	if brief == "" {
		brief = member.Name
	}
	return spawnLabel(member.Label, brief)
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
func (m *replModel) hydrateSwarmAgents(view swarm.DisplayView) {
	if view == nil {
		return
	}
	s := view.DisplaySnapshot(m.swarmParent)
	if s == nil {
		return
	}
	byCall := s.Launches
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
	byMember, byWorkflow := s.MemberDecisions, s.WorkflowDecisions
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
			live, liveWarn := agentRowStatus(s, member, m.swarmActivities[id], time.Now())
			approval := m.memberNeedsApproval(member.ID)
			label := a.label
			if row.isProjectedAgent() {
				label = style.SanitizeImageText(swarmMemberLabel(s, member))
			}
			decisions, workflowDecisions := byMember[member.ID], byWorkflow[a.workflowID]
			if a.label != label || a.session != member.Name || a.state != now || a.live != live || a.liveWarn != liveWarn || a.approval != approval || !a.attached || a.decisions != decisions || a.workflowDecisions != workflowDecisions {
				// The cue fires on the machine facts: work went from busy to
				// settled with something for the parent to look at.
				task := s.Tasks[member.Task]
				settled := task != nil && (task.Status == "done" || (now.Delivering || task.Status == "awaiting_review") && !now.Deferred)
				if a.state.Busy && !now.Busy && settled {
					m.noteAgentCompletion(record.id)
				}
				a.label, a.viewID, a.session = label, id, member.Name
				a.state, a.approval, a.local, a.attached = now, approval, "", true
				a.live, a.liveWarn = live, liveWarn
				a.decisions, a.workflowDecisions = decisions, workflowDecisions
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

// Never read SQLite on the paint path. Scoped hints permit reads at most twice
// a second; unchanged frames only advance cached display clocks. Fence delayed
// results by tab, root, runtime, store, and notification-source identity.
func (r *managedREPL) refreshSwarmActivities() {
	if r.work == nil {
		return
	}
	for _, tab := range r.tabs {
		if tab.state == nil || tab.swarmLoading || time.Now().Before(tab.swarmRefreshAt) {
			continue
		}
		state, runtime, id := tab.state, tab.state.swarm, tab.viewID()
		viewStore := state.sessionStore
		_, viewErr := sessions.OpenCoordinationViewAccess(viewStore)
		if runtime == nil && (viewErr != nil || r.rootTab(tab) != tab || id == "") {
			continue
		}
		force := tab.swarmRefreshAt.IsZero() || tab.swarmDisplay == nil
		tab.swarmRefreshAt = time.Now().Add(500 * time.Millisecond)
		if tab.swarmDisplay != nil {
			tab.model.mu.Lock()
			tab.model.hydrateSwarmAgents(tab.swarmDisplay)
			tab.model.mu.Unlock()
		}
		source, notificationErr := tab.notificationSource(r.work.ctx)
		if notificationErr == nil {
			notificationErr = tab.swarmChanges.bind(r.work.ctx, source, swarmOverviewScopes())
		}
		readDisplay := tab.swarmChanges.take(force)
		if !readDisplay && (runtime == nil || !runtime.HasActive() && len(tab.swarmActivities) == 0) {
			continue
		}
		// Stream/tool activity is intentionally not persisted per chunk. Keep
		// its cheap in-memory clock independent of durable display revisions.
		display := tab.swarmDisplay
		tab.swarmLoading = true
		if !r.background(func() {
			s := display
			var activities map[string]swarm.LiveActivity
			var err error
			if readDisplay {
				if runtime != nil {
					s, err = tab.swarmView.Read(r.work.ctx, runtime)
				} else {
					s, err = tab.swarmView.ReadView(r.work.ctx, viewStore, id)
				}
			}
			if runtime != nil {
				activities = runtime.LiveActivities()
			}
			r.postUI(r.work.ctx, func() {
				tab.swarmLoading = false
				if !slices.Contains(r.tabs, tab) || tab.viewID() != id || tab.state != state || tab.state.swarm != runtime || tab.notifications != source || source != nil && !source.matches(tab) {
					tab.swarmChanges.initial = true
					return
				}
				readErr := err
				if readErr == nil {
					readErr = notificationErr
				}
				if readErr != nil {
					tab.swarmChanges.initial = true
					if text := readErr.Error(); tab.swarmReadError != text {
						tab.swarmReadError = text
						tab.model.mu.Lock()
						tab.model.appendErrorLine("swarm updates unavailable: " + text)
						tab.model.mu.Unlock()
					}
				} else {
					tab.swarmReadError = ""
				}
				if err != nil {
					return
				}
				tab.swarmDisplay = s
				if !reflect.DeepEqual(tab.swarmActivities, activities) {
					tab.swarmActivityRevision++
				}
				tab.swarmActivities = activities
				tab.swarmActive = s.BusyMembers > 0
				tab.model.mu.Lock()
				tab.model.swarmParent = tab.viewID()
				tab.model.swarmActivities = activities
				tab.agentTimeline.observe(s, activities, tab.model.approvalRequesters(), time.Now())
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
func (r *managedREPL) announceSwarmCompletions(tab *replTab, view swarm.DisplayView) {
	s := view.DisplaySnapshot(tab.viewID())
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
