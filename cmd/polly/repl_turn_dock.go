package main

import (
	"fmt"
	"strings"
	"time"

	rw "github.com/mattn/go-runewidth"
)

// activityKind names one kind of turn activity an inline row can disclose.
type activityKind int

const (
	activityNone activityKind = iota
	activityThought
	activityTools
	activityAgents
	activityImages
)

type turnDockState struct {
	visible      bool
	settled      bool
	outcome      turnOutcome
	elapsed      time.Duration
	elapsedKnown bool
	inputTokens  int
	outputTokens int
	reasoningIDs []int64 // every reasoning record opened during the turn
	toolIDs      []int64 // every tool disclosure opened during the turn
}

type turnDockPlacement struct {
	kind activityKind
	X, Y int
	Cols int
}

// turnTrailerRecord is the settled status row a turn leaves in the
// transcript: outcome, elapsed time, and tokens. The turn's activity stays
// inline where it ran, with its own disclosures.
type turnTrailerRecord struct {
	transcriptAnchor
	dock turnDockState
}

type turnDockField struct {
	raw       string
	rendered  string
	kind      activityKind
	expanded  bool
	optional  bool
	protected bool
	elapsed   string
	outcome   turnOutcome
}

// activityField is one disclosure on an inline activity row: a muted label
// whose hitbox toggles the detail beneath the row.
func activityField(label string, kind activityKind, expanded bool) turnDockField {
	return turnDockField{raw: label, rendered: styled(label, "muted", ""), kind: kind, expanded: expanded}
}

// activityRowHeader is the collapsed activity row for one disclosure: the
// accent triangle, then the muted label. Rows with several disclosures share
// the triangle and join their labels with dots; see renderActivityRow.
func activityRowHeader(glyph, label string) string {
	return "  " + styled(glyph, "accent", "bold") + " " + styled(label, "muted", "")
}

// toolRowCount counts the ordinary (non-agent) tool rows behind a set of
// disclosures; the Tools label and the child summary both report it.
func (m *replModel) toolRowCount(ids []int64) int {
	total := 0
	for _, id := range ids {
		if record := m.toolDisclosures.get(id); record != nil {
			total += len(ordinaryToolRows(record.rows))
		}
	}
	return total
}

// activitySummaryFor is the TUI side of the parity seam with the one-shot
// frontend: both account a turn's activity the same way.
func (m *replModel) activitySummaryFor(dock turnDockState) turnActivitySummary {
	s := turnActivitySummary{Tools: m.toolRowCount(dock.toolIDs), Agents: m.agentCounts(dock.toolIDs), Outcome: dock.outcome, Elapsed: m.turnDockElapsedFor(dock), In: dock.inputTokens, Out: dock.outputTokens}
	for _, id := range dock.toolIDs {
		if record := m.toolDisclosures.get(id); record != nil {
			for _, row := range record.rows {
				s.Images += len(row.inspectionImages)
			}
		}
	}
	for _, id := range dock.reasoningIDs {
		record := m.reasoningRecords.get(id)
		if record == nil || len(record.tail) == 0 {
			continue
		}
		s.Reasoned = true
		s.Thought += record.elapsed
		if record.active && !m.thinkingSegmentStart.IsZero() && record.id == m.turnReasoningID {
			s.Thought += time.Since(m.thinkingSegmentStart)
		}
	}
	return s
}

func turnToolLabel(total int) string {
	if total == 1 {
		return "1 tool"
	}
	return fmt.Sprintf("%d tools", total)
}

func turnImageLabel(total int) string {
	if total == 1 {
		return "1 image viewed"
	}
	return fmt.Sprintf("%d images viewed", total)
}

func (m *replModel) startTurnDock() {
	m.turnDock = turnDockState{visible: !m.quiet, elapsedKnown: true}
}

func (m *replModel) settleTurnDock() {
	if !m.turnDock.visible {
		return
	}
	m.turnDock.settled = true
	m.turnDock.outcome = m.lastOutcome
	m.turnDock.elapsed = m.lastElapsed
	m.turnDock.elapsedKnown = true
	m.turnDock.inputTokens = m.lastIn
	m.turnDock.outputTokens = m.lastOut
}

func (m *replModel) clearTurnDock() {
	m.turnDock = turnDockState{}
}

func (m *replModel) turnDockElapsedFor(dock turnDockState) time.Duration {
	if dock.settled {
		return dock.elapsed
	}
	if m.turnStarted.IsZero() {
		return 0
	}
	return time.Since(m.turnStarted)
}

// turnDockStatusFields renders the status tail shared by the live dock and
// settled trailers. The live dock carries only token counts: the running
// elapsed time lives solely in the status row at the lower left, so the two
// never duplicate. Settled trailers keep their final elapsed time (with the
// outcome glyph) because the status row has moved on by then.
func (m *replModel) turnDockStatusFields(dock turnDockState) []turnDockField {
	var fields []turnDockField
	if dock.settled && dock.elapsedKnown {
		fields = append(fields, turnOutcomeField(dock.outcome, formatElapsed(m.turnDockElapsedFor(dock))))
	} else if dock.settled && dock.outcome != turnOutcomeNone {
		fields = append(fields, turnOutcomeField(dock.outcome, ""))
	}

	in, out := dock.inputTokens, dock.outputTokens
	if !dock.settled {
		in, out = m.lastIn, m.lastOut
	}
	if field, ok := turnTokenField(in, out); ok {
		fields = append(fields, field)
	}
	return fields
}

