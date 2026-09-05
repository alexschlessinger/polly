package main

import (
	"fmt"
	"slices"
	"strings"
	"time"

	ui "github.com/metaspartan/gotui/v5"
)

// The transcript: entry owners, the assistant stream, row layout, and scrolling.

// setTranscriptText replaces entry index's text. Every in-place transcript
// write goes through here or setTranscriptEntry, so the visual cache is
// always marked stale by the write itself rather than by each caller.
func (m *replModel) setTranscriptText(index int, text string) {
	m.transcript[index].text = text
	m.visual.invalidate()
}

// setTranscriptEntry replaces entry index's text and image list together.
func (m *replModel) setTranscriptEntry(index int, text string, images []transcriptImage) {
	m.setTranscriptText(index, text)
	m.setTranscriptImages(index, images)
}

// setTranscriptImages replaces entry index's image list.
func (m *replModel) setTranscriptImages(index int, images []transcriptImage) {
	if len(images) == 0 {
		m.transcript[index].images = nil
	} else {
		m.transcript[index].images = append([]transcriptImage(nil), images...)
	}
	m.visual.invalidate()
}

func (m *replModel) deleteTranscriptEntry(index int) {
	if id, ok := m.turnTrailerAt[index]; ok {
		delete(m.turnTrailerAt, index)
		delete(m.turnTrailers, id)
		if m.openTurnTrailerID == id {
			m.openTurnTrailerID = 0
		}
	}
	if id, ok := m.reasoningAt[index]; ok {
		delete(m.reasoningAt, index)
		delete(m.reasoningRecords, id)
		for i, orderedID := range m.reasoningOrder {
			if orderedID == id {
				m.reasoningOrder = append(m.reasoningOrder[:i], m.reasoningOrder[i+1:]...)
				break
			}
		}
		if m.turnReasoningID == id {
			m.resetCurrentThinking()
		}
	}
	if id, ok := m.toolDisclosureAt[index]; ok {
		delete(m.toolDisclosureAt, index)
		delete(m.toolDisclosures, id)
		if m.turnToolDisclosureID == id {
			m.turnToolDisclosureID = 0
		}
	}
	m.transcript = slices.Delete(m.transcript, index, index+1)
	for i := index + 1; i <= len(m.transcript); i++ {
		if id, ok := m.reasoningAt[i]; ok {
			m.reasoningAt[i-1] = id
			delete(m.reasoningAt, i)
			if record := m.reasoningRecords[id]; record != nil {
				record.transcriptIndex = i - 1
			}
		}
		if id, ok := m.toolDisclosureAt[i]; ok {
			m.toolDisclosureAt[i-1] = id
			delete(m.toolDisclosureAt, i)
			if record := m.toolDisclosures[id]; record != nil {
				record.transcriptIndex = i - 1
			}
		}
		if id, ok := m.turnTrailerAt[i]; ok {
			m.turnTrailerAt[i-1] = id
			delete(m.turnTrailerAt, i)
			if record := m.turnTrailers[id]; record != nil {
				record.transcriptIndex = i - 1
			}
		}
	}
	for i := range m.queue {
		if !m.queue[i].transcriptShown {
			continue
		}
		switch {
		case m.queue[i].transcriptIndex == index:
			m.queue[i].transcriptShown = false
		case m.queue[i].transcriptIndex > index:
			m.queue[i].transcriptIndex--
		}
	}
	m.visual.invalidate()
}

// appendLine appends a pre-rendered transcript entry (may contain inline
// style markup). A non-assistant boundary first settles any active assistant
// block so pending fence text and provider terminal newlines cannot leak across
// notices, warnings, tools, or user turns.
func (m *replModel) appendLine(s string) {
	if m.currentAssistant >= 0 && m.currentAssistant < len(m.transcript) {
		m.finishAssistantBlock("")
	}
	m.appendTranscriptEntry(s)
	m.currentAssistant = -1
}

// appendTranscriptEntry grows the transcript and returns the new entry's
// index; every transcript append goes through here.
func (m *replModel) appendTranscriptEntry(text string) int {
	m.transcript = append(m.transcript, transcriptEntry{text: text})
	m.visual.invalidate()
	return len(m.transcript) - 1
}

func (m *replModel) resetAssistantStream() {
	m.streamRaw.Reset()
	m.streamShown = 0
	m.streamCodeCache = nil
}

