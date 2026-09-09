package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/subagent"
	ui "github.com/metaspartan/gotui/v5"
)

// agentActivity belongs to the spawn request's disclosure, not its tool
// execution. A background tool result can settle long before this run does.
type agentActivity struct {
	viewID                   string
	label                    string
	status                   string
	session                  string
	background               bool
	active                   bool
	attached                 bool
	workflowID, workflowName string

	// Reported usage for the original delegated run, retained after completion.
	inputTokens, outputTokens int
	// origin binds saved display rows to a live inspector across renames.
	origin *agentActivity
}

// agentLink is relative to its rendered block until projected into a viewport.
type agentLink struct {
	recordID int64
	rowIndex int
	X, Y     int
	Cols     int
}

func (row *toolDisclosureRow) setCall(call messages.ChatMessageToolCall) {
	row.toolName = call.Name
	if call.Name != subagent.ToolName || row.agent != nil {
		return
	}
	var args struct {
		Label      string `json:"label"`
		Task       string `json:"task"`
		Background bool   `json:"background"`
	}
	_ = json.Unmarshal([]byte(call.Arguments), &args)
	label := sanitizeTranscriptImageText(spawnLabel(args.Label, args.Task))
	if label == "" {
		label = "agent"
	}
	row.agent = &agentActivity{label: label, status: "starting", active: true, background: args.Background}
}

func (row *toolDisclosureRow) isAgent() bool {
	return row.agent != nil || row.toolName == subagent.ToolName
}

func (row *toolDisclosureRow) isProjectedAgent() bool {
	return row.agent != nil && row.toolName == ""
}

func ordinaryToolRows(rows []toolDisclosureRow) []toolDisclosureRow {
	var ordinary []toolDisclosureRow
	for _, row := range rows {
		if !row.isAgent() {
			ordinary = append(ordinary, row)
		}
	}
	return ordinary
}

func (row *toolDisclosureRow) finishAgentCall(_ messages.ChatMessageToolCall, denied bool, err error) {
	a := row.agent
	if a == nil || a.attached {
		return
	}
	a.active = false
	a.status = toolActivityOutcome(denied, err)
	if a.background && a.status == "done" {
		a.status = "unknown"
	}
}

func (row *toolDisclosureRow) hydrateAgentResult(msg messages.ChatMessage) {
	if row.agent == nil {
		return
	}
	a := row.agent
	a.active, a.status = false, "unknown"
	if toolWasDenied(msg.Content) {
		a.status = "denied"
	} else if succeeded, known := msg.ToolSucceeded(); known {
		if !succeeded {
			a.status = "failed"
		} else if !a.background {
			a.status = "done"
		}
	}
}

func turnAgentLabel(n int) string {
	if n == 1 {
		return "1 agent"
	}
	return fmt.Sprintf("%d agents", n)
}

// agentCounts tallies the agent rows behind a set of tool disclosures; the
// launch row's Agents disclosure reports them, running ones first, for as
// long as any child runs.
func (m *replModel) agentCounts(ids []int64) activityAgentCounts {
	var counts activityAgentCounts
	for _, id := range ids {
		record := m.toolDisclosures.get(id)
		if record == nil {
			continue
		}
		for _, row := range record.rows {
			if !row.isAgent() {
				continue
			}
			if row.agent == nil {
				counts.add("unknown", false)
				continue
			}
			counts.add(row.agent.status, row.agent.active)
		}
	}
	return counts
}

func (m *replModel) agentField(ids []int64, expanded bool) (turnDockField, bool) {
	c := m.agentCounts(ids)
	if c.Total == 0 {
		return turnDockField{}, false
	}
	return activityField(turnAgentSummaryLabel(c.Total, c.Running, c.Failed, c.Canceled, c.Paused), activityAgents, expanded), true
}

// turnAgentSummaryLabel composes the agent field's label from its counts,
// shared by the TUI launch row and the one-shot summary. Running agents lead
// while any child runs; afterwards one total with its failed and canceled
// tails.
func turnAgentSummaryLabel(total, running, failed, canceled, paused int) string {
	label := turnAgentLabel(total)
	if running > 0 {
		label = turnAgentLabel(running) + " running"
		if completed := total - running - failed - canceled - paused; completed > 0 {
			label += fmt.Sprintf(", %d completed", completed)
		}
	}
	if failed > 0 {
		label += fmt.Sprintf(", %d failed", failed)
	}
	if canceled > 0 {
		label += fmt.Sprintf(", %d canceled", canceled)
	}
	if paused > 0 {
		label += fmt.Sprintf(", %d paused", paused)
	}
	return label
}

