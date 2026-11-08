package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/subagent"
	"github.com/alexschlessinger/pollytool/swarm"
	rw "github.com/mattn/go-runewidth"
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
	// live is agentRowStatus while the agent runs here; liveWarn colors its
	// clock.
	live     string
	liveWarn bool
	// approval overlays the swarm facts: this model holds a request from the
	// agent that a person must answer.
	approval bool
	// decisions the parent owes this member's work; workflowDecisions the
	// ones its workflow owns as a whole (its acknowledgment). Headings sum them.
	decisions, workflowDecisions int
	// origin binds saved display rows to a live inspector across renames.
	origin *agentActivity
	// sessionSummaryBase is the immutable state before saved child metadata
	// enriched this row. Display copies can discard that enrichment without
	// guessing (or erasing) the transcript's launch outcome and identity.
	sessionSummaryBase *agentSessionBinding
}

type agentSessionBinding struct {
	viewID, session string
	attached        bool
	state           swarm.AgentPresentation
	local           string
}

func (a *agentActivity) clearSessionSummary() {
	if base := a.sessionSummaryBase; base != nil {
		a.viewID, a.session, a.attached = base.viewID, base.session, base.attached
		a.state, a.local = base.state, base.local
		a.sessionSummaryBase = nil
	}
}

func (m *replModel) clearAgentSessionSummaries() {
	changed := false
	for _, record := range m.toolDisclosures.all() {
		for i := range record.rows {
			if a := record.rows[i].agent; a != nil && a.sessionSummaryBase != nil {
				a.clearSessionSummary()
				changed = true
			}
		}
	}
	if changed {
		m.visual.invalidate()
	}
}

// agentLink is relative to its rendered block until projected into a viewport.
type agentLink struct {
	recordID int64
	rowIndex int
	X, Y     int
	Cols     int
}