// appendAssistant accumulates streamed model output into the current
// assistant entry. Provider chunk frequency never determines paint frequency;
// the event loop renders the accumulated prefix once per visible frame.
func (m *replModel) appendAssistant(text string) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	if text == "" {
		return
	}
	if m.currentAssistant < 0 || m.currentAssistant >= len(m.transcript) {
		// Start a fresh assistant entry (currentAssistant is reset to -1 by any
		// intervening non-assistant line), so the builder starts empty too.
		m.resetAssistantStream()
		m.currentAssistant = m.appendTranscriptEntry("")
		m.transcript[m.currentAssistant].assistant = true
	}
	m.streamRaw.WriteString(text)
}

// renderPendingMarkdown runs only for a visible paint, never on a provider
// callback or when a hidden child settles. Caller holds m.mu.
func (m *replModel) renderPendingMarkdown() {
	if m.markdownPending {
		for i := range m.transcript {
			entry := &m.transcript[i]
			if entry.markdown == "" {
				continue
			}
			rendered, images, _ := renderMarkdownWithCache(entry.markdown, m.imageBaseDir, false, entry.codeCache)
			entry.markdown, entry.codeCache = "", nil
			m.setTranscriptEntry(i, rendered, images)
		}
		m.markdownPending = false
	}
	m.renderAssistantStream()
}

// renderAssistantStream renders the visible (holdback-trimmed) prefix of the
// streaming message through goldmark into its entry. An unchanged visible
// prefix skips the re-render, so a chunk that only extends a held-back token
// costs nothing.
func (m *replModel) renderAssistantStream() {
	if m.currentAssistant < 0 || m.currentAssistant >= len(m.transcript) {
		return
	}
	raw := m.streamRaw.String()
	visible := raw[:safeVisibleLen(raw)]
	if len(visible) == m.streamShown && m.transcript[m.currentAssistant].text != "" {
		return
	}
	m.streamShown = len(visible)
	if m.streamCodeCache == nil {
		m.streamCodeCache = &markdownCodeCache{}
	}
	rendered, images, _ := renderMarkdownWithCache(visible, m.imageBaseDir, true, m.streamCodeCache)
	m.setTranscriptEntry(m.currentAssistant, rendered, images)
}

// finishAssistantBlock closes the semantic assistant block that is currently
// streaming. Provider-owned terminal newlines are removed here so layout—not
// arbitrary chunk boundaries—owns the space between turns. Internal markdown
// whitespace is preserved. label, when non-empty, records that the visible
// partial block was not committed to session history.
func (m *replModel) finishAssistantBlock(label string) bool {
	if m.currentAssistant < 0 || m.currentAssistant >= len(m.transcript) {
		return false
	}
	idx := m.currentAssistant
	content := strings.TrimRight(m.streamRaw.String(), "\r\n")
	if strings.TrimSpace(content) == "" {
		content = ""
		m.deleteTranscriptEntry(idx)
	} else {
		m.transcript[idx].markdown = content
		m.transcript[idx].codeCache = m.streamCodeCache
		m.markdownPending = true
		m.visual.invalidate()
	}
	m.currentAssistant = -1
	m.resetAssistantStream()
	if label != "" && content != "" {
		m.appendLine("  " + styled(label, "muted", ""))
		m.outcomeLabeled = true
	}
	return content != ""
}

// labelTurnOutcome appends the turn's outcome label ("canceled/failed · …")
// unless the closing assistant block already carried it. Exactly one outcome
// label lands per settled turn with visible output.
func (m *replModel) labelTurnOutcome(label string) {
	if m.outcomeLabeled || !m.turnHasOutput {
		return
	}
	m.appendLine("  " + styled(label, "muted", ""))
	m.outcomeLabeled = true
}

func formattedUserPrompt(p string) string {
	lines := strings.Split(p, "\n")
	for i, line := range lines {
		if i == 0 {
			lines[i] = styled("> ", "accent", "bold") + styleEscape(line)
		} else {
			lines[i] = "  " + styleEscape(line)
		}
	}
	return strings.Join(lines, "\n")
}

func (m *replModel) appendUserPrompt(p string) {
	m.appendLine(formattedUserPrompt(p))
	m.transcript[len(m.transcript)-1].turnStart = true
}

// appendTurnSeparator inserts one renderer-owned blank row before a turn
// footer or a new user turn, without duplicating an existing blank entry.
func (m *replModel) appendTurnSeparator() {
	if len(m.transcript) == 0 || !m.transcriptEntryHasContent(len(m.transcript)-1) {
		return
	}
	m.appendLine("")
}