// turnOutcomeField is the settled outcome glyph with the turn's elapsed time,
// shared by the TUI trailer and the one-shot summary.
func turnOutcomeField(outcome turnOutcome, elapsed string) turnDockField {
	raw, rendered := elapsed, styled(elapsed, "muted", "")
	tail := ""
	if elapsed != "" {
		tail = " · " + elapsed
	}
	switch outcome {
	case turnOutcomeDone:
		raw = strings.TrimSpace("✓ " + elapsed)
		rendered = styled("✓", "ok", "bold")
		if elapsed != "" {
			rendered += " " + styled(elapsed, "muted", "")
		}
	case turnOutcomeFailed:
		raw = "✗ failed" + tail
		rendered = styled("✗ failed", "err", "bold") + styled(tail, "muted", "")
	case turnOutcomeCanceled:
		raw = "canceled" + tail
		rendered = styled("canceled", "muted", "bold") + styled(tail, "muted", "")
	case turnOutcomeIncomplete:
		raw = "incomplete" + tail
		rendered = styled("incomplete", "active", "bold") + styled(tail, "muted", "")
	}
	return turnDockField{raw: raw, rendered: rendered, protected: true, elapsed: elapsed, outcome: outcome}
}

// turnTokenField is the optional token-count tail of a status row.
func turnTokenField(in, out int) (turnDockField, bool) {
	if in <= 0 && out <= 0 {
		return turnDockField{}, false
	}
	raw := fmt.Sprintf("%s in / %s out", humanizeTokens(in), humanizeTokens(out))
	return turnDockField{raw: raw, rendered: styled(raw, "muted", ""), optional: true}, true
}

func (m *replModel) setHydratedTurnDock(reasoning *reasoningRecord, tools *toolDisclosureRecord, in, out int) {
	m.turnDock = turnDockState{
		visible:      !m.quiet,
		settled:      true,
		outcome:      turnOutcomeDone,
		inputTokens:  in,
		outputTokens: out,
	}
	if reasoning != nil {
		m.turnDock.reasoningIDs = []int64{reasoning.id}
	}
	if tools != nil {
		m.turnDock.toolIDs = []int64{tools.id}
	}
}

func (m *replModel) turnDockRow(width int) (string, []turnDockPlacement) {
	if !m.turnDock.visible || width <= 0 {
		return "", nil
	}
	return m.turnDockRowFor(m.turnDock, width)
}

func (m *replModel) turnDockRowFor(dock turnDockState, width int) (string, []turnDockPlacement) {
	if width <= 0 {
		return "", nil
	}
	return renderTurnActivityRow(m.turnDockStatusFields(dock), width)
}

// turnDockFieldsWidth is the rendered width of an activity row: the indent,
// the raw fields, and a separator between each pair. The TUI dock and the
// one-shot status row fit their fields with the same measure.
func turnDockFieldsWidth(fields []turnDockField) int {
	if len(fields) == 0 {
		return 0
	}
	width := rw.StringWidth("  ") + (len(fields)-1)*rw.StringWidth(" · ")
	for _, f := range fields {
		width += rw.StringWidth(f.raw)
	}
	return width
}