func (row *toolDisclosureRow) setCall(call messages.ChatMessageToolCall) {
	if call.DisplayID != "" {
		row.displayID = call.DisplayID
	}
	row.toolName = call.Name
	if command, ok := bashCommandOf(call); ok {
		row.bash = newBashSummary(command)
	}
	if call.Arguments != "" {
		row.arguments = call.Arguments
		row.file = fileSummaryOf(call)
	}
	if call.Name != subagent.ToolName || row.agent != nil {
		return
	}
	var args struct {
		Label      string `json:"label"`
		Task       string `json:"task"`
		TaskName   string `json:"task_name"`
		Message    string `json:"message"`
		Background bool   `json:"background"`
	}
	_ = json.Unmarshal([]byte(call.Arguments), &args)
	if args.TaskName != "" {
		args.Background = true
		if args.Label == "" {
			args.Label = args.TaskName
		}
		args.Task = args.Message
	}
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
	if a.live != "" {
		return a.live
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

// agentActivityLine pads the label to labelWidth so the statuses of a block's
// rows share one column, as they do in the agents list.
func agentActivityLine(a *agentActivity, labelWidth int) string {
	status := style.SanitizeImageText(a.display())
	glyph, color := agentGlyph(a)
	label := style.Escape(a.label)
	if a.session != "" {
		label = style.Link(a.label)
	}
	label += strings.Repeat(" ", max(0, labelWidth-rw.StringWidth(a.label))+2)
	return style.Styled(glyph, color, "") + " " + label + rowStatusMarkup(status, "muted", a.liveWarn && !a.approval)
}

// agentLinkStyle is the cell style style.Link renders with, which agentDetail
// matches to find the clickable cells of a rendered row. It is resolved on
// every call rather than cached: it holds a resolved ui.Color, so a cache would
// stop matching freshly parsed link cells after a theme change and the links
// would silently stop being clickable (see applyStyleEpoch in
// repl_theme_epoch.go).
func agentLinkStyle() ui.Style {
	return style.ParseCells(style.Link("x"), ui.StyleClear)[0].Style
}

// agentDetail lists the agents behind ids as rows with their inspector
// links, each row behind prefix and wrapped at width with the normal cell
// wrapper, so display and link geometry agree. Only the accent label is
// clickable, including each wrapped fragment. Outside full depth, only the
// newest activityAgentWindow agents and every busy one appear, selected
// before workflow grouping, until a click opens the cut.
func (m *replModel) agentDetail(ids []int64, width int, prefix string) (string, []agentLink) {
	detail, links, _ := m.agentDetailCut(ids, width, prefix)
	return detail, links
}

// agentDetailCut is agentDetail with its cut control: the detail lines
// that carry it, the cut marked on each.
func (m *replModel) agentDetailCut(ids []int64, width int, prefix string) (string, []agentLink, []int) {
	// Keep interleaved workflow steps together without moving stored rows:
	// active tools and inspector links refer to their original row indices.
	type rowRef struct {
		record *toolDisclosureRecord
		index  int
		order  int
	}
	total := 0
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
			groups[group] = append(groups[group], rowRef{record, i, total})
			total++
		}
	}
	cuts := m.depth != depthFull && total > activityAgentWindow
	open := len(ids) > 0 && m.openCuts[agentsCutKey(ids)]
	shown := make([]bool, total)
	labelWidth := 0
	for _, group := range groups {
		for _, ref := range group {
			a := ref.record.rows[ref.index].agent
			labelWidth = max(labelWidth, rw.StringWidth(a.label))
			shown[ref.order] = !cuts || open || ref.order >= total-activityAgentWindow || a.busy()
		}
	}
	var controls []int
	var lines []string
	var links []agentLink
	y := 0
	linkStyle := agentLinkStyle()
	// appendLine renders one line and records its link cells, one link per
	// wrapped fragment, while advancing the running row count.
	appendLine := func(line string, link agentLink, linked bool, cut bool) {
		if cut {
			line = style.Styled(reasoningCutIndent, "muted", "") + line
		} else {
			line = prefix + line
		}
		lines = append(lines, line)
		for _, cells := range style.VisualRows(line, ui.StyleClear, width) {
			x, start, end := 0, -1, 0
			for _, cell := range cells {
				w := style.CellWidth(cell)
				if linked && cell.Style == linkStyle {
					if start < 0 {
						start = x
					}
					end = x + w
				}
				x += w
			}
			if start >= 0 && start < width && end > start {
				link.X, link.Y, link.Cols = start, y, min(end, width)-start
				links = append(links, link)
			}
			y++
		}
	}
	// The row after hidden agents marks the cut; an open cut closes from
	// its first row.
	marked := func(order int) bool { return order > 0 && !shown[order-1] }
	for _, group := range groups {
		if !slices.ContainsFunc(group, func(ref rowRef) bool { return shown[ref.order] }) {
			continue
		}
		first := group[0]
		agent := first.record.rows[first.index].agent
		if agent.workflowID != "" {
			need := agent.workflowDecisions
			for _, ref := range group {
				need += ref.record.rows[ref.index].agent.decisions
			}
			heading := style.Styled("Workflow · "+style.SanitizeImageText(agent.workflowName), "muted", "")
			if need > 0 {
				heading += style.Styled(" · "+needsLabel(need, "decision"), "active", "")
			}
			if cuts && open && len(lines) == 0 {
				controls = append(controls, 0)
			}
			appendLine(heading, agentLink{}, false, false)
		}
		for _, ref := range group {
			if !shown[ref.order] {
				continue
			}
			cut := cuts && !open && marked(ref.order)
			if cut || (cuts && open && len(lines) == 0) {
				controls = append(controls, len(lines))
			}
			row := ref.record.rows[ref.index]
			appendLine(agentActivityLine(row.agent, labelWidth), agentLink{recordID: ref.record.id, rowIndex: ref.index}, row.agent.session != "", cut)
		}
	}
	return strings.Join(lines, "\n"), links, controls
}

// agentsCutKey and imagesCutKey name a line's agents and images cuts.
func agentsCutKey(ids []int64) string { return fmt.Sprintf("agents:%d", ids[0]) }
func imagesCutKey(ids []int64) string { return fmt.Sprintf("images:%d", ids[0]) }

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
	m.clearAgentSessionSummaries()
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
			a := row.agent
			a.sessionSummaryBase = &agentSessionBinding{viewID: a.viewID, session: a.session, attached: a.attached, state: a.state, local: a.local}
			row.agent.viewID = matches[0].ID
			row.agent.session, row.agent.attached = md.Name, true
			row.agent.state, row.agent.local = spawnOutcomeState(md.SpawnOutcome), ""
		}
	}
	m.visual.invalidate()
}

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