func (m *replModel) transcriptEntryHasContent(i int) bool {
	entry := m.transcript[i]
	return entry.text != "" || entry.markdown != "" || (i == m.currentAssistant && strings.TrimSpace(m.streamRaw.String()) != "")
}

// streamCursorGlyph is the caret appended to the streaming assistant block —
// the classic "the model is typing here" marker.
const streamCursorGlyph = "▍"

// streamCursorNow returns the styled caret for the current pulse phase, or ""
// when no assistant text is actively streaming. It breathes on the same
// bold↔dim cycle as the running-tool arrows. Caller must hold m.mu.
func (m *replModel) streamCursorNow() string {
	if !m.busy || m.canceling || m.state != turnStateStreaming || m.turnStarted.IsZero() {
		return ""
	}
	mod := arrowPulse[int(time.Since(m.turnStarted)/arrowPulsePeriod)%len(arrowPulse)]
	return styled(streamCursorGlyph, "accent", mod)
}

// refreshStreamCursor recomputes the caret frame, invalidating the visual
// cache only when it changes — the caret is display-only chrome and never
// enters the transcript. Caller must hold m.mu.
func (m *replModel) refreshStreamCursor() {
	next := m.streamCursorNow()
	if next != m.streamCursorFrame {
		m.streamCursorFrame = next
		m.visual.invalidate()
	}
}

func (m *replModel) appendNoticeLine(text string) {
	m.appendLine(styled(text, "muted", ""))
}

// appendErrorLine reports a failure inline and follows it, so the user sees
// why the composer kept their input.
func (m *replModel) appendErrorLine(text string) {
	m.appendLine(styled("Error: "+text, "err", ""))
	m.followBottom = true
}

func (m *replModel) clearDisplay() {
	m.transcript = nil
	m.markdownPending = false
	m.currentAssistant = -1
	m.activeTools = nil
	m.activeToolsPhase = -1
	m.clearToolDisclosures()
	m.clearReasoningRecords()
	m.turnTrailers = make(map[int64]*turnTrailerRecord)
	m.turnTrailerAt = make(map[int]int64)
	m.turnTrailerSeq = 0
	m.turnTrailerPlacements = nil
	m.agentLinkPlacements = nil
	m.agentDisclosurePlacements = nil
	m.openTurnTrailerID = 0
	m.turnDock.overlay = turnDockOverlayNone
	for i := range m.queue {
		m.queue[i].transcriptShown = false
	}
	if !m.busy {
		m.runningTools = 0
		m.clearTurnDock()
	}
	m.resetAssistantStream()
	m.scrollAnchor = 0
	m.followBottom = true
	m.visual.invalidate()
}

// transcriptVisualCache keeps the styled, wrapped transcript for the current
// terminal width and image geometry, so busy spinner ticks redraw only
// visible rows without reparsing a long, unchanged context 20 times per
// second. Every transcript mutation invalidates it; a geometry change
// rebuilds it on the next frame.
type transcriptVisualCache struct {
	rows   [][]ui.Cell
	blocks []transcriptVisualBlock
	valid  bool

	// The geometry rows were built for.
	width        int
	nativeImages bool
	cellWidth    int
	cellHeight   int
}

func (c *transcriptVisualCache) invalidate() { c.valid = false }

// fits reports whether the cache was built for this geometry.
func (c *transcriptVisualCache) fits(width int, nativeImages bool, cellWidth, cellHeight int) bool {
	return c.width == width && c.nativeImages == nativeImages &&
		c.cellWidth == cellWidth && c.cellHeight == cellHeight
}

