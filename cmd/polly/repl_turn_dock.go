package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
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
	activityKindCount
)

type turnDockState struct {
	visible      bool
	settled      bool
	outcome      turnOutcome
	elapsed      time.Duration
	elapsedKnown bool
	inputTokens  int
	outputTokens int
	estimated    bool
	cost         turnCost
	cache        turnCacheUsage
	reasoningIDs []int64 // every reasoning record opened during the turn
	toolIDs      []int64 // every tool disclosure opened during the turn
}

type turnDockPlacement struct {
	kind activityKind
	X, Y int
	Cols int
}

// turnTrailerRecord is the settled status row a turn leaves in the
// transcript: outcome, elapsed time, tokens, and cost. The turn's activity stays
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
	return turnDockField{raw: label, rendered: style.Styled(label, "muted", ""), kind: kind, expanded: expanded}
}

// activityRowHeader is the collapsed activity row for one disclosure: the
// accent triangle, then the muted label. Rows with several disclosures repeat
// the shape per control and join them with dots; see renderActivityRow.
func activityRowHeader(glyph, label string) string {
	return "  " + style.Styled(glyph, "accent", "bold") + " " + style.Styled(label, "muted", "")
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
	s := turnActivitySummary{Tools: m.toolRowCount(dock.toolIDs), Agents: m.agentCounts(dock.toolIDs), Outcome: dock.outcome, Elapsed: m.turnDockElapsedFor(dock), In: dock.inputTokens, Out: dock.outputTokens, Cost: dock.cost}
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
	m.turnDock.estimated = m.lastEstimated
	m.turnDock.cost = m.lastCost
	if m.completion != nil {
		m.turnDock.cache = m.completion.Cache
	}
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
// settled trailers. The live dock carries only token counts and cost: the running
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

	in, out, estimated, cost := dock.inputTokens, dock.outputTokens, dock.estimated, dock.cost
	if !dock.settled {
		in, out, estimated, cost = m.lastIn, m.lastOut, m.lastEstimated, m.lastCost
	}
	if field, ok := turnTokenField(in, out, estimated); ok {
		fields = append(fields, field)
	}
	if dock.settled {
		if field, ok := turnCacheField(dock.cache); ok {
			fields = append(fields, field)
		}
	}
	if field, ok := turnCostField(cost); ok {
		fields = append(fields, field)
	}
	return fields
}

// turnOutcomeField is the settled outcome glyph with the turn's elapsed time,
// shared by the TUI trailer and the one-shot summary.
func turnOutcomeField(outcome turnOutcome, elapsed string) turnDockField {
	raw, rendered := elapsed, style.Styled(elapsed, "muted", "")
	tail := ""
	if elapsed != "" {
		tail = " · " + elapsed
	}
	switch outcome {
	case turnOutcomeDone:
		raw = strings.TrimSpace("✓ " + elapsed)
		rendered = style.Styled("✓", "ok", "bold")
		if elapsed != "" {
			rendered += " " + style.Styled(elapsed, "muted", "")
		}
	case turnOutcomeFailed:
		raw = "✗ failed" + tail
		rendered = style.Styled("✗ failed", "err", "bold") + style.Styled(tail, "muted", "")
	case turnOutcomeCanceled:
		raw = "canceled" + tail
		rendered = style.Styled("canceled", "muted", "bold") + style.Styled(tail, "muted", "")
	case turnOutcomeIncomplete:
		raw = "incomplete" + tail
		rendered = style.Styled("incomplete", "active", "bold") + style.Styled(tail, "muted", "")
	}
	return turnDockField{raw: raw, rendered: rendered, protected: true, elapsed: elapsed, outcome: outcome}
}

// turnTokenField is the optional token-count tail of a status row. A "~"
// marks counts that include an estimate.
func turnTokenField(in, out int, estimated bool) (turnDockField, bool) {
	if in <= 0 && out <= 0 {
		return turnDockField{}, false
	}
	raw := fmt.Sprintf("%s%s in / %s out", estimateMark(estimated), humanizeTokens(in), humanizeTokens(out))
	return turnDockField{raw: raw, rendered: style.Styled(raw, "muted", ""), optional: true}, true
}

// turnCostField is the optional cost at the end of a status row, the first
// field a narrow row drops. A "~" marks a cost estimated from advertised
// rates rather than billed by the provider.
func turnCostField(cost turnCost) (turnDockField, bool) {
	if !cost.known {
		return turnDockField{}, false
	}
	raw := estimateMark(cost.estimated) + formatCostUSD(cost.usd)
	return turnDockField{raw: raw, rendered: style.Styled(raw, "muted", ""), optional: true}, true
}

