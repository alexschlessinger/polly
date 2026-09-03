package main

import (
	"strings"
	"time"

	rw "github.com/mattn/go-runewidth"
)

// Reasoning records: streamed thinking previews and their disclosures.

// reasoningPreviewLines is the bounded expanded tail. The preview uses the
// full transcript width after its block indent. The retained/limit pair bounds
// the duplicate display copy while amortizing compaction of streamed chunks.
const (
	reasoningPreviewLines    = 5
	reasoningTailRetainRunes = 8192
	reasoningTailLimitRunes  = 2 * reasoningTailRetainRunes
)

const reasoningBlockIndent = "    "

// newReasoningRecord appends one stable disclosure row for a turn. Later
// reasoning segments update this record instead of adding more transcript
// rows. Caller must hold m.mu.
func (m *replModel) newReasoningRecord(complete bool) *reasoningRecord {
	m.reasoningSeq++
	record := &reasoningRecord{id: m.reasoningSeq, complete: complete}
	if !complete {
		record.expanded = m.turnReasoningOpen
		// The pending shortcut applies only to the first record it creates. An
		// expanded record must not silently pre-arm later reasoning segments.
		m.turnReasoningOpen = false
	}
	m.appendLine("")
	record.transcriptIndex = len(m.transcript) - 1
	m.reasoningRecords[record.id] = record
	m.reasoningAt[record.transcriptIndex] = record.id
	m.reasoningOrder = append(m.reasoningOrder, record.id)
	m.refreshReasoningRecord(record, 80)
	return record
}

func (m *replModel) currentReasoningRecord() *reasoningRecord {
	if m.turnReasoningID == 0 {
		return nil
	}
	return m.reasoningRecords[m.turnReasoningID]
}

// appendThinking adds one streamed provider chunk to this turn's bounded UI
// tail. Complete reasoning remains on the assistant ChatMessage and is not
// duplicated here. Caller must hold m.mu.
func (m *replModel) appendThinking(chunk string) {
	if chunk == "" {
		return
	}
	record := m.currentReasoningRecord()
	resumed := false
	if record == nil {
		// A new disclosure opens at the current transcript position only when
		// assistant prose broke the run (or the turn just started), so an
		// unbroken thinking→tools→thinking continuation keeps aggregating
		// into one indicator instead of stranding an expanded block.
		record = m.newReasoningRecord(false)
		m.turnReasoningID = record.id
		m.turnReasoningIDs = append(m.turnReasoningIDs, record.id)
		m.thinkingSegmentOpen = true
		m.thinkingSegmentStart = time.Now()
		record.active = true
	} else if !m.thinkingSegmentOpen {
		// Resuming after a tool phase paused the segment: same record, fresh
		// clock, one semantic break between the tool-separated passages.
		m.thinkingSegmentOpen = true
		m.thinkingSegmentStart = time.Now()
		record.active = true
		resumed = true
	}
	m.appendReasoningTail(record, chunk, resumed)
	// Width-aware wrapping belongs to the render loop. Provider callbacks only
	// mutate the bounded semantic tail under the model lock.
}