// transcriptRows returns the styled, wrapped transcript for width. Visual
// clipping happens after style parsing and wrapping in transcriptParagraph,
// so wrapped rows remain reachable through scrollback.
func (m *replModel) transcriptRows(width int) [][]ui.Cell {
	if width < 1 {
		width = 1
	}
	if m.nativeImages && m.refreshTranscriptImageSources(width) {
		m.visual.invalidate()
	}
	c := &m.visual
	fits := c.fits(width, m.nativeImages, m.imageCellWidth, m.imageCellHeight)
	if c.valid && fits {
		return c.rows
	}

	sources := m.transcriptDisplayEntries(width)
	oldBlocks := c.blocks
	canPatch := fits && len(oldBlocks) == len(sources)
	if len(oldBlocks) != len(sources) {
		c.blocks = make([]transcriptVisualBlock, len(sources))
	}

	offset := 0
	for i, source := range sources {
		followed := i < len(sources)-1
		var old transcriptVisualBlock
		if i < len(oldBlocks) {
			old = oldBlocks[i]
		}
		rows := old.rows
		imageSpans := old.imageSpans
		changed := !fits ||
			old.key != source.key || old.text != source.text || old.followed != followed ||
			!slices.Equal(old.reasoningIDs, source.reasoningIDs) ||
			!slices.Equal(old.toolDisclosureIDs, source.toolDisclosureIDs) ||
			old.turnTrailerID != source.turnTrailerID ||
			!transcriptImagesEqual(old.images, source.images)
		if changed {
			nativeSlots := m.nativeImages && width >= minimumImageThumbnailCols
			rows, imageSpans = transcriptBlockRowsWithImages(
				source.text, followed, width, source.images, nativeSlots,
				m.imageCellWidth, m.imageCellHeight,
			)
		}
		if canPatch && len(rows) != len(old.rows) {
			canPatch = false
		}
		if canPatch && changed {
			copy(c.rows[offset:offset+len(rows)], rows)
		}
		c.blocks[i] = transcriptVisualBlock{
			key:               source.key,
			text:              source.text,
			followed:          followed,
			rows:              rows,
			images:            append([]transcriptImage(nil), source.images...),
			imageSpans:        imageSpans,
			reasoningIDs:      append([]int64(nil), source.reasoningIDs...),
			toolDisclosureIDs: append([]int64(nil), source.toolDisclosureIDs...),
			turnTrailerID:     source.turnTrailerID,
			activityFields:    append([]turnDockPlacement(nil), source.activityFields...),
			agentLinks:        append([]agentLink(nil), source.agentLinks...),
		}
		offset += len(old.rows)
	}

	if !canPatch {
		total := 0
		for _, block := range c.blocks {
			total += len(block.rows)
		}
		rows := make([][]ui.Cell, 0, total)
		for _, block := range c.blocks {
			rows = append(rows, block.rows...)
		}
		c.rows = rows
	}
	c.width = width
	c.nativeImages = m.nativeImages
	c.cellWidth = m.imageCellWidth
	c.cellHeight = m.imageCellHeight
	c.valid = true
	return c.rows
}

func (m *replModel) transcriptDisplayEntries(width int) []transcriptDisplayBlock {
	blocks := make([]transcriptDisplayBlock, 0, len(m.transcript)+1)
	for i := range m.transcript {
		entry := m.transcript[i].text
		reasoningID := m.reasoningAt[i]
		toolDisclosureID := m.toolDisclosureAt[i]
		turnTrailerID := m.turnTrailerAt[i]
		// Reasoning and tool activity render inline where they occur. In quiet
		// mode the records still back the settled trailer, but nothing extra is
		// projected into the transcript so script output stays clean.
		if m.quiet && (reasoningID != 0 || toolDisclosureID != 0) {
			continue
		}
		if i == m.currentAssistant {
			entry = strings.TrimRight(entry, "\r\n")
			if entry == "" {
				continue
			}
			images := m.transcript[i].images
			if len(images) > 0 && strings.HasSuffix(entry, string(transcriptImageMarker(len(images)-1))) {
				// Keep the pulsing stream caret out of the final reserved image
				// row. The newline is stable even on the caret's hidden frame, so
				// follow-bottom does not oscillate by one row.
				entry += "\n" + m.streamCursorFrame
			} else {
				entry += m.streamCursorFrame
			}
		}
		if stats := m.transcript[i].turnStats; stats != "" && !m.quiet {
			entry = strings.TrimRight(entry, "\r\n")
			separator := "  "
			images := m.transcript[i].images
			if len(images) > 0 && strings.HasSuffix(entry, string(transcriptImageMarker(len(images)-1))) {
				separator = "\n"
			} else {
				// Keep a fitting stats group together instead of stranding a
				// token arrow on the following row. Measure only the final
				// logical line; earlier paragraphs do not affect its wrapping.
				lastLine := entry[strings.LastIndex(entry, "\n")+1:]
				rows := transcriptVisualRows(lastLine, ui.NewStyle(ui.ColorClear), width)
				statsWidth := styledTextWidth(stats)
				if len(rows) > 0 && statsWidth <= width && transcriptCellsWidth(rows[len(rows)-1])+2+statsWidth > width {
					separator = "\n"
				}
			}
			entry += separator + stats
		}
		key := fmt.Sprintf("transcript:%d", i)
		if turnTrailerID != 0 {
			key = fmt.Sprintf("turn-trailer:%d", turnTrailerID)
		} else if reasoningID != 0 {
			key = fmt.Sprintf("reasoning:%d", reasoningID)
		} else if toolDisclosureID != 0 {
			key = fmt.Sprintf("tools:%d", toolDisclosureID)
		}
		block := transcriptDisplayBlock{
			key:           key,
			text:          entry,
			images:        m.transcript[i].images,
			turnTrailerID: turnTrailerID,
		}
		if trailer := m.turnTrailers[turnTrailerID]; trailer != nil && trailer.dock.overlay == turnDockOverlayAgents {
			// Rebuild links and detail at the current width, just as inline
			// activity does; stored trailer text may precede a resize.
			block.text, trailer.fields = m.turnDockRowFor(trailer.dock, width)
			m.appendAgentDetail(&block, trailer.dock.toolIDs, width)
		}
		if reasoningID != 0 {
			block.reasoningIDs = []int64{reasoningID}
		}
		if toolDisclosureID != 0 {
			block.toolDisclosureIDs = []int64{toolDisclosureID}
		}
		blocks = append(blocks, block)
	}
	if m.slashHints != "" {
		blocks = append(blocks, transcriptDisplayBlock{key: "slash", text: styled(m.slashHints, "muted", "")})
	}
	return m.layoutInlineActivityBlocks(blocks, width)
}

