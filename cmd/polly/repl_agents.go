package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/subagent"
	"github.com/alexschlessinger/pollytool/swarm"
	ui "github.com/metaspartan/gotui/v5"
)

// agentActivity belongs to the spawn request's disclosure, not its tool
// execution. A background tool result can settle long before this run does.
type agentActivity struct {
	viewID                   string
	label                    string
	session                  string
	background               bool
	attached                 bool
	workflowID, workflowName string
	// state is what the row says about its agent: the swarm's presentation
	// once attached to a member, else built from the launch call's outcome.
	// Rows decide on its typed fields, never on its label.
	state swarm.AgentPresentation
	// local is the launch-call word behind state for rows without a member
	// ("starting", "done", "denied", ...). It empties once a presentation
	// from the swarm or from saved metadata owns the row.
	local string
	// approval overlays the swarm facts: this model holds a request from the
	// agent that a person must answer.
	approval bool

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
	label := style.SanitizeImageText(spawnLabel(args.Label, args.Task))
	if label == "" {
		label = "agent"
	}
	row.agent = &agentActivity{label: label, background: args.Background}
	row.agent.setLocal("starting", true)
}

func (a *agentActivity) display() string {
	if a.approval {
		return "approval needed"
	}
	return a.state.Display
}

func (a *agentActivity) busy() bool { return a.state.Busy }

func (a *agentActivity) setLocal(word string, busy bool) {
	a.local, a.state = word, localAgentState(word, busy)
}

// localAgentState maps a launch call's own vocabulary (starting, done,
// failed, denied, canceled, unknown, paused …) onto presentation facts so
// glyphs and counters have one input. Display keeps the word.
func localAgentState(word string, busy bool) swarm.AgentPresentation {
	p := swarm.AgentPresentation{Display: word}
	switch {
	case busy:
		p.Lifecycle, p.Busy = swarm.LifecycleActive, true
	case word == "done":
		p.Lifecycle, p.TaskStatus, p.Detail = swarm.LifecycleIdle, "done", "done"
	case word == "failed", word == "denied":
		p.Lifecycle, p.Outcome, p.Detail = swarm.LifecyclePaused, "failed", word
	case word == "canceled":
		p.Lifecycle, p.Detail = swarm.LifecyclePaused, "canceled"
	case strings.HasPrefix(word, "paused"):
		p.Lifecycle, p.Outcome, p.Detail = swarm.LifecyclePaused, "paused", strings.TrimPrefix(word, "paused · ")
	default:
		p.Lifecycle = swarm.LifecycleIdle
	}
	return p
}

// agentGlyph marks a row by what happened, not by how it is worded.
func agentGlyph(a *agentActivity) (glyph, color string) {
	switch {
	case a.approval:
		return "!", "active"
	case a.state.Busy:
		return " ", "muted"
	case a.state.Outcome == "failed":
		return "✗", "err"
	case a.state.Lifecycle == swarm.LifecycleIdle && a.state.TaskStatus == "done":
		return "✓", "ok"
	}
	return "·", "muted"
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
	word := toolActivityOutcome(denied, err)
	if a.background && word == "done" {
		word = "unknown"
	}
	a.setLocal(word, false)
}

func (row *toolDisclosureRow) hydrateAgentResult(msg messages.ChatMessage) {
	if row.agent == nil {
		return
	}
	a := row.agent
	word := "unknown"
	if toolWasDenied(msg.Content) {
		word = "denied"
	} else if succeeded, known := msg.ToolSucceeded(); known {
		if !succeeded {
			word = "failed"
		} else if !a.background {
			word = "done"
		}
	}
	a.setLocal(word, false)
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
				counts.addOutcome("unknown", false)
				continue
			}
			if row.agent.local != "" {
				counts.addOutcome(row.agent.local, row.agent.busy())
				continue
			}
			counts.add(row.agent.state)
		}
	}
	return counts
}

func (m *replModel) agentField(ids []int64, expanded bool) (turnDockField, bool) {
	c := m.agentCounts(ids)
	if c.Total == 0 {
		return turnDockField{}, false
	}
	return activityField(turnAgentSummaryLabel(c.Total, c.Running, c.Failed, c.Canceled, c.Paused, c.Deferred), activityAgents, expanded), true
}

// turnAgentSummaryLabel composes the agent field's label from its counts,
// shared by the TUI launch row and the one-shot summary. Running agents lead
// while any child runs; afterwards one total with its failed and canceled
// tails.
func turnAgentSummaryLabel(total, running, failed, canceled, paused int, deferred ...int) string {
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
	if len(deferred) > 0 && deferred[0] > 0 {
		label += fmt.Sprintf(", %d deferred", deferred[0])
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
	status := style.SanitizeImageText(a.display())
	glyph, color := agentGlyph(a)
	label := style.Escape(a.label)
	if a.session != "" {
		label = style.Link(a.label)
	}
	detail := " · " + status
	if a.inputTokens > 0 || a.outputTokens > 0 {
		detail += fmt.Sprintf(" · %s in / %s out", humanizeTokens(a.inputTokens), humanizeTokens(a.outputTokens))
	}
	return "  " + style.Styled(glyph, color, "") + " " + label + style.Styled(detail, "muted", "")
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
	linkStyle := style.ParseCells(style.Link("x"), ui.StyleClear)[0].Style
	for _, group := range groups {
		for n, ref := range group {
			row := ref.record.rows[ref.index]
			if n == 0 && row.agent.workflowID != "" {
				heading := "  " + style.Styled("Workflow · "+style.SanitizeImageText(row.agent.workflowName), "muted", "")
				lines = append(lines, heading)
				y += len(style.VisualRows(heading, ui.StyleClear, width))
			}
			line := agentActivityLine(row.agent)
			lines = append(lines, line)
			for _, cells := range style.VisualRows(line, ui.StyleClear, width) {
				x, start, end := 0, -1, 0
				for _, cell := range cells {
					w := style.CellWidth(cell)
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
	native := m.nativeImages && width >= style.MinimumThumbnailCols
	rows, _ := transcriptBlockRowsWithImages(block.text, false, width, block.images, native, m.imageCellWidth, m.imageCellHeight)
	for i := range links {
		links[i].Y += len(rows)
	}
	block.agentLinks = links
	block.text += "\n" + detail
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

// spawnOutcomeState presents a saved first-run outcome in the lifecycle
// vocabulary, so archived rows read like live ones.
func spawnOutcomeState(outcome sessions.ReportStatus) swarm.AgentPresentation {
	var p swarm.AgentPresentation
	switch outcome {
	case sessions.ReportFinished:
		p.Lifecycle, p.TaskStatus, p.Detail = swarm.LifecycleIdle, "done", "done"
	case sessions.ReportFailed:
		p.Lifecycle, p.Outcome, p.Detail = swarm.LifecyclePaused, "failed", "failed"
	case sessions.ReportCanceled:
		p.Lifecycle, p.Outcome, p.Detail = swarm.LifecyclePaused, "paused", "interrupted"
	case sessions.ReportPaused:
		p.Lifecycle, p.Outcome, p.StopReason, p.Detail = swarm.LifecyclePaused, "paused", string(messages.StopReasonMaxIterations), "iteration limit"
	default:
		p.Lifecycle, p.Display = swarm.LifecycleIdle, "unknown"
		return p
	}
	p.Display = swarm.DisplayLabel(p.Lifecycle, p.Detail, false)
	return p
}

func spawnOutcomeStatus(outcome sessions.ReportStatus) string {
	return spawnOutcomeState(outcome).Display
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
			row.agent.state, row.agent.local = spawnOutcomeState(md.SpawnOutcome), ""
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
