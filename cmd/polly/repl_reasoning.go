package main

import (
	"slices"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/markdown"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	rw "github.com/mattn/go-runewidth"
)

// Reasoning records: streamed thinking previews and their disclosures.

// The retained/limit pair bounds the duplicate display copy of a thought
// while amortizing compaction of streamed chunks. An opened thought part
// shows every retained row of its step, at the full transcript width after
// the indent of a row's detail.
const (
	reasoningTailRetainRunes = 8192
	reasoningTailLimitRunes  = 2 * reasoningTailRetainRunes
)

const reasoningBlockIndent = style.RailIndent

// newReasoningRecord appends one stable disclosure row for a turn. Later
// reasoning segments update this record instead of adding more transcript
// rows. Caller must hold m.mu.
func (m *replModel) newReasoningRecord(complete bool) *reasoningRecord {
	record := &reasoningRecord{complete: complete}
	// The initial header names style roles but resolves no theme colors.
	// Provider callbacks and history replay leave the preview for paint.
	m.appendLine(m.reasoningRecordText(record, 80))
	m.reasoningRecords.add(record, len(m.transcript)-1)
	m.reasoningOrder = append(m.reasoningOrder, record.id)
	record.dirty = true
	return record
}

func (m *replModel) currentReasoningRecord() *reasoningRecord {
	if m.turnReasoningID == 0 {
		return nil
	}
	return m.reasoningRecords.get(m.turnReasoningID)
}

// appendThinking adds one streamed provider chunk to this turn's bounded UI
// tail. Complete reasoning remains on the assistant ChatMessage and is not
// duplicated here. Caller must hold m.mu.
func (m *replModel) appendThinking(chunk string) {
	if chunk == "" {
		return
	}
	record := m.currentReasoningRecord()
	if record == nil || !m.thinkingSegmentOpen {
		if record == nil {
			// A new disclosure opens at the current transcript position only when
			// assistant prose broke the run (or the turn just started), so an
			// unbroken thinking→tools→thinking continuation keeps aggregating
			// into one indicator instead of stranding an expanded block.
			record = m.newReasoningRecord(false)
			m.turnReasoningID = record.id
			m.turnReasoningIDs = append(m.turnReasoningIDs, record.id)
		}
		// Start the clock, but preserve delta concatenation within a response.
		// Only tools or a new model response request a semantic separator.
		m.thinkingSegmentOpen = true
		m.thinkingSegmentStart = time.Now()
		record.active = true
	}
	m.appendReasoningTail(record, chunk, m.thinkingSegmentBreak)
	m.thinkingSegmentBreak = false
	// Width-aware wrapping belongs to the render loop. Provider callbacks only
	// mutate the bounded semantic tail under the model lock.
}

// appendReasoningTail adds text to a bounded rune tail. segmentBreak inserts
// one semantic newline between tool-separated assistant reasoning segments.
func (m *replModel) appendReasoningTail(record *reasoningRecord, text string, segmentBreak bool) {
	m.displayCatalog.thoughtKey(&record.sectionKey)
	version := record.tailVersion
	// A reader following the running turn's thinking sees each new step's
	// thought open as it starts. Replayed history opens nothing.
	record.beginSegment(m.turnStep, m.turnStepAt, m.turnStepKnown, segmentBreak, m.followThoughts && m.busy && !m.hydrating)
	record.trimSegments(appendReasoningTail(record, text, segmentBreak))
	if record.tailVersion != version {
		m.visual.invalidate()
	}
}