func transcriptBlockRowsWithImages(text string, followed bool, width int, images []transcriptImage, native bool, cellWidth, cellHeight int) ([][]ui.Cell, []transcriptImageSpan) {
	cells := parseStyledCells(text, ui.NewStyle(ui.ColorClear))
	if followed {
		// The joined renderer places one newline between transcript blocks. Add
		// that separator before splitting: a normal separator is absorbed by the
		// following block, while a block that already ends in a (possibly styled)
		// newline correctly produces an interior blank row.
		cells = append(cells, ui.Cell{Rune: '\n', Style: ui.StyleClear})
	}
	rows := ui.SplitCells(wrapTranscriptCells(cells, width), '\n')
	return locateTranscriptImages(rows, images, native, width, cellWidth, cellHeight)
}

// scrollByWidth moves the scroll anchor by delta display rows (negative = up)
// at the given layout width. Caller must hold m.mu. Disengages followBottom
// on first upward scroll; re-engages when the user scrolls back to the bottom.
func (m *replModel) scrollByWidth(delta, viewportHeight, width int) {
	total := len(m.transcriptRows(width))
	if total <= viewportHeight {
		m.followBottom = true
		m.scrollAnchor = 0
		return
	}

	if m.followBottom {
		m.scrollAnchor = total - viewportHeight
	}
	m.followBottom = false
	m.scrollAnchor += delta
	if m.scrollAnchor < 0 {
		m.scrollAnchor = 0
	}
	if m.scrollAnchor >= total-viewportHeight {
		m.scrollAnchor = total - viewportHeight
		m.followBottom = true
	}
}

func (m *replModel) scrollToBottom() {
	m.followBottom = true
}

// settleScroll clamps the held anchor to the rows that exist at this width
// and re-engages followBottom once it reaches the end, returning the frame's
// top row and whether the pane pins to the bottom. Caller must hold m.mu.
func (m *replModel) settleScroll(totalRows, viewportHeight int) (topRow int, pinBottom bool) {
	if m.followBottom {
		return m.scrollAnchor, true
	}
	topRow = m.scrollAnchor
	if maxTop := max(0, totalRows-viewportHeight); topRow >= maxTop {
		topRow = maxTop
		m.followBottom = true
	}
	topRow = max(topRow, 0)
	m.scrollAnchor = topRow
	return topRow, m.followBottom
}

// transcriptViewport is the window of display rows the transcript pane shows
// this frame, plus the screen offset every placement projects through. It is
// resolved once per frame by frameLayout.transcriptViewport.
type transcriptViewport struct {
	start, end int // display rows in [start, end) are on screen
	topPadding int // blank rows above a transcript shorter than the pane
	logoRows   int
	width      int
}

func (v transcriptViewport) contains(row int) bool { return row >= v.start && row < v.end }

// screenY maps a display row inside the window to its screen row.
func (v transcriptViewport) screenY(row int) int { return v.logoRows + v.topPadding + row - v.start }