// renderTurnActivityRow is the one-line status renderer shared by the live
// dock, the settled trailer, and the one-shot frontend. Fields that do not
// fit are truncated as one row; placements are returned only for controls
// wholly on that row.
func renderTurnActivityRow(fields []turnDockField, width int) (string, []turnDockPlacement) {
	if width <= 0 {
		return "", nil
	}
	const indent = "  "
	const separator = " · "
	fields = append([]turnDockField(nil), fields...)
	measure := turnDockFieldsWidth
	for measure(fields) > width {
		removed := false
		for i := len(fields) - 1; i >= 0; i-- {
			if !fields[i].optional {
				continue
			}
			fields = append(fields[:i], fields[i+1:]...)
			removed = true
			break
		}
		if !removed {
			break
		}
	}
	// Keep the outcome and elapsed time visible. Activity controls may lose
	// detail on a narrow row, but may not push the result off its right edge.
	protected := 0
	for _, field := range fields {
		if field.protected {
			protected += rw.StringWidth(field.raw)
		}
	}
	if protected > 0 {
		for measure(fields) > width && len(fields) > 1 {
			pick := -1
			for i, f := range fields {
				if !f.protected {
					pick = i
					break
				}
			}
			if pick < 0 {
				break
			}
			excess := measure(fields) - width
			f := fields[pick]
			available := rw.StringWidth(f.raw) - excess
			if available >= 3 {
				f.raw = rw.Truncate(f.raw, available, "…")
				f.rendered = styled(f.raw, "accent", "")
				f.kind = activityNone
				fields[pick] = f
			} else {
				fields = append(fields[:pick], fields[pick+1:]...)
			}
		}
	}

	var raw, rendered strings.Builder
	raw.WriteString(indent)
	rendered.WriteString(indent)
	var placements []turnDockPlacement
	for i, field := range fields {
		if i > 0 {
			raw.WriteString(separator)
			rendered.WriteString(styled(separator, "muted", ""))
		}
		start := rw.StringWidth(raw.String())
		raw.WriteString(field.raw)
		rendered.WriteString(field.rendered)
		if field.kind != activityNone && start+rw.StringWidth(field.raw) <= width {
			placements = append(placements, turnDockPlacement{
				kind: field.kind,
				X:    start,
				Cols: rw.StringWidth(field.raw),
			})
		}
	}
	if rw.StringWidth(raw.String()) > width {
		// The ellipsis occupies the final cell, so retain only controls that
		// remain wholly visible before it. Keeping those placements prevents
		// the inline fallback from turning the entire truncated row into
		// overlapping reasoning and tool targets.
		visibleWidth := width - rw.StringWidth("…")
		visiblePlacements := placements[:0]
		for _, placement := range placements {
			if placement.X+placement.Cols <= visibleWidth {
				visiblePlacements = append(visiblePlacements, placement)
			}
		}
		return styled(rw.Truncate(raw.String(), width, "…"), "muted", ""), visiblePlacements
	}
	return rendered.String(), placements
}

// renderActivityRow lays out one inline activity row: the accent triangle
// (down when any disclosure is expanded), then the muted labels joined by
// dots. Each label is a hitbox; the triangle belongs to the first one. A row
// wider than the terminal clips with an ellipsis and keeps only the hitboxes
// that remain wholly visible.
func renderActivityRow(expanded bool, fields []turnDockField, width int) (string, []turnDockPlacement) {
	if width <= 0 || len(fields) == 0 {
		return "", nil
	}
	const indent = "  "
	const separator = " · "
	glyph := "▸"
	if expanded {
		glyph = "▾"
	}
	prefix := indent + glyph + " "
	header := indent + styled(glyph, "accent", "bold") + " "
	var raw, rendered strings.Builder
	var placements []turnDockPlacement
	for i, field := range fields {
		if i > 0 {
			raw.WriteString(separator)
			rendered.WriteString(styled(separator, "muted", ""))
		}
		start := rw.StringWidth(prefix) + rw.StringWidth(raw.String())
		raw.WriteString(field.raw)
		rendered.WriteString(styled(field.raw, "muted", ""))
		cols := rw.StringWidth(field.raw)
		if field.kind == activityNone || start+cols > width {
			continue
		}
		placement := turnDockPlacement{kind: field.kind, X: start, Cols: cols}
		if i == 0 {
			placement.X = rw.StringWidth(indent)
			placement.Cols += rw.StringWidth(glyph + " ")
		}
		placements = append(placements, placement)
	}
	if rw.StringWidth(prefix)+rw.StringWidth(raw.String()) > width {
		visibleWidth := width - rw.StringWidth("…")
		visiblePlacements := placements[:0]
		for _, placement := range placements {
			if placement.X+placement.Cols <= visibleWidth {
				visiblePlacements = append(visiblePlacements, placement)
			}
		}
		room := width - rw.StringWidth(prefix)
		if room < 1 {
			return styled(rw.Truncate(prefix, width, "…"), "muted", ""), nil
		}
		return header + styled(rw.Truncate(raw.String(), room, "…"), "muted", ""), visiblePlacements
	}
	return header + rendered.String(), placements
}

// attachTurnDockTrailer leaves the settled status row in the transcript. A
// bare outcome glyph says nothing the reply did not, so the row appears only
// with an elapsed time, a token count, or an outcome other than success.
func (m *replModel) attachTurnDockTrailer() {
	if !m.turnDock.visible || !m.turnDock.settled {
		m.clearTurnDock()
		return
	}
	dock := m.turnDock
	fields := m.turnDockStatusFields(dock)
	if len(fields) == 0 || (len(fields) == 1 && fields[0].outcome == turnOutcomeDone && fields[0].elapsed == "") {
		m.clearTurnDock()
		return
	}
	text, _ := renderTurnActivityRow(fields, m.disclosureLayoutWidth(0))
	if text == "" {
		m.clearTurnDock()
		return
	}
	m.appendLine(text)
	m.turnTrailers.add(&turnTrailerRecord{dock: dock}, len(m.transcript)-1)
	m.clearTurnDock()
}

// boundedReasoningDetail keeps the newest already-wrapped physical rows from
// a merged inline reasoning disclosure. Individual records are bounded before
// projection; the merged group must preserve the same global row budget.
func boundedReasoningDetail(detail string, limit int) string {
	if detail == "" || limit < 1 {
		return ""
	}
	lines := strings.Split(detail, "\n")
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	return strings.Join(lines, "\n")
}