func (m *replModel) hasAgentRows() bool {
	for _, record := range m.toolDisclosures.all() {
		for _, row := range record.rows {
			if row.isAgent() {
				return true
			}
		}
	}
	return false
}

func (m *replModel) agentsExpanded(ids []int64) bool {
	for _, id := range ids {
		if record := m.toolDisclosures.get(id); record != nil && record.agentsExpanded {
			return true
		}
	}
	return false
}

func agentActivityLine(a *agentActivity) string {
	status := sanitizeTranscriptImageText(a.status)
	glyph, color := "·", "muted"
	switch {
	case status == "approval needed":
		glyph, color = "!", "active"
	case a.active:
		glyph = " "
	case status == "done":
		glyph, color = "✓", "ok"
	case status == "failed" || status == "denied":
		glyph, color = "✗", "err"
	}
	label := styleEscape(a.label)
	if a.session != "" {
		label = link(a.label)
	}
	detail := " · " + status
	if a.inputTokens > 0 || a.outputTokens > 0 {
		detail += fmt.Sprintf(" · %s in / %s out", humanizeTokens(a.inputTokens), humanizeTokens(a.outputTokens))
	}
	return "  " + styled(glyph, color, "") + " " + label + styled(detail, "muted", "")
}

// agentDetail uses the normal cell wrapper for both display and link geometry.
// Only the accent label is clickable, including each wrapped fragment.
func (m *replModel) agentDetail(ids []int64, width int) (string, []agentLink) {
	// Keep interleaved workflow steps together without moving stored rows:
	// active tools and inspector links refer to their original row indices.
	type rowRef struct {
		record *toolDisclosureRecord
		index  int
	}
	var groups [][]rowRef
	workflowGroups := map[string]int{}
	for _, id := range ids {
		record := m.toolDisclosures.get(id)
		if record == nil {
			continue
		}
		for i, row := range record.rows {
			if !row.isAgent() || row.agent == nil {
				continue
			}
			group, exists := workflowGroups[row.agent.workflowID]
			if row.agent.workflowID == "" || !exists {
				group = len(groups)
				groups = append(groups, nil)
				if row.agent.workflowID != "" {
					workflowGroups[row.agent.workflowID] = group
				}
			}
			groups[group] = append(groups[group], rowRef{record, i})
		}
	}
	var lines []string
	var links []agentLink
	y := 0
	linkStyle := parseStyledCells(link("x"), ui.StyleClear)[0].Style
	for _, group := range groups {
		for n, ref := range group {
			row := ref.record.rows[ref.index]
			if n == 0 && row.agent.workflowID != "" {
				heading := "  " + styled("Workflow · "+sanitizeTranscriptImageText(row.agent.workflowName), "muted", "")
				lines = append(lines, heading)
				y += len(transcriptVisualRows(heading, ui.StyleClear, width))
			}
			line := agentActivityLine(row.agent)
			lines = append(lines, line)
			for _, cells := range transcriptVisualRows(line, ui.StyleClear, width) {
				x, start, end := 0, -1, 0
				for _, cell := range cells {
					w := transcriptCellWidth(cell)
					if row.agent.session != "" && cell.Style == linkStyle {
						if start < 0 {
							start = x
						}
						end = x + w
					}
					x += w
				}
				if start >= 0 && start < width && end > start {
					links = append(links, agentLink{recordID: ref.record.id, rowIndex: ref.index, X: start, Y: y, Cols: min(end, width) - start})
				}
				y++
			}
		}
	}
	return strings.Join(lines, "\n"), links
}

func (m *replModel) appendAgentDetail(block *transcriptDisplayBlock, ids []int64, width int) {
	detail, links := m.agentDetail(ids, width)
	if detail == "" {
		return
	}
	native := m.nativeImages && width >= minimumImageThumbnailCols
	rows, _ := transcriptBlockRowsWithImages(block.text, false, width, block.images, native, m.imageCellWidth, m.imageCellHeight)
	for i := range links {
		links[i].Y += len(rows)
	}
	block.agentLinks = links
	block.text += "\n" + detail
}

