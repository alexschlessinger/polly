package main

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/swarm"
)

// inspectingAgent reports whether the inspector shows another session's
// conversation: an agent, whose view keeps its reading position across
// projections and ticks the calls its runtime reports.
func (r *managedREPL) inspectingAgent() bool {
	i := &r.workspace().inspector
	return i.open && i.target.kind == conversationViewKind && !r.targetsVisibleTab(i.target)
}

// inspectedLiveTools returns the calls the owning runtime reports as running
// in the inspected agent's current execution. Its transcript may not have
// recorded them yet.
func (r *managedREPL) inspectedLiveTools() []swarm.LiveToolActivity {
	i := &r.workspace().inspector
	owner := r.rootTab(r.visibleTab())
	snapshot := owner.swarmSnapshot
	if snapshot == nil {
		return nil
	}
	member := snapshot.Members[i.target.session.ID]
	if member == nil {
		return nil
	}
	activity := owner.swarmActivities[member.ID]
	if !currentAgentActivity(snapshot, member, activity) {
		return nil
	}
	return slices.Clone(activity.Tools)
}

// liveToolsKey names a set of running calls for a projection revision.
func liveToolsKey(live []swarm.LiveToolActivity) string {
	if len(live) == 0 {
		return ""
	}
	var b strings.Builder
	for _, tool := range live {
		fmt.Fprintf(&b, "%s/%s;", tool.Call.ID, tool.Call.DisplayID)
	}
	return b.String()
}

// adoptLiveTools shows the calls the owning runtime reports as running in the
// current turn's activity line. A recorded row that is still open takes the
// runtime's start time for its clock; a call the transcript has not recorded
// yet gets a running row of its own. The rows join m.activeTools so that
// refreshActiveTools ticks them at paint. m is an isolated display model.
func (m *replModel) adoptLiveTools(live []swarm.LiveToolActivity) {
	if len(live) == 0 {
		return
	}
	record := m.currentTurnToolRecord()
	known := make(map[string]bool, len(m.displayCatalog.tools))
	for _, tool := range m.displayCatalog.tools {
		if tool.call.DisplayID != "" {
			known[tool.call.DisplayID] = true
		}
	}
	adopted := false
	for _, tool := range live {
		call := tool.Call
		index := -1
		if record != nil {
			for i := range record.rows {
				row := &record.rows[i]
				if row.isProjectedAgent() {
					continue
				}
				// A host identity names its row outright. Without one, only a
				// still-open row that also lacks one and carries the provider's
				// call ID can be this call.
				if call.DisplayID != "" && row.displayID == call.DisplayID || call.DisplayID == "" && row.displayID == "" && row.callID == call.ID && !row.settled {
					index = i
					break
				}
			}
		}
		if index < 0 && call.DisplayID != "" && known[call.DisplayID] {
			// The transcript owns this call: its row sits in an earlier record
			// or has settled while the metadata lags.
			continue
		}
		if index >= 0 && record.rows[index].settled {
			continue
		}
		if index < 0 {
			if record == nil {
				record = m.ensureToolDisclosure()
			}
			row := toolDisclosureRow{callID: call.ID, label: style.StripImageMarkers(toolLabel(call))}
			row.setCall(call)
			row.sectionKey = displayToolKey(call, len(m.displayCatalog.tools)+len(record.rows)+1)
			record.rows = append(record.rows, row)
			index = len(record.rows) - 1
		}
		row := &record.rows[index]
		row.setLine(runningInlineTool(max(0, time.Since(tool.Started))))
		ticking := false
		for i := range m.activeTools {
			if m.activeTools[i].row == index {
				m.activeTools[i].started, ticking = tool.Started, true
			}
		}
		if !ticking {
			m.activeTools = append(m.activeTools, activeTool{id: call.ID, row: index, label: row.label, started: tool.Started})
		}
		adopted = true
	}
	if !adopted {
		return
	}
	record.complete = false
	m.activeToolsPhase = -1
	m.turnToolDisclosureID = record.id
	m.refreshToolDisclosure(record)
}

// currentTurnToolRecord is the newest tool record, when it belongs to the
// turn in progress: no later prompt follows it.
func (m *replModel) currentTurnToolRecord() *toolDisclosureRecord {
	var last *toolDisclosureRecord
	for _, record := range m.toolDisclosures.all() {
		if last == nil || record.transcriptIndex > last.transcriptIndex {
			last = record
		}
	}
	if last == nil || last.transcriptIndex < 0 {
		return nil
	}
	for _, entry := range m.transcript[min(len(m.transcript), last.transcriptIndex+1):] {
		if strings.HasPrefix(entry.text, userGutter()) {
			return nil
		}
	}
	return last
}
