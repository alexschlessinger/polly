package main

import (
	"fmt"
	"strings"
	"time"

	rw "github.com/mattn/go-runewidth"
	ui "github.com/metaspartan/gotui/v5"
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
	reasoningID      int64
	toolDisclosureID int64
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

func (m *replModel) startTurnDock() {
	if record := m.turnTrailers[m.openTurnTrailerID]; record != nil {
		record.dock.overlay = turnDockOverlayNone
		m.refreshTurnTrailer(record)
	}
	m.openTurnTrailerID = 0
	m.turnDock = turnDockState{visible: !m.quiet, elapsedKnown: true}
	m.turnDockPlacements = nil
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
	m.turnDockPlacements = nil
}

func (m *replModel) turnDockElapsed() time.Duration {
	return m.turnDockElapsedFor(m.turnDock)
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

func (m *replModel) turnDockThought() (*reasoningRecord, bool) {
	return m.turnDockThoughtFor(m.turnDock)
}

func (m *replModel) turnDockThoughtFor(dock turnDockState) (*reasoningRecord, bool) {
	record := m.reasoningRecords[dock.reasoningID]
	return record, record != nil && len(record.tail) > 0
}

func (m *replModel) turnDockTools() (*toolDisclosureRecord, bool) {
	return m.turnDockToolsFor(m.turnDock)
}

func (m *replModel) turnDockToolsFor(dock turnDockState) (*toolDisclosureRecord, bool) {
	record := m.toolDisclosures[dock.toolDisclosureID]
	return record, record != nil && len(record.rows) > 0
}

func (m *replModel) turnDockFields() []turnDockField {
	return m.turnDockFieldsFor(m.turnDock)
}

func (m *replModel) turnDockFieldsFor(dock turnDockState) []turnDockField {
	var fields []turnDockField
	if record, ok := m.turnDockThoughtFor(dock); ok {
		elapsed := record.elapsed
		if record.active && !m.thinkingSegmentStart.IsZero() {
			elapsed += time.Since(m.thinkingSegmentStart)
		}
		label := "Thought"
		if elapsed > 0 {
			label += " " + formatElapsed(elapsed)
		}
		glyph := "▸"
		if dock.overlay == turnDockOverlayThought {
			glyph = "▾"
		}
		raw := glyph + " " + label
		rendered := styled(glyph, "accent", "bold") + " " + styled(label, "accent", "bold")
		fields = append(fields, turnDockField{raw: raw, rendered: rendered, overlay: turnDockOverlayThought})
	}
	if record, ok := m.turnDockToolsFor(dock); ok {
		label := fmt.Sprintf("%d tools", len(record.rows))
		if len(record.rows) == 1 {
			label = "1 tool"
		}
		glyph := "▸"
		if dock.overlay == turnDockOverlayTools {
			glyph = "▾"
		}
		raw := glyph + " " + label
		rendered := styled(glyph, "accent", "bold") + " " + styled(label, "accent", "bold")
		fields = append(fields, turnDockField{raw: raw, rendered: rendered, overlay: turnDockOverlayTools})
	}

	if !dock.settled || dock.elapsedKnown {
		elapsed := formatElapsed(m.turnDockElapsedFor(dock))
		raw, rendered := elapsed, styled(elapsed, "muted", "")
		if dock.settled {
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
		}
		fields = append(fields, turnDockField{raw: raw, rendered: rendered})
	} else if dock.outcome == turnOutcomeDone {
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
	}
	if tools != nil {
		m.turnDock.toolDisclosureID = tools.id
	}
	m.turnDockPlacements = nil
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
	const indent = "  "
	const separator = " · "
	fields := m.turnDockFieldsFor(dock)
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
		return styled(rw.Truncate(raw.String(), width, "…"), "muted", ""), nil
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
		record, ok := m.turnDockThoughtFor(dock)
		if !ok {
			return ""
		}
		contentWidth := width - rw.StringWidth(reasoningBlockIndent)
		if contentWidth < 2 {
			return ""
		}
		lines := reasoningTailLines(string(record.tail), contentWidth, reasoningPreviewLines)
		for i := range lines {
			lines[i] = reasoningBlockIndent + styled(lines[i], "muted", "italic")
		}
		return strings.Join(lines, "\n")
	case turnDockOverlayTools:
		record, ok := m.turnDockToolsFor(dock)
		if !ok {
			return ""
		}
		start := 0
		if len(record.rows) > turnDockToolOverlayRows {
			start = len(record.rows) - (turnDockToolOverlayRows - 1)
		}
		var lines []string
		if start > 0 {
			lines = append(lines, "  "+styled(fmt.Sprintf("… %d earlier", start), "muted", ""))
		}
		for _, row := range record.rows[start:] {
			if row.line != "" {
				lines = append(lines, stripTranscriptImageMarkers(row.line))
			}
		}
		return strings.Join(lines, "\n")
	}
	return ""
}

func (m *replModel) refreshExpandedTurnTrailer(width int) {
	if width > 0 {
		m.reasoningWidth = width
	}
	if record := m.turnTrailers[m.openTurnTrailerID]; record != nil {
		m.refreshTurnTrailer(record)
	}
}

func (m *replModel) toggleTurnDockAt(x, y int) bool {
	for _, placement := range m.turnDockPlacements {
		if y != placement.Y || x < placement.X || x >= placement.X+placement.Cols {
			continue
		}
		return m.toggleTurnDockOverlay(placement.overlay)
	}
	return false
}

func (m *replModel) toggleTurnDockOverlay(overlay turnDockOverlay) bool {
	if open := m.turnTrailers[m.openTurnTrailerID]; open != nil {
		open.dock.overlay = turnDockOverlayNone
		m.refreshTurnTrailer(open)
		m.openTurnTrailerID = 0
	}
	switch overlay {
	case turnDockOverlayThought:
		if _, ok := m.turnDockThought(); !ok {
			return false
		}
	case turnDockOverlayTools:
		if _, ok := m.turnDockTools(); !ok {
			return false
		}
	default:
		return false
	}
	if m.turnDock.overlay == overlay {
		m.turnDock.overlay = turnDockOverlayNone
	} else {
		m.turnDock.overlay = overlay
	}
	if overlay == turnDockOverlayThought && m.currentReasoningRecord() != nil {
		m.turnReasoningOpen = m.turnDock.overlay == turnDockOverlayThought
	}
	return true
}

func (m *replModel) closeTurnDockOverlay() bool {
	if m.turnDock.overlay != turnDockOverlayNone {
		m.turnDock.overlay = turnDockOverlayNone
		return true
	}
	if record := m.turnTrailers[m.openTurnTrailerID]; record != nil && record.dock.overlay != turnDockOverlayNone {
		record.dock.overlay = turnDockOverlayNone
		m.refreshTurnTrailer(record)
		m.openTurnTrailerID = 0
		return true
	}
	return false
}

func (m *replModel) turnDockOverlayRows(width int) [][]ui.Cell {
	if width < 2 || !m.turnDock.visible {
		return nil
	}
	dock := m.turnDock
	switch dock.overlay {
	case turnDockOverlayThought:
		record, ok := m.turnDockThoughtFor(dock)
		if !ok {
			return nil
		}
		contentWidth := width - rw.StringWidth(reasoningBlockIndent)
		if contentWidth < 2 {
			return nil
		}
		lines := reasoningTailLines(string(record.tail), contentWidth, reasoningPreviewLines)
		rows := make([][]ui.Cell, 0, len(lines))
		for _, line := range lines {
			text := reasoningBlockIndent + styled(line, "muted", "italic")
			rows = append(rows, ui.ParseStyles(text, ui.NewStyle(ui.ColorClear)))
		}
		return rows
	case turnDockOverlayTools:
		record, ok := m.turnDockToolsFor(dock)
		if !ok {
			return nil
		}
		start := 0
		if len(record.rows) > turnDockToolOverlayRows {
			start = len(record.rows) - (turnDockToolOverlayRows - 1)
		}
		var lines []string
		if start > 0 {
			lines = append(lines, "  "+styled(fmt.Sprintf("… %d earlier", start), "muted", ""))
		}
		for _, row := range record.rows[start:] {
			if row.line != "" {
				lines = append(lines, stripTranscriptImageMarkers(row.line))
			}
		}
		rows := transcriptVisualRows(strings.Join(lines, "\n"), ui.NewStyle(ui.ColorClear), width)
		if len(rows) > turnDockToolOverlayRows {
			rows = rows[len(rows)-turnDockToolOverlayRows:]
		}
		return rows
	}
	return nil
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
				if _, ok := m.turnDockThoughtFor(record.dock); !ok {
					continue
				}
			case turnDockOverlayTools:
				if _, ok := m.turnDockToolsFor(record.dock); !ok {
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