// appendReasoningTail adds text to a bounded rune tail. segmentBreak inserts
// one semantic newline between tool-separated assistant reasoning segments.
func (m *replModel) appendReasoningTail(record *reasoningRecord, text string, segmentBreak bool) {
	if record == nil {
		return
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	// Expanded reasoning can share a visual block with tool image sidecars.
	// Remove reserved slot runes before they can claim those tool images.
	text = stripTranscriptImageMarkers(text)
	addition := []rune(text)
	if segmentBreak && len(record.tail) > 0 && record.tail[len(record.tail)-1] != '\n' {
		withBreak := make([]rune, 1, len(addition)+1)
		withBreak[0] = '\n'
		addition = append(withBreak, addition...)
	}
	if len(addition) >= reasoningTailRetainRunes {
		record.tail = append(record.tail[:0], addition[len(addition)-reasoningTailRetainRunes:]...)
		record.tailVersion++
		record.dirty = true
		return
	}
	// Compact at the hard limit instead of copying the whole tail on every
	// streamed token after the retained target first fills.
	if len(record.tail)+len(addition) > reasoningTailLimitRunes {
		keep := reasoningTailRetainRunes - len(addition)
		next := make([]rune, 0, reasoningTailRetainRunes)
		if keep > 0 && len(record.tail) > keep {
			next = append(next, record.tail[len(record.tail)-keep:]...)
		} else if keep > 0 {
			next = append(next, record.tail...)
		}
		record.tail = append(next, addition...)
		record.tailVersion++
		record.dirty = true
		return
	}
	record.tail = append(record.tail, addition...)
	if len(addition) > 0 {
		record.tailVersion++
		record.dirty = true
	}
}

// finishThinkingSegment closes the current provider segment's disclosure.
// The segment settles in place; a later reasoning segment opens a fresh
// disclosure at its own transcript position. Caller must hold m.mu.
func (m *replModel) finishThinkingSegment() {
	record := m.currentReasoningRecord()
	if record == nil {
		m.thinkingSegmentOpen = false
		m.thinkingSegmentStart = time.Time{}
		return
	}
	// The record may be paused (tool phase mid-run) rather than live; either
	// way it is the current run and prose or settlement closes it here. A
	// paused record already banked its elapsed time.
	if m.thinkingSegmentOpen {
		record.elapsed += time.Since(m.thinkingSegmentStart)
	}
	record.active = false
	record.complete = true
	record.dirty = true
	// Refresh unconditionally: reasoningWidth may be zero in provider
	// callbacks, and refreshReasoningRecord treats that as "keep the
	// existing text", which would strand the live "Thinking…" label.
	// A deliberately expanded segment stays open until the turn settles.
	m.refreshReasoningRecord(record, m.reasoningWidth)
	m.turnReasoningID = 0
	m.thinkingSegmentOpen = false
	m.thinkingSegmentStart = time.Time{}
}

// pauseThinkingSegment banks the running segment's elapsed time and stops its
// live clock without closing the record: the turn's next reasoning chunk
// resumes the same disclosure. Tool phases pause; only assistant prose (or
// turn settlement) closes the run. Caller must hold m.mu.
func (m *replModel) pauseThinkingSegment() {
	if !m.thinkingSegmentOpen {
		return
	}
	if record := m.currentReasoningRecord(); record != nil {
		record.elapsed += time.Since(m.thinkingSegmentStart)
		record.active = false
		record.dirty = true
		m.refreshReasoningRecord(record, m.reasoningWidth)
	}
	m.thinkingSegmentOpen = false
	m.thinkingSegmentStart = time.Time{}
}

// completeThinkingTurn settles any still-open reasoning segment and marks
// whether the provider-generated messages were saved. Caller must hold m.mu.
func (m *replModel) completeThinkingTurn(unsaved bool) {
	m.finishThinkingSegment()
	// Every segment of the turn auto-collapses at settlement; the "not saved"
	// marker lands on each when the turn persisted nothing.
	for _, id := range m.turnReasoningIDs {
		record := m.reasoningRecords[id]
		if record == nil {
			continue
		}
		record.complete = true
		record.expanded = false
		if unsaved {
			record.unsaved = true
		}
		record.dirty = true
		m.refreshReasoningRecord(record, m.reasoningWidth)
	}
	m.resetCurrentThinking()
}

func (m *replModel) resetCurrentThinking() {
	m.turnReasoningID = 0
	m.turnReasoningIDs = nil
	m.turnReasoningOpen = false
	m.thinkingSegmentOpen = false
	m.thinkingSegmentStart = time.Time{}
}

func (m *replModel) clearReasoningRecords() {
	m.reasoningRecords = make(map[int64]*reasoningRecord)
	m.reasoningAt = make(map[int]int64)
	m.reasoningOrder = nil
	m.reasoningPlacements = nil
	m.reasoningSeq = 0
	m.resetCurrentThinking()
}

func (m *replModel) refreshReasoningRecords(width int) {
	widthChanged := width > 0 && width != m.reasoningWidth
	if width > 0 {
		m.reasoningWidth = width
	}
	if widthChanged {
		for _, id := range m.reasoningOrder {
			m.refreshReasoningRecord(m.reasoningRecords[id], width)
		}
		return
	}
	// Refresh every record of the active turn: a turn now spans several
	// per-segment disclosures, and a settled segment may still be dirty.
	for _, id := range m.turnReasoningIDs {
		if record := m.reasoningRecords[id]; record != nil && (record.active || record.dirty) {
			m.refreshReasoningRecord(record, width)
		}
	}
}

func (m *replModel) refreshReasoningRecord(record *reasoningRecord, width int) {
	m.refreshReasoningRecordWithAnchor(record, width, true)
}

func (m *replModel) refreshReasoningRecordWithAnchor(record *reasoningRecord, width int, reanchor bool) {
	if record == nil || record.transcriptIndex < 0 || record.transcriptIndex >= len(m.transcript) {
		return
	}
	width = m.disclosureLayoutWidth(width)
	next := m.reasoningRecordText(record, width)
	record.dirty = false
	if m.transcript[record.transcriptIndex].text == next {
		return
	}
	// Reasoning renders inline in the transcript, so growth re-anchors a held
	// viewport like any other transcript mutation.
	if !reanchor {
		m.setTranscriptText(record.transcriptIndex, next)
		return
	}
	m.mutateAnchored(width, matchReasoningGroup([]int64{record.id}), func(bool) {
		m.setTranscriptText(record.transcriptIndex, next)
	})
}

func (m *replModel) reasoningRecordText(record *reasoningRecord, width int) string {
	glyph := "▸"
	if record.expanded {
		glyph = "▾"
	}

	elapsed := record.elapsed
	if record.active && !m.thinkingSegmentStart.IsZero() && record.id == m.turnReasoningID {
		elapsed += time.Since(m.thinkingSegmentStart)
	}
	label := reasoningDisclosureLabel(record.active, record.unsaved, elapsed)
	header := "  " + inlineActivityControl(glyph, label, record.complete && !record.active)
	if !record.expanded {
		return header
	}

	contentWidth := width - rw.StringWidth(reasoningBlockIndent)
	if contentWidth < 2 {
		return header
	}
	if record.previewWidth != contentWidth || record.previewVersion != record.tailVersion {
		record.previewLines = reasoningTailLines(string(record.tail), contentWidth, reasoningPreviewLines)
		record.previewWidth = contentWidth
		record.previewVersion = record.tailVersion
	}
	lines := record.previewLines
	if len(lines) == 0 {
		return header
	}
	var b strings.Builder
	b.WriteString(header)
	// Keep the disclosure visually stable and worth opening even for a short
	// thought: when the terminal is wide enough for detail, reserve two rows.
	detailRows := max(2, len(lines))
	for i := 0; i < detailRows; i++ {
		line := ""
		if i < len(lines) {
			line = lines[i]
		}
		b.WriteString("\n")
		b.WriteString(reasoningBlockIndent)
		b.WriteString(styled(line, "muted", "italic"))
	}
	return b.String()
}

func reasoningDisclosureLabel(active, unsaved bool, elapsed time.Duration) string {
	// A paused segment (tool phase mid-run) reads "Thought" like a settled
	// one; the header styling, not the label, distinguishes live from done.
	label := "thought"
	if active {
		label = "thinking…"
	}
	if elapsed > 0 {
		label += " " + formatElapsed(elapsed)
	}
	if unsaved {
		label += " · not saved"
	}
	return label
}

// reasoningTailLines wraps the retained text for the current terminal width,
// then returns only its newest physical rows.
func reasoningTailLines(text string, width, limit int) []string {
	if width < 1 || limit < 1 {
		return nil
	}
	lines := make([]string, 0, limit)
	push := func(line string) {
		line = strings.TrimSpace(line)
		if line == "" {
			return
		}
		if len(lines) == limit {
			copy(lines, lines[1:])
			lines[len(lines)-1] = line
			return
		}
		lines = append(lines, line)
	}

	for _, paragraph := range strings.Split(strings.TrimSpace(text), "\n") {
		current := ""
		currentWidth := 0
		flush := func() {
			push(current)
			current = ""
			currentWidth = 0
		}
		for _, word := range strings.Fields(paragraph) {
			wordWidth := rw.StringWidth(word)
			if wordWidth > width {
				flush()
				pieces := splitReasoningWord(word, width)
				for i, piece := range pieces {
					if i < len(pieces)-1 {
						push(piece)
						continue
					}
					current = piece
					currentWidth = rw.StringWidth(piece)
				}
				continue
			}
			if current == "" {
				current, currentWidth = word, wordWidth
				continue
			}
			if currentWidth+1+wordWidth <= width {
				current += " " + word
				currentWidth += 1 + wordWidth
				continue
			}
			flush()
			current, currentWidth = word, wordWidth
		}
		flush()
	}
	return lines
}

func splitReasoningWord(word string, width int) []string {
	var pieces []string
	var chunk []rune
	chunkWidth := 0
	for _, r := range word {
		runeWidth := max(0, rw.RuneWidth(r))
		if len(chunk) > 0 && chunkWidth+runeWidth > width {
			pieces = append(pieces, string(chunk))
			chunk = chunk[:0]
			chunkWidth = 0
		}
		chunk = append(chunk, r)
		chunkWidth += runeWidth
	}
	if len(chunk) > 0 {
		pieces = append(pieces, string(chunk))
	}
	return pieces
}

func (m *replModel) toggleReasoning(recordID int64, width int) bool {
	record := m.reasoningRecords[recordID]
	if record == nil || record.transcriptIndex < 0 || record.transcriptIndex >= len(m.transcript) {
		return false
	}
	if width > 0 {
		m.reasoningWidth = width
	}
	record.expanded = !record.expanded
	m.refreshReasoningRecord(record, width)
	return true
}

// disclosureLayoutWidth resolves the width inline activity is laid out at:
// the explicit width when usable, else the last renderer width, else 80.
func (m *replModel) disclosureLayoutWidth(width int) int {
	if width < 2 {
		width = m.reasoningWidth
	}
	if width < 2 {
		return 80
	}
	return width
}

// latestTurnReasoningGroup returns the current turn's newest projected inline
// reasoning group. Adjacent thought/tool records share one visual control, so
// the keyboard shortcut must use the same grouping as mouse hit-testing.
func (m *replModel) latestTurnReasoningGroup(width int) []int64 {
	if len(m.turnReasoningIDs) == 0 {
		return nil
	}
	currentTurn := make(map[int64]struct{}, len(m.turnReasoningIDs))
	for _, id := range m.turnReasoningIDs {
		currentTurn[id] = struct{}{}
	}
	blocks := m.transcriptDisplayEntries(m.disclosureLayoutWidth(width))
	for i := len(blocks) - 1; i >= 0; i-- {
		var ids []int64
		for _, id := range blocks[i].reasoningIDs {
			if _, ok := currentTurn[id]; ok && m.reasoningRecords[id] != nil {
				ids = append(ids, id)
			}
		}
		if len(ids) > 0 {
			return ids
		}
	}
	// Quiet mode does not project inline activity. Keep the shortcut useful by
	// falling back to the newest current-turn record in that mode.
	for i := len(m.turnReasoningIDs) - 1; i >= 0; i-- {
		id := m.turnReasoningIDs[i]
		if m.reasoningRecords[id] != nil {
			return []int64{id}
		}
	}
	return nil
}

func (m *replModel) toggleLatestReasoning(width int) bool {
	if m.busy {
		if ids := m.latestTurnReasoningGroup(width); len(ids) > 0 {
			if len(ids) == 1 {
				return m.toggleReasoning(ids[0], width)
			}
			return m.toggleReasoningGroup(ids, width)
		}
		// No current-turn record exists yet. Remember the user's choice only
		// until the first record is created; never target an older turn.
		m.turnReasoningOpen = !m.turnReasoningOpen
		return true
	}
	for i := len(m.reasoningOrder) - 1; i >= 0; i-- {
		if m.toggleReasoning(m.reasoningOrder[i], width) {
			return true
		}
	}
	return false
}
