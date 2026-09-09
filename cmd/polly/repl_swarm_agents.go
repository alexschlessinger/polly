package main

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/swarm"
)

func swarmMemberActivity(s *swarm.State, member *swarm.Member) (string, bool) {
	switch member.Status {
	case "queued", "running", "waiting":
		return member.Status, true
	}
	if execution := s.Executions[member.Execution]; execution != nil {
		if member.Status == "paused" && execution.StopReason == messages.StopReasonMaxIterations {
			return fmt.Sprintf("paused · iteration limit reached (%d/%d)", execution.Iterations, execution.Request.MaxIterations), false
		}
		if execution.Status == "failed" {
			return "failed", false
		}
	}
	if member.Status == "idle" {
		if task := s.Tasks[member.Task]; task != nil {
			return strings.ReplaceAll(task.Status, "_", " "), false
		}
		return "idle", false
	}
	return member.Status, false
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
	for _, record := range m.toolDisclosures {
		for _, row := range record.rows {
			if row.agent != nil {
				counts[row.callID]++
			}
		}
	}
	for _, record := range m.toolDisclosures {
		for i := range record.rows {
			row := &record.rows[i]
			if row.agent != nil && row.agent.viewID == "" && counts[row.callID] == 1 && len(byCall[row.callID]) == 1 {
				row.agent.viewID = byCall[row.callID][0]
			}
		}
	}
	m.projectSwarmAgents(s)
	for _, record := range m.toolDisclosures {
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
			status, active := swarmMemberActivity(s, member)
			if m.memberNeedsApproval(member.ID) {
				status = "approval needed"
			}
			label := a.label
			if row.isProjectedAgent() {
				label = sanitizeTranscriptImageText(spawnLabel(member.Label, member.Name))
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
			if a.label != label || a.session != member.Name || a.status != status || a.active != active || !a.attached || a.inputTokens != in || a.outputTokens != out {
				if a.active && !active && (status == "done" || status == "awaiting review") {
					m.noteAgentCompletion(record.id)
				}
				a.label, a.viewID, a.session, a.status = label, id, member.Name, status
				a.active, a.attached = active, true
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
		if tab.state == nil || tab.state.swarm == nil || tab.swarmLoading || time.Now().Before(tab.swarmRefreshAt) {
			continue
		}
		runtime, id := tab.state.swarm, tab.viewID()
		tab.swarmLoading = true
		tab.swarmRefreshAt = time.Now().Add(500 * time.Millisecond)
		if !r.background(func() {
			s, err := runtime.State(r.work.ctx)
			r.postUI(r.work.ctx, func() {
				tab.swarmLoading = false
				if err != nil || !slices.Contains(r.tabs, tab) || tab.viewID() != id || tab.state == nil || tab.state.swarm != runtime {
					return
				}
				tab.swarmSnapshot = s
				tab.swarmActive = false
				for _, member := range s.Members {
					_, active := swarmMemberActivity(s, member)
					tab.swarmActive = tab.swarmActive || active
				}
				tab.model.mu.Lock()
				tab.model.hydrateSwarmAgents(s)
				r.announceSwarmCompletions(tab, s)
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
		status, active := swarmMemberActivity(s, member)
		e := s.Executions[member.Execution]
		if active || e == nil {
			continue
		}
		key := fmt.Sprintf("%s:%d:%s", e.ID, e.Generation, e.Status)
		if previous == key {
			continue
		}
		tab.swarmAnnounced[id] = key
		tab.model.appendNoticeLine(member.Name + " · " + status)
	}
}
