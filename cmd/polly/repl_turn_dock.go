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
	// activityRunning is the label naming the call still executing.
	activityRunning
	// activityAll is the whole activity line: one control that opens or
	// folds every disclosure the line carries.
	activityAll
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
	// latest marks the line's latest call, which takes the row under the
	// line rather than a place among its counts.
	latest bool
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
	return style.Styled(glyph, "accent", "bold") + " " + style.Styled(label, "muted", "")
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

// turnDockStatusFields is the status row of a settled turn that had no
// activity line to carry it: the outcome glyph with the final elapsed time.
// A running turn has no dock row: its clock lives in the status row at the
// lower left, and its tokens and cost in /context, so nothing is said twice.
func (m *replModel) turnDockStatusFields(dock turnDockState) []turnDockField {
	var fields []turnDockField
	if dock.settled && dock.elapsedKnown {
		fields = append(fields, turnOutcomeField(dock.outcome, formatElapsed(m.turnDockElapsedFor(dock))))
	} else if dock.settled && dock.outcome != turnOutcomeNone {
		fields = append(fields, turnOutcomeField(dock.outcome, ""))
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
	case turnOutcomePaused:
		raw = retryPausedLabel + tail
		rendered = style.Styled(retryPausedLabel, "active", "bold") + style.Styled(tail, "muted", "")
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

// renderActivityRow lays out one activity line: one accent triangle, down
// while any of the line's disclosures is open, then the fields joined by
// dots, and the right field (the settled turn's outcome and time) against
// the right edge. The line's latest call, when it has one, takes the row
// under the line by itself, in the first label's column. The triangle, the
// labels and the call row are hitboxes of kind activityAll, one per row, so
// wherever the line is clicked it opens or folds as a whole. A line wider
// than its room clips with an ellipsis and keeps the right field. Alongside
// the hitboxes it returns one label placement per field, the on-screen
// columns of the label text (zero or fewer when the label is clipped away),
// so painting never re-derives the line's geometry.
func renderActivityRow(fields []turnDockField, right turnDockField, width int) (string, []turnDockPlacement, []turnDockPlacement) {
	glyph := "▸"
	for _, field := range fields {
		if field.expanded {
			glyph = "▾"
		}
	}
	return renderActivityFields(fields, right, width, glyph)
}

// renderActivityFields shares the summary and latest-call layout between
// transcript expanders and agent previews. A blank glyph leaves the fold
// column empty; the preview's caller owns navigation instead of disclosures.
func renderActivityFields(fields []turnDockField, right turnDockField, width int, glyph string) (string, []turnDockPlacement, []turnDockPlacement) {
	if width <= 0 || len(fields) == 0 {
		return "", nil, nil
	}
	const separator = " · "
	type piece struct{ raw, rendered string }
	if glyph == "" {
		glyph = " "
	}
	pieces := []piece{{glyph, style.Styled(glyph, "accent", "bold")}, {" ", " "}}
	x := rw.StringWidth(glyph + " ")
	labels := make([]turnDockPlacement, len(fields))
	latest, inline := -1, 0
	for i, field := range fields {
		if field.latest {
			latest = i
			continue
		}
		if inline > 0 {
			pieces = append(pieces, piece{separator, style.Styled(separator, "muted", "")})
			x += rw.StringWidth(separator)
		}
		inline++
		rendered := field.rendered
		if rendered == "" {
			rendered = style.Styled(field.raw, "muted", "")
		}
		pieces = append(pieces, piece{field.raw, rendered})
		labels[i] = turnDockPlacement{kind: field.kind, X: x, Cols: rw.StringWidth(field.raw)}
		x += rw.StringWidth(field.raw)
	}
	// The right field keeps two columns clear of the labels; a line too
	// narrow to show any label beside it drops the field instead.
	rightCols := 0
	if right.raw != "" {
		rightCols = rw.StringWidth(right.raw) + 2
		if width-rightCols < rw.StringWidth(glyph+" ")+3 {
			right, rightCols = turnDockField{}, 0
		}
	}
	room := width - rightCols
	var rendered strings.Builder
	visible := x
	if x <= room {
		for _, p := range pieces {
			rendered.WriteString(p.rendered)
		}
	} else {
		cut := room - rw.StringWidth("…")
		// Label columns under or past the ellipsis are not on screen.
		for i := range labels {
			if i != latest {
				labels[i].Cols = min(labels[i].Cols, cut-labels[i].X)
			}
		}
		for _, p := range pieces {
			if cut <= 0 {
				break
			}
			if w := rw.StringWidth(p.raw); w <= cut {
				rendered.WriteString(p.rendered)
				cut -= w
				continue
			}
			rendered.WriteString(style.Styled(rw.Truncate(p.raw, cut, ""), "muted", ""))
			cut = 0
		}
		rendered.WriteString(style.Styled("…", "muted", ""))
		visible = room
	}
	if right.raw != "" {
		rendered.WriteString(strings.Repeat(" ", width-visible-rw.StringWidth(right.raw)))
		rendered.WriteString(right.rendered)
	}
	hits := []turnDockPlacement{{kind: activityAll, X: 0, Cols: visible}}
	if room := width - len(activityIndent); latest >= 0 && room > 0 {
		field := fields[latest]
		raw, styled := field.raw, field.rendered
		if styled == "" {
			styled = style.Styled(raw, "muted", "")
		}
		if rw.StringWidth(raw) > room {
			raw = rw.Truncate(raw, room, "…")
			styled = style.Styled(raw, "muted", "")
		}
		rendered.WriteString("\n" + activityIndent + styled)
		cols := rw.StringWidth(raw)
		labels[latest] = turnDockPlacement{kind: field.kind, X: len(activityIndent), Y: 1, Cols: cols}
		hits = append(hits, turnDockPlacement{kind: activityAll, X: 0, Y: 1, Cols: cols + len(activityIndent)})
	}
	return rendered.String(), hits, labels
}

// attachTurnDockTrailer hands the settled turn to its last activity line,
// whose right column then carries the outcome and elapsed time. A turn with
// no activity line leaves a status row in the transcript instead, and only
// with an elapsed time or an outcome other than success: a bare outcome
// glyph says nothing the reply did not. Tokens and cost live in /context.
func (m *replModel) attachTurnDockTrailer() {
	if !m.turnDock.visible || !m.turnDock.settled {
		m.clearTurnDock()
		return
	}
	dock := m.turnDock
	m.settledDock = dock
	if m.attachDockToActivity(dock) {
		m.clearTurnDock()
		return
	}
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

// attachDockToActivity gives the settled dock to the turn's last activity
// record, by transcript position, and reports whether there was one. The
// layout reads it back as the line's right field.
func (m *replModel) attachDockToActivity(dock turnDockState) bool {
	index := -1
	var reasoning *reasoningRecord
	var tools *toolDisclosureRecord
	for _, id := range dock.reasoningIDs {
		if record := m.reasoningRecords.get(id); record != nil && record.transcriptIndex > index {
			index, reasoning, tools = record.transcriptIndex, record, nil
		}
	}
	for _, id := range dock.toolIDs {
		if record := m.toolDisclosures.get(id); record != nil && record.transcriptIndex > index {
			index, reasoning, tools = record.transcriptIndex, nil, record
		}
	}
	if index < 0 {
		return false
	}
	if reasoning != nil {
		reasoning.trailer = &dock
	} else {
		tools.trailer = &dock
	}
	m.visual.invalidate()
	return true
}

// activityTrailerField is the right field of an activity line: the settled
// turn's elapsed time, with its outcome when that was not success. Empty
// for a line that is not a turn's last, or a settled turn with nothing to
// say.
func (m *replModel) activityTrailerField(reasoningIDs, toolIDs []int64) turnDockField {
	var dock *turnDockState
	for _, id := range reasoningIDs {
		if record := m.reasoningRecords.get(id); record != nil && record.trailer != nil {
			dock = record.trailer
		}
	}
	for _, id := range toolIDs {
		if record := m.toolDisclosures.get(id); record != nil && record.trailer != nil {
			dock = record.trailer
		}
	}
	if dock == nil {
		return turnDockField{}
	}
	elapsed := ""
	if dock.elapsedKnown {
		elapsed = formatElapsed(m.turnDockElapsedFor(*dock))
	}
	if dock.outcome == turnOutcomeDone {
		return turnDockField{raw: elapsed, rendered: style.Styled(elapsed, "muted", "")}
	}
	return turnOutcomeField(dock.outcome, elapsed)
}

func turnCacheField(cache turnCacheUsage) (turnDockField, bool) {
	if !cache.reported || cache.missing || cache.input <= 0 || cache.read < 0 || cache.read > cache.input {
		return turnDockField{}, false
	}
	raw := fmt.Sprintf("%.0f%% cache hit", 100*float64(cache.read)/float64(cache.input))
	return turnDockField{raw: raw, rendered: style.Styled(raw, "muted", ""), optional: true}, true
}

// lastTurnSummary is the latest turn as /context reports it: the elapsed
// time, the tokens in and out, the cache rate and the cost, as far as they
// are known. A running turn that has reported tokens is the latest; else
// the last settled one. Empty until a turn has reported tokens.
func (m *replModel) lastTurnSummary() string {
	dock := m.settledDock
	if m.turnDock.visible && (m.turnDock.settled || m.turnDock.inputTokens > 0 || m.turnDock.outputTokens > 0) {
		dock = m.turnDock
	}
	if dock.inputTokens <= 0 && dock.outputTokens <= 0 {
		return ""
	}
	parts := []string{}
	if dock.settled && dock.elapsedKnown && dock.elapsed > 0 {
		parts = append(parts, formatElapsed(dock.elapsed))
	}
	if field, ok := turnTokenField(dock.inputTokens, dock.outputTokens, dock.estimated); ok {
		parts = append(parts, field.raw)
	}
	if field, ok := turnCacheField(dock.cache); ok {
		parts = append(parts, field.raw)
	}
	if field, ok := turnCostField(dock.cost); ok {
		parts = append(parts, field.raw)
	}
	return strings.Join(parts, " · ")
}
