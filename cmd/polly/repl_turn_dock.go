package main

import (
	"fmt"
	"strings"
	"time"

	rw "github.com/mattn/go-runewidth"
)

type turnDockOverlay int

const (
	turnDockOverlayNone turnDockOverlay = iota
	turnDockOverlayThought
	turnDockOverlayTools
)

const turnDockToolOverlayRows = 6

type turnDockState struct {
	visible          bool
	settled          bool
	outcome          turnOutcome
	elapsed          time.Duration
	elapsedKnown     bool
	inputTokens      int
	outputTokens     int
	reasoningID      int64   // first reasoning record of the turn (label/legacy)
	toolDisclosureID int64   // first tool disclosure of the turn (label/legacy)
	reasoningIDs     []int64 // every reasoning record opened during the turn
	toolIDs          []int64 // every tool disclosure opened during the turn
	overlay          turnDockOverlay
}

type turnDockPlacement struct {
	overlay turnDockOverlay
	X, Y    int
	Cols    int
}

type turnTrailerRecord struct {
	id              int64
	transcriptIndex int
	dock            turnDockState
	fields          []turnDockPlacement
}

type turnTrailerPlacement struct {
	recordID int64
	turnDockPlacement
}

type turnDockField struct {
	raw      string
	rendered string
	overlay  turnDockOverlay
	optional bool
}

// turnActivityControl is the shared visual language for clickable reasoning
// and tool controls, whether they appear inline or in a settled trailer.
func turnActivityControl(glyph, label string) string {
	return styled(glyph, "accent", "bold") + " " + styled(label, "accent", "bold")
}

func inlineActivityControl(glyph, label string, settled bool) string {
	if settled {
		return styled(glyph, "muted", "") + " " + styled(label, "muted", "")
	}
	return turnActivityControl(glyph, label)
}

func turnToolLabel(total int) string {
	if total == 1 {
		return "1 tool"
	}
	return fmt.Sprintf("%d tools", total)
}