// appendReasoningTail is the bounded append itself; it returns how many
// runes compaction dropped from the front of the tail.
func appendReasoningTail(record *reasoningRecord, text string, segmentBreak bool) (dropped int) {
	if record == nil {
		return 0
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	// Expanded reasoning can share a visual block with tool image sidecars.
	// Remove reserved slot runes before they can claim those tool images.
	text = style.StripImageMarkers(text)
	addition := []rune(text)
	if segmentBreak && len(record.tail) > 0 && record.tail[len(record.tail)-1] != '\n' {
		withBreak := make([]rune, 1, len(addition)+1)
		withBreak[0] = '\n'
		addition = append(withBreak, addition...)
	}
	// Compact large chunks immediately, or at the hard limit for small deltas.
	// Keep fence context so a retained closer cannot swallow later prose.
	if len(addition) >= reasoningTailRetainRunes || len(record.tail)+len(addition) > reasoningTailLimitRunes {
		record.tailCut = record.tailCut || len(record.tail)+len(addition) > reasoningTailRetainRunes
		whole := len(record.tail) + len(addition)
		tail, prefix := markdown.TrimPreview(record.tailPrefix+string(record.tail)+string(addition), reasoningTailRetainRunes)
		record.tail = []rune(tail)
		record.tailPrefix = prefix
		record.tailVersion++
		record.dirty = true
		return max(0, whole-len(record.tail))
	}
	record.tail = append(record.tail, addition...)
	if len(addition) > 0 {
		record.tailVersion++
		record.dirty = true
	}
	return 0
}

// addThinkingTime banks one interval to both the whole record and the step
// that produced it. Commentary can pause the same step's clock several times.
func (record *reasoningRecord) addThinkingTime(d time.Duration) {
	record.elapsed += d
	if n := len(record.segments); n > 0 {
		record.segments[n-1].elapsed += d
	}
}

// finishThinkingSegment closes the current provider segment's disclosure.
// The segment settles in place; a later reasoning segment opens a fresh
// disclosure at its own transcript position. Caller must hold m.mu.
func (m *replModel) finishThinkingSegment() {
	m.thinkingSegmentBreak = false
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
		record.addThinkingTime(time.Since(m.thinkingSegmentStart))
	}
	record.active = false
	record.complete = true
	m.markReasoningDirty(record)
	// Paint freezes the header and flushes any held Markdown. A provider
	// callback must not resolve colors while another tab applies a theme.
	// A deliberately expanded segment stays open until the turn settles.
	m.turnReasoningID = 0
	m.thinkingSegmentOpen = false
	m.thinkingSegmentStart = time.Time{}
}

// pauseThinkingSegment separates tool/model rounds within one disclosure, even
// if commentary or completion already paused its clock. Caller must hold m.mu.
func (m *replModel) pauseThinkingSegment() {
	m.thinkingSegmentBreak = true
	m.pauseThinkingClock()
}

// pauseThinkingClock banks elapsed time without changing text boundaries.
// Hidden commentary can interrupt reasoning within one response, whose deltas
// still concatenate exactly as streamed. Caller must hold m.mu.
func (m *replModel) pauseThinkingClock() {
	if !m.thinkingSegmentOpen {
		return
	}
	if record := m.currentReasoningRecord(); record != nil {
		record.addThinkingTime(time.Since(m.thinkingSegmentStart))
		record.active = false
		m.markReasoningDirty(record)
	}
	m.thinkingSegmentOpen = false
	m.thinkingSegmentStart = time.Time{}
}

// completeThinkingTurn settles any still-open reasoning segment and marks
// whether the provider-generated messages were saved. Caller must hold m.mu.
func (m *replModel) completeThinkingTurn(unsaved bool) {
	m.finishThinkingSegment()
	m.followThoughts = false
	// Every segment of the turn settles where it stands; the newest line is
	// the open one until the next turn begins. The "not saved" marker lands on
	// each when the turn persisted nothing.
	for _, id := range m.turnReasoningIDs {
		record := m.reasoningRecords.get(id)
		if record == nil {
			continue
		}
		record.complete = true
		if unsaved {
			record.unsaved = true
		}
		m.markReasoningDirty(record)
	}
	m.resetCurrentThinking()
}

func (m *replModel) resetCurrentThinking() {
	m.turnReasoningID = 0
	m.turnReasoningIDs = nil
	m.thinkingSegmentOpen = false
	m.thinkingSegmentBreak = false
	m.thinkingSegmentStart = time.Time{}
}

func (m *replModel) clearReasoningRecords() {
	m.reasoningRecords.reset()
	m.reasoningOrder = nil
	m.disclosurePlacements[activityThought] = nil
	m.resetCurrentThinking()
}

// markReasoningDirty schedules a paint and advances the source revision without
// parsing Markdown. Background-tab inspector snapshots must observe settlement
// and retry changes even when that tab has never painted its preview.
func (m *replModel) markReasoningDirty(record *reasoningRecord) {
	record.dirty = true
	m.visual.invalidate()
}

// refreshReasoningRecords renders pending previews on the event-loop paint.
// Markdown wrapping resolves the global theme map, so neither provider callbacks
// nor background history/projection builders may call it. Caller holds m.mu or
// owns the isolated display model being painted.
func (m *replModel) refreshReasoningRecords(width int) {
	widthChanged := width > 0 && width != m.reasoningWidth
	if width > 0 {
		m.reasoningWidth = width
	}
	// A settled record can still be dirty, including an older disclosure a
	// projection re-expanded or an aborted attempt restored to its prefix.
	for _, id := range m.reasoningOrder {
		record := m.reasoningRecords.get(id)
		if record == nil || !(widthChanged || record.active || record.dirty) {
			continue
		}
		// The activity line lays the preview rows out straight from the
		// record, so a held viewport is measured before they re-render.
		m.mutateAnchored(m.disclosureLayoutWidth(width), matchReasoningGroup([]int64{record.id}), func(bool) {
			m.renderReasoningPreview(record, width)
			m.refreshReasoningRecordWithAnchor(record, width, false)
		})
		record.dirty = false
	}
}

