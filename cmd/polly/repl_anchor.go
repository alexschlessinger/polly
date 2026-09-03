package main

import (
	"strings"
)

// Held-viewport re-anchoring: block matching and entry measurement.

// displayRecordSpan locates the flattened display block satisfying match and
// returns its first display row and row count. The viewport anchor indexes
// display rows, and adjacent activity entries merge into one display block —
// so raw per-entry offsets drift from anchor space and cannot re-anchor a
// held viewport. Forces the layout for width. Caller must hold m.mu.
func (m *replModel) displayRecordSpan(width int, match func(*transcriptVisualBlock) bool) (int, int, bool) {
	m.transcriptRows(width)
	start := 0
	for i := range m.visual.blocks {
		block := &m.visual.blocks[i]
		if match(block) {
			return start, len(block.rows), true
		}
		start += len(block.rows)
	}
	return 0, 0, false
}

// mutateAnchored applies mutate to the transcript while keeping a held
// viewport steady: the display block satisfying match is measured before and
// after, and the scroll anchor shifts by the height change. mutate receives
// whether the block was found under a held viewport, so nested refreshes can
// skip their own re-anchoring. When the viewport follows the bottom nothing
// is measured. Caller must hold m.mu.
func (m *replModel) mutateAnchored(width int, match func(*transcriptVisualBlock) bool, mutate func(held bool)) {
	if m.followBottom {
		mutate(false)
		return
	}
	oldStart, oldCount, held := m.displayRecordSpan(width, match)
	mutate(held)
	if held {
		if _, newCount, ok := m.displayRecordSpan(width, match); ok {
			m.anchorForResizedEntry(oldStart, oldCount, newCount)
		}
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
	if m.quiet && (m.reasoningAt[index] != 0 || m.toolDisclosureAt[index] != 0) {
		return 0
	}
	if width < 1 {
		width = 80
	}
	entry := m.transcript[index].text
	if index == m.currentAssistant {
		entry = strings.TrimRight(entry, "\r\n")
		if entry == "" {
			return 0
		}
		entry += m.streamCursorFrame
	}
	followed := index < len(m.transcript)-1 || m.slashHints != ""
	rows, _ := transcriptBlockRowsWithImages(
		entry, followed, width, m.transcript[index].images,
		m.nativeImages && width >= minimumImageThumbnailCols,
		m.imageCellWidth, m.imageCellHeight,
	)
	return len(rows)
}

func (m *replModel) entryVisualStart(index, width int) int {
	start := 0
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
	if m.followBottom || oldCount == newCount {
		return
	}
	delta := newCount - oldCount
	switch {
	case start+oldCount <= m.scrollAnchor:
		// Entry is entirely above the anchor: shift by the height change.
		m.scrollAnchor += delta
	case start < m.scrollAnchor:
		// Entry straddles the anchor: preserve the anchor's fractional
		// position within the entry so the viewport does not jump.
		rel := m.scrollAnchor - start
		if oldCount > 0 {
			rel = rel * newCount / oldCount
		}
		m.scrollAnchor = start + rel
	}
	if m.scrollAnchor < 0 {
		m.scrollAnchor = 0
	}
}