func (m *replModel) toggleAgentDisclosureGroup(ids []int64) bool {
	if _, ok := m.agentField(ids, false); !ok {
		return false
	}
	m.noteDisclosure(activityAgents, ids[0])
	expand := !m.agentsExpanded(ids)
	m.mutateAnchored(m.disclosureLayoutWidth(0), matchToolGroup(ids), func(bool) {
		for _, id := range ids {
			if record := m.toolDisclosures.get(id); record != nil {
				record.agentsExpanded = expand
			}
		}
		m.visual.invalidate()
	})
	return true
}

func (m *replModel) toggleAgentDisclosureAt(x, y int) bool {
	for _, p := range m.agentDisclosurePlacements {
		if p.Y == y && x >= p.X && x < p.X+p.Cols {
			return m.toggleAgentDisclosureGroup(p.recordIDs)
		}
	}
	return false
}

func (m *replModel) visibleAgentLinks(v transcriptViewport) []agentLink {
	var links []agentLink
	offset := 0
	for _, block := range m.visual.blocks {
		for _, link := range block.agentLinks {
			row := offset + link.Y
			if v.contains(row) {
				link.Y = v.screenY(row)
				links = append(links, link)
			}
		}
		offset += len(block.rows)
	}
	return links
}

func spawnOutcomeStatus(outcome sessions.ReportStatus) string {
	switch outcome {
	case sessions.ReportFinished:
		return "done"
	case sessions.ReportFailed:
		return "failed"
	case sessions.ReportCanceled:
		return "canceled"
	case sessions.ReportPaused:
		return "paused · iteration limit"
	default:
		return "unknown"
	}
}

// Saved metadata supplies identity and first-run outcomes without parsing
// model-visible result prose or treating a live lease as proof of activity.
func (m *replModel) hydrateAgentSessions(parent string, summaries []sessions.SessionSummary) {
	byCall := make(map[string][]sessions.SessionSummary)
	for _, summary := range summaries {
		md := summary.Metadata
		if md != nil && md.Parent == parent && md.SpawnCallID != "" {
			byCall[md.SpawnCallID] = append(byCall[md.SpawnCallID], summary)
		}
	}
	counts := make(map[string]int)
	for _, record := range m.toolDisclosures.all() {
		for _, row := range record.rows {
			if row.isAgent() {
				counts[row.callID]++
			}
		}
	}
	for _, record := range m.toolDisclosures.all() {
		for i := range record.rows {
			row := &record.rows[i]
			matches := byCall[row.callID]
			if row.agent == nil || counts[row.callID] != 1 || len(matches) != 1 {
				continue
			}
			md := matches[0].Metadata
			row.agent.viewID = matches[0].ID
			row.agent.session, row.agent.attached = md.Name, true
			row.agent.status, row.agent.active = spawnOutcomeStatus(md.SpawnOutcome), false
		}
	}
	m.visual.invalidate()
}

// refreshAgentActivities reads runtime state without tying execution to tabs.
func (r *managedREPL) refreshAgentActivities() { r.refreshSwarmActivities() }

func (m *replModel) refreshAgentRecord(record *toolDisclosureRecord) {
	m.mutateAnchored(m.disclosureLayoutWidth(0), matchToolGroup([]int64{record.id}), func(bool) { m.visual.invalidate() })
}

// openAgentAt is called with the visible model locked. Store validation runs
// off the UI loop and resolves the child again, so renames and deleted/reused
// session names cannot silently open an unrelated or newly created session.
func (r *managedREPL) openAgentAt(x, y int) bool {
	for _, link := range r.model.agentLinkPlacements {
		if link.Y == y && x >= link.X && x < link.X+link.Cols {
			return r.inspectAgent(r.model, tabViewTarget(r.visibleTab()), link)
		}
	}
	return false
}

// The lease acquisition checks the identity again after the asynchronous
// picker lookup. A deleted or reused name must not open a fresh conversation.
type agentSessionTargetKey struct{}
type agentSessionTarget struct{ parent, callID string }
