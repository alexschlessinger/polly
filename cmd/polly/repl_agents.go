package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/subagent"
	ui "github.com/metaspartan/gotui/v5"
)

// agentActivity belongs to the spawn request's disclosure, not its tool
// execution. A background tool result can settle long before this run does.
type agentActivity struct {
	label      string
	status     string
	session    string
	background bool
	active     bool
	attached   bool
	// origin binds a restored row to the same live run across child renames.
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

func (row *toolDisclosureRow) isAgent() bool { return row.toolName == subagent.ToolName }

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
	switch {
	case denied:
		a.status = "denied"
	case errors.Is(err, context.Canceled):
		a.status = "canceled"
	case err != nil:
		a.status = "failed"
	case a.background:
		a.status = "unknown"
	default:
		a.status = "done"
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

// agentField renders the Agents control. A running child lights exactly one
// control: the launch row while its turn runs, then that turn's settled
// trailer. settled dims the inline copy once the trailer owns liveness.
func (m *replModel) agentField(ids []int64, expanded, settled bool) (turnDockField, bool) {
	n, active := 0, false
	for _, id := range ids {
		if record := m.toolDisclosures[id]; record != nil {
			for _, row := range record.rows {
				if row.isAgent() {
					n++
					active = active || (row.agent != nil && row.agent.active)
				}
			}
		}
	}
	if n == 0 {
		return turnDockField{}, false
	}
	glyph, label := "▸", turnAgentLabel(n)
	if expanded {
		glyph = "▾"
	}
	return turnDockField{raw: glyph + " " + label, rendered: inlineActivityControl(glyph, label, settled || !active), overlay: turnDockOverlayAgents}, true
}

func (m *replModel) hasAgentRows() bool {
	for _, record := range m.toolDisclosures {
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
		if record := m.toolDisclosures[id]; record != nil && record.agentsExpanded {
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
		glyph, color = "↻", "run"
	case status == "done":
		glyph, color = "✓", "ok"
	case status == "failed" || status == "denied":
		glyph, color = "✗", "err"
	}
	label := styleEscape(a.label)
	if a.session != "" {
		label = styled(a.label, "accent", "underline")
	}
	return "  " + styled(glyph, color, "") + " " + label + styled(" · "+status, "muted", "")
}

// agentDetail uses the normal cell wrapper for both display and link geometry.
// Only the underlined label is clickable, including each wrapped fragment.
func (m *replModel) agentDetail(ids []int64, width int) (string, []agentLink) {
	var lines []string
	var links []agentLink
	y := 0
	linkStyle := parseStyledCells(styled("x", "accent", "underline"), ui.StyleClear)[0].Style
	for _, id := range ids {
		record := m.toolDisclosures[id]
		if record == nil {
			continue
		}
		for i, row := range record.rows {
			if !row.isAgent() || row.agent == nil {
				continue
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
					links = append(links, agentLink{recordID: id, rowIndex: i, X: start, Y: y, Cols: min(end, width) - start})
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
	if _, ok := m.agentField(ids, false, false); !ok {
		return false
	}
	expand := !m.agentsExpanded(ids)
	m.mutateAnchored(m.disclosureLayoutWidth(0), matchToolGroup(ids), func(bool) {
		for _, id := range ids {
			if record := m.toolDisclosures[id]; record != nil {
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
	default:
		return "unknown"
	}
}

// Saved metadata supplies identity and first-run outcomes without parsing
// model-visible result prose or treating a live lease as proof of activity.
func (m *replModel) hydrateAgentSessions(parent string, summaries []sessions.SessionSummary) {
	byCall := make(map[string][]*sessions.Metadata)
	for _, summary := range summaries {
		md := summary.Metadata
		if md != nil && md.Parent == parent && md.SpawnCallID != "" {
			byCall[md.SpawnCallID] = append(byCall[md.SpawnCallID], md)
		}
	}
	counts := make(map[string]int)
	for _, record := range m.toolDisclosures {
		for _, row := range record.rows {
			if row.isAgent() {
				counts[row.callID]++
			}
		}
	}
	for _, record := range m.toolDisclosures {
		for i := range record.rows {
			row := &record.rows[i]
			matches := byCall[row.callID]
			if row.agent == nil || counts[row.callID] != 1 || len(matches) != 1 {
				continue
			}
			md := matches[0]
			row.agent.session, row.agent.attached = md.Name, true
			row.agent.status, row.agent.active = spawnOutcomeStatus(md.SpawnOutcome), false
		}
	}
	m.visual.invalidate()
}

// refreshAgentActivities runs before painting, with no model lock held. It
// samples each child separately, then updates its original display records.
func (r *managedREPL) refreshAgentActivities() {
	pending := r.pendingAgentUpdates[:0]
	for _, child := range r.pendingAgentUpdates {
		if !r.updateChildAgent(child) {
			pending = append(pending, child)
		}
	}
	clear(r.pendingAgentUpdates[len(pending):])
	r.pendingAgentUpdates = pending
	for _, child := range r.tabs {
		if child.agentActivity == nil {
			continue
		}
		if child.report != nil {
			if child.model.mu.TryLock() {
				status := "working"
				if child.model.approval != nil {
					status = "approval needed"
				} else if child.model.busy {
					status = child.model.busyLabel()
				}
				child.model.mu.Unlock()
				child.agentStatus, child.agentActive = status, true
			}
		}
		r.updateChildAgent(child)
	}
}

func (r *managedREPL) updateChildAgent(child *replTab) bool {
	updated := true
	for _, parent := range r.tabs {
		m := parent.model
		if m == child.model {
			continue
		}
		if !m.mu.TryLock() {
			updated = false
			continue
		}
		for _, record := range m.toolDisclosures {
			changed := false
			for i := range record.rows {
				row := &record.rows[i]
				a := row.agent
				if a == nil || (a != child.agentActivity && a.origin != child.agentActivity && !(a.session == child.name && row.callID == child.spawnCallID && a.session != "")) {
					continue
				}
				if a != child.agentActivity {
					a.origin = child.agentActivity
				}
				if a.session != child.name || a.status != child.agentStatus || a.active != child.agentActive {
					a.session, a.status, a.active, a.attached = child.name, child.agentStatus, child.agentActive, true
					changed = true
				}
			}
			if changed {
				m.refreshAgentRecord(record)
			}
		}
		m.mu.Unlock()
	}
	return updated
}

func (m *replModel) refreshAgentRecord(record *toolDisclosureRecord) {
	m.mutateAnchored(m.disclosureLayoutWidth(0), matchToolGroup([]int64{record.id}), func(bool) { m.visual.invalidate() })
	for _, trailer := range m.turnTrailers {
		if slices.Contains(trailer.dock.toolIDs, record.id) {
			m.refreshTurnTrailer(trailer)
		}
	}
}

func recordSpawnOutcome(session sessions.Session, outcome sessions.ReportStatus) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return updateMetadata(ctx, session, func(md *sessions.Metadata) {
		if md.SpawnCallID != "" && md.SpawnOutcome == "" {
			md.SpawnOutcome = outcome
		}
	})
}

func (r *managedREPL) finishChildAgent(child *replTab, err error) {
	if child.spawnCallID == "" {
		return
	}
	outcome := storedChildReport(subagent.Result{}, err).Status
	child.agentStatus, child.agentActive = spawnOutcomeStatus(outcome), false
	if !r.updateChildAgent(child) {
		r.pendingAgentUpdates = append(r.pendingAgentUpdates, child)
	}
	done := make(chan struct{})
	child.agentWriteDone = done
	if !r.background(func() {
		defer close(done)
		if err := recordSpawnOutcome(child.state.session, outcome); err != nil {
			r.work.recordError(fmt.Errorf("save agent outcome: %w", err))
		}
	}) {
		close(done)
	}
}

// openAgentAt is called with the visible model locked. Store validation runs
// off the UI loop and resolves the child again, so renames and deleted/reused
// session names cannot silently open an unrelated or newly created session.
func (r *managedREPL) openAgentAt(x, y int) bool {
	m := r.model
	for _, link := range m.agentLinkPlacements {
		if link.Y != y || x < link.X || x >= link.X+link.Cols {
			continue
		}
		record := m.toolDisclosures[link.recordID]
		if record == nil || link.rowIndex >= len(record.rows) {
			return false
		}
		row := record.rows[link.rowIndex]
		if row.agent == nil || row.agent.session == "" || r.state == nil {
			return false
		}
		for i, tab := range r.tabs {
			if tab.agentActivity == row.agent {
				r.requestShowTabLocked(i)
				return true
			}
		}
		parent, store := r.visibleTab(), r.state.sessionStore
		if store == nil {
			return true
		}
		parentName, callID := parent.name, row.callID
		r.background(func() {
			summaries, err := store.ListSummaries(r.work.ctx)
			r.postUI(r.work.ctx, func() {
				if r.tabIndexOfModel(m) < 0 {
					return
				}
				m.mu.Lock()
				defer m.mu.Unlock()
				if err != nil {
					m.appendNoticeLine("agent session: " + err.Error())
					return
				}
				var matches []sessions.SessionSummary
				for _, summary := range summaries {
					md := summary.Metadata
					if md != nil && md.Parent == parentName && md.SpawnCallID == callID {
						matches = append(matches, summary)
					}
				}
				if len(matches) != 1 {
					m.appendNoticeLine("agent session is missing or ambiguous")
					return
				}
				target := matches[0]
				if m != r.model {
					return
				}
				if i := r.tabIndexOf(target.Metadata.Name); i >= 0 {
					r.requestShowTabLocked(i)
					return
				}
				if target.InUse {
					m.appendNoticeLine("could not open " + target.Metadata.Name + ": it is open in another polly")
					return
				}
				if r.opener == nil || !r.canOpenLocked() {
					return
				}
				ctx := context.WithValue(r.runCtx, agentSessionTargetKey{}, agentSessionTarget{parentName, callID})
				r.beginOpenContextLocked(ctx, target.Metadata.Name, false)
			})
		})
		return true
	}
	return false
}

// The lease acquisition checks the identity again after the asynchronous
// picker lookup. A deleted or reused name must not open a fresh conversation.
type agentSessionTargetKey struct{}
type agentSessionTarget struct{ parent, callID string }
