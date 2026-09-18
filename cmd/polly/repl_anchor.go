package main

import (
	"strings"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
)

// Held-viewport re-anchoring: block matching and entry measurement.

// displaySpan is one flattened display block's place in anchor space: its
// key, first display row and row count.
type displaySpan struct {
	key          string
	start, count int
}

// displayRecordSpans locates every flattened display block satisfying match,
// in display order. The viewport anchor indexes display rows, and adjacent
// activity entries merge into one display block — so raw per-entry offsets
// drift from anchor space and cannot re-anchor a held viewport. Forces the
// layout for width. Caller must hold m.mu.
func (m *replModel) displayRecordSpans(width int, match func(*transcriptVisualBlock) bool) []displaySpan {
	m.transcriptRows(width)
	var spans []displaySpan
	start := 0
	for i := range m.visual.blocks {
		block := &m.visual.blocks[i]
		if match(block) {
			spans = append(spans, displaySpan{key: block.key, start: start, count: len(block.rows)})
		}
		start += len(block.rows)
	}
	return spans
}

// mutateAnchored applies mutate to the transcript while keeping a held
// viewport steady: every display block satisfying match is measured before
// and after, and the scroll anchor shifts by their height changes. mutate
// receives whether a block was found under a held viewport, so nested
// refreshes can skip their own re-anchoring. When the viewport follows the
// bottom nothing is measured. Caller must hold m.mu.
func (m *replModel) mutateAnchored(width int, match func(*transcriptVisualBlock) bool, mutate func(held bool)) {
	if m.followBottom {
		mutate(false)
		return
	}
	before := m.displayRecordSpans(width, match)
	mutate(len(before) > 0)
	if len(before) > 0 {
		m.anchorForResizedBlocks(before, m.displayRecordSpans(width, match))
	}
}

// matchToolGroup and matchReasoningGroup match the merged activity block
// that owns every record in ids. Raw transcript entries over-count their
// independent headers and reasoning previews after layout combines them into
// one block, so re-anchoring matches the block, not the entry.
func matchToolGroup(ids []int64) func(*transcriptVisualBlock) bool {
	return func(block *transcriptVisualBlock) bool {
		return activityGroupContains(block.toolDisclosureIDs, ids)
	}
}

func matchReasoningGroup(ids []int64) func(*transcriptVisualBlock) bool {
	return func(block *transcriptVisualBlock) bool {
		return activityGroupContains(block.reasoningIDs, ids)
	}
}

func matchTurnTrailerBlock(recordID int64) func(*transcriptVisualBlock) bool {
	return func(block *transcriptVisualBlock) bool {
		return block.turnTrailerID == recordID
	}
}

func (m *replModel) entryVisualLineCount(index, width int) int {
	if index < 0 || index >= len(m.transcript) {
		return 0
	}
	if m.quiet && (m.reasoningRecords.idAt(index) != 0 || m.toolDisclosures.idAt(index) != 0) {
		return 0
	}
	if width < 1 {
		width = 80
	}
	followed := index < len(m.transcript)-1
	blankRow := m.userPromptBlankRow(index)
	count := 0
	if m.collapseInitialPrompt && m.transcript[index].initialPrompt {
		// The prompt row stands in for the collapsed entry and precedes it
		// when expanded, so it is part of this entry's height either way.
		rows, _ := transcriptBlockRowsWithImages(initialPromptRow(m.initialPromptExpanded), followed || m.initialPromptExpanded, width, nil, false, 0, 0)
		count += len(rows)
		if !m.initialPromptExpanded {
			return count
		}
	}
	entry := m.transcript[index].text
	if index == m.currentAssistant {
		entry = strings.TrimRight(entry, "\r\n")
		if entry == "" {
			return count
		}
		entry += m.streamCursorFrame
	}
	if blankRow {
		// Mirrors the display block: a prompt's blank row is part of its own
		// text, so this entry measures one row taller than its prompt lines.
		entry += "\n"
	}
	rows, _ := transcriptBlockRowsWithImages(
		entry, followed, width, m.transcript[index].images,
		m.nativeImages && width >= style.MinimumThumbnailCols,
		m.imageCellWidth, m.imageCellHeight,
	)
	return count + len(rows)
}

func (m *replModel) entryVisualStart(index, width int) int {
	start := m.mastheadRowCount(width)
	for i := 0; i < index; i++ {
		start += m.entryVisualLineCount(i, width)
	}
	return start
}

// anchorForResizedEntry keeps the viewport steady when the entry at visual
// offset start changes height. An entry wholly above the anchor shifts the
// anchor by the height delta; an entry containing the anchor keeps the
// anchor's relative position inside the entry instead of snapping to its top.
func (m *replModel) anchorForResizedEntry(start, oldCount, newCount int) {
	if m.followBottom {
		return
	}
	m.scrollAnchor = max(0, m.scrollAnchor+anchorShiftForResizedEntry(m.scrollAnchor, start, oldCount, newCount))
}

// anchorForResizedBlocks keeps the viewport steady when several display
// blocks change height at once. before and after are the same blocks, paired
// by key, measured around the mutation. Every span in before is in
// pre-mutation rows, so each block's shift is judged against the anchor's
// position in those rows and the shifts add up instead of compounding.
func (m *replModel) anchorForResizedBlocks(before, after []displaySpan) {
	counts := make(map[string]int, len(after))
	for _, span := range after {
		counts[span.key] = span.count
	}
	anchor := m.scrollAnchor
	for _, span := range before {
		if count, ok := counts[span.key]; ok {
			m.scrollAnchor += anchorShiftForResizedEntry(anchor, span.start, span.count, count)
		}
	}
	m.scrollAnchor = max(0, m.scrollAnchor)
}

// anchorShiftForResizedEntry is how far anchor moves when the entry at start
// changes from oldCount to newCount rows: the whole delta for an entry wholly
// above it, a proportional move for the entry containing it so the viewport
// keeps its place inside rather than snapping to the top, and nothing for an
// entry below it.
func anchorShiftForResizedEntry(anchor, start, oldCount, newCount int) int {
	switch {
	case oldCount == newCount:
		return 0
	case start+oldCount <= anchor:
		return newCount - oldCount
	case start < anchor:
		rel := anchor - start
		if oldCount > 0 {
			rel = rel * newCount / oldCount
		}
		return start + rel - anchor
	}
	return 0
}