func (m *replModel) startTurnDock() {
	if record := m.turnTrailers[m.openTurnTrailerID]; record != nil {
		record.dock.overlay = turnDockOverlayNone
		m.refreshTurnTrailer(record)
	}
	m.openTurnTrailerID = 0
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

// turnDockThoughtRecords returns the turn's reasoning records in order. The
// singular reasoningID seeds the list for docks settled before plural IDs
// existed. Caller must hold m.mu.
func (m *replModel) turnDockThoughtRecords(dock turnDockState) []*reasoningRecord {
	ids := dock.reasoningIDs
	if len(ids) == 0 && dock.reasoningID != 0 {
		ids = []int64{dock.reasoningID}
	}
	var records []*reasoningRecord
	for _, id := range ids {
		if record := m.reasoningRecords[id]; record != nil && len(record.tail) > 0 {
			records = append(records, record)
		}
	}
	return records
}

// turnDockToolRecords returns the turn's tool disclosures in order. Caller
// must hold m.mu.
func (m *replModel) turnDockToolRecords(dock turnDockState) []*toolDisclosureRecord {
	ids := dock.toolIDs
	if len(ids) == 0 && dock.toolDisclosureID != 0 {
		ids = []int64{dock.toolDisclosureID}
	}
	var records []*toolDisclosureRecord
	for _, id := range ids {
		if record := m.toolDisclosures[id]; record != nil && len(record.rows) > 0 {
			records = append(records, record)
		}
	}
	return records
}

func (m *replModel) turnDockToolRowCount(dock turnDockState) int {
	total := 0
	for _, record := range m.turnDockToolRecords(dock) {
		total += len(record.rows)
	}
	return total
}

func (m *replModel) turnDockFieldsFor(dock turnDockState) []turnDockField {
	var fields []turnDockField
	// The live dock is status-only: reasoning and tool activity render inline
	// in the transcript as they occur. Settled trailers keep the activity
	// fields because their overlays are the only way to revisit them.
	if !dock.settled {
		return m.turnDockStatusFields(dock)
	}
	if records := m.turnDockThoughtRecords(dock); len(records) > 0 {
		var elapsed time.Duration
		for _, record := range records {
			elapsed += record.elapsed
			if record.active && !m.thinkingSegmentStart.IsZero() && record.id == m.turnReasoningID {
				elapsed += time.Since(m.thinkingSegmentStart)
			}
		}
		label := reasoningDisclosureLabel(false, false, elapsed)
		glyph := "▸"
		if dock.overlay == turnDockOverlayThought {
			glyph = "▾"
		}
		raw := glyph + " " + label
		rendered := turnActivityControl(glyph, label)
		fields = append(fields, turnDockField{raw: raw, rendered: rendered, overlay: turnDockOverlayThought})
	}
	if total := m.turnDockToolRowCount(dock); total > 0 {
		label := turnToolLabel(total)
		glyph := "▸"
		if dock.overlay == turnDockOverlayTools {
			glyph = "▾"
		}
		raw := glyph + " " + label
		rendered := turnActivityControl(glyph, label)
		fields = append(fields, turnDockField{raw: raw, rendered: rendered, overlay: turnDockOverlayTools})
	}

	return append(fields, m.turnDockStatusFields(dock)...)
}

// turnDockStatusFields renders the status tail shared by the live dock and
// settled trailers. The live dock carries only token counts: the running
// elapsed time lives solely in the status row at the lower left, so the two
// never duplicate. Settled trailers keep their final elapsed time (with the
// outcome glyph) because the status row has moved on by then.
func (m *replModel) turnDockStatusFields(dock turnDockState) []turnDockField {
	var fields []turnDockField
	if dock.settled && dock.elapsedKnown {
		elapsed := formatElapsed(m.turnDockElapsedFor(dock))
		raw, rendered := elapsed, styled(elapsed, "muted", "")
		switch dock.outcome {
		case turnOutcomeDone:
			raw = "✓ " + elapsed
			rendered = styled("✓", "ok", "bold") + " " + styled(elapsed, "muted", "")
		case turnOutcomeFailed:
			raw = "✗ failed · " + elapsed
			rendered = styled("✗ failed", "err", "bold") + styled(" · "+elapsed, "muted", "")
		case turnOutcomeCanceled:
			raw = "canceled · " + elapsed
			rendered = styled("canceled", "muted", "bold") + styled(" · "+elapsed, "muted", "")
		}
		fields = append(fields, turnDockField{raw: raw, rendered: rendered})
	} else if dock.settled && dock.outcome == turnOutcomeDone {
		fields = append(fields, turnDockField{raw: "✓", rendered: styled("✓", "ok", "bold")})
	}

	in, out := dock.inputTokens, dock.outputTokens
	if !dock.settled {
		in, out = m.lastIn, m.lastOut
	}
	if in > 0 || out > 0 {
		tokenRaw := fmt.Sprintf("%s in / %s out", humanizeTokens(in), humanizeTokens(out))
		fields = append(fields, turnDockField{
			raw: tokenRaw, rendered: styled(tokenRaw, "muted", ""), optional: true,
		})
	}
	return fields
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
		m.turnDock.reasoningID = reasoning.id
		m.turnDock.reasoningIDs = []int64{reasoning.id}
	}
	if tools != nil {
		m.turnDock.toolDisclosureID = tools.id
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
	return renderTurnActivityRow(m.turnDockFieldsFor(dock), width)
}

// renderTurnActivityRow is the one-line renderer shared by inline activity
// and the settled trailer. Fields that do not fit are truncated as one row;
// clickable placements are returned only for controls wholly on that row.
func renderTurnActivityRow(fields []turnDockField, width int) (string, []turnDockPlacement) {
	if width <= 0 {
		return "", nil
	}
	const indent = "  "
	const separator = " · "
	measure := func(fields []turnDockField) int {
		parts := make([]string, len(fields))
		for i := range fields {
			parts[i] = fields[i].raw
		}
		return rw.StringWidth(indent) + rw.StringWidth(strings.Join(parts, separator))
	}
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
		if field.overlay != turnDockOverlayNone && start+rw.StringWidth(field.raw) <= width {
			placements = append(placements, turnDockPlacement{
				overlay: field.overlay,
				X:       start,
				Cols:    rw.StringWidth(field.raw),
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

func (m *replModel) attachTurnDockTrailer() {
	if !m.turnDock.visible || !m.turnDock.settled {
		m.clearTurnDock()
		return
	}
	dock := m.turnDock
	dock.overlay = turnDockOverlayNone
	width := m.reasoningWidth
	if width < 2 {
		width = 80
	}
	text, fields := m.turnDockRowFor(dock, width)
	if text == "" {
		m.clearTurnDock()
		return
	}
	m.turnTrailerSeq++
	record := &turnTrailerRecord{id: m.turnTrailerSeq, dock: dock, fields: fields}
	m.appendLine(text)
	record.transcriptIndex = len(m.transcript) - 1
	m.turnTrailers[record.id] = record
	m.turnTrailerAt[record.transcriptIndex] = record.id
	m.clearTurnDock()
}

func (m *replModel) refreshTurnTrailer(record *turnTrailerRecord) {
	if record == nil || record.transcriptIndex < 0 || record.transcriptIndex >= len(m.transcript) {
		return
	}
	width := m.reasoningWidth
	if width < 2 {
		width = 80
	}
	text, fields := m.turnDockRowFor(record.dock, width)
	if detail := m.turnTrailerDetailText(record.dock, width); detail != "" {
		text += "\n" + detail
	}
	record.fields = fields
	if m.transcript[record.transcriptIndex] == text {
		return
	}
	oldCount, start := 0, 0
	if !m.followBottom {
		oldCount = m.entryVisualLineCount(record.transcriptIndex, width)
		start = m.entryVisualStart(record.transcriptIndex, width)
	}
	m.transcript[record.transcriptIndex] = text
	m.invalidateFlat()
	if !m.followBottom {
		m.anchorForResizedEntry(start, oldCount, m.entryVisualLineCount(record.transcriptIndex, width))
	}
}

func (m *replModel) turnTrailerDetailText(dock turnDockState, width int) string {
	switch dock.overlay {
	case turnDockOverlayThought:
		records := m.turnDockThoughtRecords(dock)
		if len(records) == 0 {
			return ""
		}
		contentWidth := width - rw.StringWidth(reasoningBlockIndent)
		if contentWidth < 2 {
			return ""
		}
		var tails []string
		for _, record := range records {
			tails = append(tails, string(record.tail))
		}
		lines := reasoningTailLines(strings.Join(tails, "\n"), contentWidth, reasoningPreviewLines)
		for i := range lines {
			lines[i] = reasoningBlockIndent + styled(lines[i], "muted", "italic")
		}
		return strings.Join(lines, "\n")
	case turnDockOverlayTools:
		records := m.turnDockToolRecords(dock)
		if len(records) == 0 {
			return ""
		}
		var all []string
		for _, record := range records {
			for _, row := range record.rows {
				if row.line != "" {
					all = append(all, stripTranscriptImageMarkers(row.line))
				}
			}
		}
		start := 0
		if len(all) > turnDockToolOverlayRows {
			start = len(all) - (turnDockToolOverlayRows - 1)
		}
		var lines []string
		if start > 0 {
			lines = append(lines, "  "+styled(fmt.Sprintf("… %d earlier", start), "muted", ""))
		}
		lines = append(lines, all[start:]...)
		return strings.Join(lines, "\n")
	}
	return ""
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

func (m *replModel) refreshExpandedTurnTrailer(width int) {
	if width > 0 {
		m.reasoningWidth = width
	}
	if record := m.turnTrailers[m.openTurnTrailerID]; record != nil {
		m.refreshTurnTrailer(record)
	}
}

// closeTurnDockOverlay dismisses an expanded settled-trailer overlay. The
// live dock is status-only and has no overlay to close.
func (m *replModel) closeTurnDockOverlay() bool {
	if record := m.turnTrailers[m.openTurnTrailerID]; record != nil && record.dock.overlay != turnDockOverlayNone {
		record.dock.overlay = turnDockOverlayNone
		m.refreshTurnTrailer(record)
		m.openTurnTrailerID = 0
		return true
	}
	return false
}

func (m *replModel) toggleTurnTrailerAt(x, y int) bool {
	for _, placement := range m.turnTrailerPlacements {
		if y != placement.Y || x < placement.X || x >= placement.X+placement.Cols {
			continue
		}
		record := m.turnTrailers[placement.recordID]
		if record == nil {
			return false
		}
		return m.toggleTurnTrailerOverlay(record, placement.overlay)
	}
	return false
}

func (m *replModel) toggleLatestTurnTrailerOverlay(overlay turnDockOverlay) bool {
	for id := m.turnTrailerSeq; id > 0; id-- {
		if record := m.turnTrailers[id]; record != nil {
			switch overlay {
			case turnDockOverlayThought:
				if len(m.turnDockThoughtRecords(record.dock)) == 0 {
					continue
				}
			case turnDockOverlayTools:
				if len(m.turnDockToolRecords(record.dock)) == 0 {
					continue
				}
			}
			return m.toggleTurnTrailerOverlay(record, overlay)
		}
	}
	return false
}

func (m *replModel) toggleTurnTrailerOverlay(record *turnTrailerRecord, overlay turnDockOverlay) bool {
	if record == nil || (overlay != turnDockOverlayThought && overlay != turnDockOverlayTools) {
		return false
	}
	if open := m.turnTrailers[m.openTurnTrailerID]; open != nil && open.id != record.id {
		open.dock.overlay = turnDockOverlayNone
		m.refreshTurnTrailer(open)
	}
	if m.turnDock.overlay == turnDockOverlayThought {
		m.turnReasoningOpen = false
	}
	m.turnDock.overlay = turnDockOverlayNone
	if record.dock.overlay == overlay {
		record.dock.overlay = turnDockOverlayNone
		m.openTurnTrailerID = 0
	} else {
		record.dock.overlay = overlay
		m.openTurnTrailerID = record.id
	}
	m.refreshTurnTrailer(record)
	return true
}