// refreshReasoningRecord updates disclosure markup using only the last painted
// preview. Projection builders and disclosure toggles may use it, but only paint
// renders new Markdown and clears dirty. Provider callbacks just mark dirty.
func (m *replModel) refreshReasoningRecord(record *reasoningRecord, width int) {
	m.refreshReasoningRecordWithAnchor(record, width, true)
}

func (m *replModel) refreshReasoningRecordWithAnchor(record *reasoningRecord, width int, reanchor bool) {
	if record == nil || record.transcriptIndex < 0 || record.transcriptIndex >= len(m.transcript) {
		return
	}
	width = m.disclosureLayoutWidth(width)
	next := m.reasoningRecordText(record, width)
	record.dirty = true
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

// renderReasoningPreview is paint-only; reasoningRecordText merely assembles
// role-named markup from this cache and can run during display projection.
func (m *replModel) renderReasoningPreview(record *reasoningRecord, width int) {
	// A thought's text sits under its row like a call's output, two columns
	// further in. A thought with no part open is one row and renders nothing.
	contentWidth := m.disclosureLayoutWidth(width) - rw.StringWidth(reasoningBlockIndent) - toolBodyCols
	if !record.anyOpen() || contentWidth < 2 {
		return
	}
	if record.previewWidth != contentWidth || record.previewVersion != record.tailVersion || record.previewActive != record.active {
		if record.previewCode == nil {
			record.previewCode = &markdown.CodeCache{}
		}
		// Each step's part of the thought renders on its own, so the
		// activity line can place it with that step's calls; a streaming
		// record cuts its newest part at a safe Markdown boundary.
		texts := record.segmentTexts()
		if record.active && len(texts) > 0 {
			last := texts[len(texts)-1]
			texts[len(texts)-1] = last[:markdown.SafeVisibleLen(last)]
		}
		previous := record.previewLines
		record.previewLines, record.previewSegments = nil, make([]int, len(texts))
		for i, text := range texts {
			rows := markdown.RenderMutedRows(text, contentWidth, record.previewCode)
			record.previewLines = append(record.previewLines, rows...)
			record.previewSegments[i] = len(rows)
		}
		if !slices.Equal(previous, record.previewLines) {
			m.visual.invalidate()
		}
		record.previewWidth = contentWidth
		record.previewVersion = record.tailVersion
		record.previewActive = record.active
	}
}

func (m *replModel) reasoningRecordText(record *reasoningRecord, width int) string {
	open := record.anyOpen()
	glyph := "▸"
	if open {
		glyph = "▾"
	}

	elapsed := record.elapsed
	if record.active && !m.thinkingSegmentStart.IsZero() && record.id == m.turnReasoningID {
		elapsed += time.Since(m.thinkingSegmentStart)
	}
	label := reasoningDisclosureLabel(record.active, record.unsaved, elapsed)
	header := activityRowHeader(glyph, label)
	if !open {
		return header
	}

	contentWidth := width - rw.StringWidth(reasoningBlockIndent)
	if contentWidth < 2 {
		return header
	}
	// The entry holds every painted row; the activity line shows the rows
	// of each part that is open.
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
		b.WriteString(line)
	}
	return b.String()
}

func reasoningDisclosureLabel(active, unsaved bool, elapsed time.Duration) string {
	// A paused segment (tool phase mid-run) reads "Thought" like a settled
	// one; the header styling, not the label, distinguishes live from done.
	// The label itself never flips to "thinking…": the elapsed timer keeps
	// ticking up in place, which already signals live activity.
	label := "thought"
	if elapsed > 0 {
		label += " " + formatElapsed(elapsed)
	}
	if unsaved {
		label += " · not saved"
	}
	return label
}

// reasoningTailLines renders the retained Markdown with the response layout,
// then returns its newest physical rows. A limit under one keeps every row.
// Rows contain styled markup; plain-text surfaces flatten styles, not whitespace.
func reasoningTailLines(text string, width, limit int) []string {
	lines := markdown.RenderMutedRows(text, width, nil)
	if limit > 0 && len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	return lines
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
