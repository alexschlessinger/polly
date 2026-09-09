package main

import (
	"sort"

	"github.com/alexschlessinger/pollytool/swarm"
)

// projectSwarmAgents adds display rows without inventing transcript tool calls.
// Real spawn rows retain their existing location; workflow members join their
// launch disclosure, and typed/restored launches receive standalone disclosures.
// Stable member/workflow identities prevent a wait, resume, or repaint from
// adding the same row again.
func (m *replModel) projectSwarmAgents(s *swarm.State) {
	calls := map[string]*toolDisclosureRecord{}
	callCounts := map[string]int{}
	members := map[string]bool{}
	workflowMembers := map[string]map[string]bool{}
	groups := map[string]*toolDisclosureRecord{}
	for _, record := range m.toolDisclosures {
		for _, row := range record.rows {
			if row.callID != "" {
				calls[row.callID] = record
				callCounts[row.callID]++
			}
			if a := row.agent; a != nil && a.viewID != "" {
				members[a.viewID] = true
				if a.workflowID != "" {
					if workflowMembers[a.workflowID] == nil {
						workflowMembers[a.workflowID] = map[string]bool{}
					}
					workflowMembers[a.workflowID][a.viewID] = true
					groups[a.workflowID] = record
				}
			}
		}
	}
	byCall := map[string][]string{}
	for _, e := range s.Executions {
		if e.Request.CallID != "" {
			byCall[e.Request.CallID] = append(byCall[e.Request.CallID], e.Member)
		}
	}
	add := func(memberID, workflowID, name string, record *toolDisclosureRecord) *toolDisclosureRecord {
		member := s.Members[memberID]
		if member == nil {
			return record
		}
		if record == nil {
			m.toolDisclosureSeq++
			record = &toolDisclosureRecord{id: m.toolDisclosureSeq, complete: true}
			// A background launch must not flush or split a streaming reply.
			record.transcriptIndex = m.appendTranscriptEntry("")
			m.toolDisclosures[record.id] = record
			m.toolDisclosureAt[record.transcriptIndex] = record.id
		}
		record.rows = append(record.rows, toolDisclosureRow{settled: true, agent: &agentActivity{
			viewID: memberID, label: sanitizeTranscriptImageText(spawnLabel(member.Label, member.Name)),
			workflowID: workflowID, workflowName: name,
		}})
		members[memberID] = true
		m.refreshAgentRecord(record)
		return record
	}
	workflowIDs := swarmRecordIDs(s.Workflows)
	sort.SliceStable(workflowIDs, func(i, j int) bool {
		return s.Workflows[workflowIDs[i]].Started.Before(s.Workflows[workflowIDs[j]].Started)
	})
	for _, workflowID := range workflowIDs {
		w := s.Workflows[workflowID]
		name := w.Name
		if name == "" {
			name = "untitled"
		}
		record := groups[workflowID]
		if record == nil && w.CallID != "" && callCounts[w.CallID] == 1 {
			record = calls[w.CallID]
		}
		seen := workflowMembers[workflowID]
		if seen == nil {
			seen = map[string]bool{}
		}
		for _, step := range w.Steps {
			if step.Kind != "agent" {
				continue
			}
			ids := byCall[step.ID]
			sort.Strings(ids)
			for _, memberID := range ids {
				if !seen[memberID] {
					record = add(memberID, workflowID, name, record)
					seen[memberID] = true
				}
			}
		}
		if record != nil {
			for i := range record.rows {
				a := record.rows[i].agent
				if a != nil && a.workflowID == workflowID && a.workflowName != name {
					a.workflowName = name
					m.refreshAgentRecord(record)
				}
			}
		}
	}
	for _, memberID := range swarmRecordIDs(s.Members) {
		if !members[memberID] {
			add(memberID, "", "", nil)
		}
	}
}