func estimateMark(estimated bool) string {
	if estimated {
		return "~"
	}
	return ""
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
	for turnDockFieldsWidth(fields) > width {
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
		for turnDockFieldsWidth(fields) > width && len(fields) > 1 {
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
			excess := turnDockFieldsWidth(fields) - width
			f := fields[pick]
			available := rw.StringWidth(f.raw) - excess
			if available >= 3 {
				f.raw = rw.Truncate(f.raw, available, "…")
				f.rendered = style.Styled(f.raw, "accent", "")
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
			rendered.WriteString(style.Styled(separator, "muted", ""))
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
		return style.Styled(rw.Truncate(raw.String(), width, "…"), "muted", ""), clippedPlacements(placements, width)
	}
	return rendered.String(), placements
}

// clippedPlacements keeps the controls that stay wholly visible on a row
// clipped to width, whose final cell the ellipsis occupies. Keeping those
// placements prevents the inline fallback from turning the entire truncated
// row into overlapping reasoning and tool targets.
func clippedPlacements(placements []turnDockPlacement, width int) []turnDockPlacement {
	visibleWidth := width - rw.StringWidth("…")
	kept := placements[:0]
	for _, placement := range placements {
		if placement.X+placement.Cols <= visibleWidth {
			kept = append(kept, placement)
		}
	}
	return kept
}

// renderActivityRow lays out one inline activity row: every disclosure as an
// accent triangle (down when that disclosure is expanded) and its muted
// label, the controls joined by dots. Each triangle-and-label pair is one
// hitbox, so controls sharing a row read and click alike. A row wider than
// the terminal clips with an ellipsis and keeps only the hitboxes that remain
// wholly visible. Alongside the hitboxes it returns one label placement per
// field, the on-screen columns of the label text (zero or fewer when the
// label is clipped away), so painting never re-derives the row's geometry.
func renderActivityRow(fields []turnDockField, width int) (string, []turnDockPlacement, []turnDockPlacement) {
	if width <= 0 || len(fields) == 0 {
		return "", nil, nil
	}
	const indent = "  "
	const separator = " · "
	type piece struct{ text, class, mod string }
	pieces := []piece{{text: indent}}
	var placements []turnDockPlacement
	labels := make([]turnDockPlacement, 0, len(fields))
	x := rw.StringWidth(indent)
	for i, field := range fields {
		if i > 0 {
			pieces = append(pieces, piece{separator, "muted", ""})
			x += rw.StringWidth(separator)
		}
		glyph := "▸"
		if field.expanded {
			glyph = "▾"
		}
		pieces = append(pieces, piece{glyph, "accent", "bold"}, piece{text: " "}, piece{field.raw, "muted", ""})
		labelX, labelCols := x+rw.StringWidth(glyph+" "), rw.StringWidth(field.raw)
		labels = append(labels, turnDockPlacement{kind: field.kind, X: labelX, Cols: labelCols})
		cols := labelX - x + labelCols
		if field.kind != activityNone && x+cols <= width {
			placements = append(placements, turnDockPlacement{kind: field.kind, X: x, Cols: cols})
		}
		x += cols
	}
	var rendered strings.Builder
	if x <= width {
		for _, p := range pieces {
			rendered.WriteString(style.Styled(p.text, p.class, p.mod))
		}
		return rendered.String(), placements, labels
	}
	room := width - rw.StringWidth("…")
	// Label columns under or past the ellipsis are not on screen.
	for i := range labels {
		labels[i].Cols = min(labels[i].Cols, room-labels[i].X)
	}
	for _, p := range pieces {
		if room <= 0 {
			break
		}
		if w := rw.StringWidth(p.text); w <= room {
			rendered.WriteString(style.Styled(p.text, p.class, p.mod))
			room -= w
			continue
		}
		rendered.WriteString(style.Styled(rw.Truncate(p.text, room, ""), p.class, p.mod))
		room = 0
	}
	rendered.WriteString(style.Styled("…", "muted", ""))
	return rendered.String(), clippedPlacements(placements, width), labels
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

func turnCacheField(cache turnCacheUsage) (turnDockField, bool) {
	if !cache.reported || cache.missing || cache.input <= 0 || cache.read < 0 || cache.read > cache.input {
		return turnDockField{}, false
	}
	raw := fmt.Sprintf("%.0f%% cache hit", 100*float64(cache.read)/float64(cache.input))
	return turnDockField{raw: raw, rendered: style.Styled(raw, "muted", ""), optional: true}, true
}
