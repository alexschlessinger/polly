package main

import (
	"strings"
	"testing"
)

// Test seams for activity folds. A line is one activity block: the chunks of
// adjacent work between prose. The product folds chunk by chunk; many tests
// only need a whole line open or folded to look at what it holds.

// chunkHit returns the on-screen chunk header match accepts.
func chunkHit(t *testing.T, m *replModel, match func(disclosurePlacement) bool) disclosurePlacement {
	t.Helper()
	for _, p := range m.disclosurePlacements[activityAll] {
		if match(p) {
			return p
		}
	}
	t.Fatalf("no matching chunk header in %+v", m.disclosurePlacements[activityAll])
	return disclosurePlacement{}
}

// lineOpen reports whether the line holding entry index shows anything under
// a header: a chunk of calls open, or, on a line of thoughts alone, a
// thought's text.
func (m *replModel) lineOpen(index int) bool {
	chunks, _, _ := m.lineChunks(index)
	calls := false
	for _, c := range chunks {
		if c.group != chunkThought {
			calls = true
			if m.chunkOpen(c.key) {
				return true
			}
		}
	}
	if calls {
		return false
	}
	for _, c := range chunks {
		if c.hasOpenThought() {
			return true
		}
	}
	return false
}

// lineOpenOf is lineOpen for the line a record is on.
func (m *replModel) lineOpenOf(record transcriptAnchored) bool {
	return m.lineOpen(record.anchor().transcriptIndex)
}

// setLineOpen folds or opens, by hand, every chunk of the line holding entry
// index. On a line of thoughts alone it shows or hides their text.
func (m *replModel) setLineOpen(index, width int, open bool) bool {
	chunks, thoughtIDs, toolIDs := m.lineChunks(index)
	if len(chunks) == 0 {
		return false
	}
	calls := false
	for _, c := range chunks {
		if c.group != chunkThought {
			calls = true
			m.setChunkOpen(c.key, thoughtIDs, toolIDs, width, open)
		}
	}
	if !calls {
		for _, c := range chunks {
			for _, p := range c.parts {
				p.thought.ensureSegments()
				p.thought.segments[p.segment].open = open
				m.markReasoningDirty(p.thought)
			}
		}
	}
	m.visual.invalidate()
	return true
}

// setLineOpenOf is setLineOpen for the line a record is on.
func (m *replModel) setLineOpenOf(record transcriptAnchored, open bool) bool {
	return m.setLineOpen(record.anchor().transcriptIndex, 0, open)
}

// toggleLine folds a line with anything open and opens an entirely folded
// one, as one control.
func (m *replModel) toggleLine(index, width int) bool {
	return m.setLineOpen(index, width, !m.lineOpen(index))
}

func (m *replModel) toggleToolDisclosure(recordID int64) bool {
	record := m.toolDisclosures.get(recordID)
	if record == nil || record.transcriptIndex < 0 || record.transcriptIndex >= len(m.transcript) {
		return false
	}
	return m.toggleLine(record.transcriptIndex, 0)
}

func (m *replModel) toggleReasoning(recordID int64, width int) bool {
	record := m.reasoningRecords.get(recordID)
	if record == nil || record.transcriptIndex < 0 || record.transcriptIndex >= len(m.transcript) {
		return false
	}
	return m.toggleLine(record.transcriptIndex, width)
}

// toggleDisclosureGroup toggles the line that the first record in ids of the
// given kind belongs to.
func (m *replModel) toggleDisclosureGroup(kind activityKind, ids []int64, width int) bool {
	for _, id := range ids {
		index := -1
		if kind == activityThought {
			if record := m.reasoningRecords.get(id); record != nil {
				index = record.transcriptIndex
			}
		} else if record := m.toolDisclosures.get(id); record != nil {
			index = record.transcriptIndex
		}
		if index >= 0 {
			return m.toggleLine(index, width)
		}
	}
	return false
}

// openThoughts shows the text of every part of the given thoughts, as a
// click on each row would.
func openThoughts(m *replModel, reasoningIDs ...int64) {
	for _, id := range reasoningIDs {
		record := m.reasoningRecords.get(id)
		record.ensureSegments()
		for i := range record.segments {
			record.segments[i].open = true
		}
		m.markReasoningDirty(record)
	}
	m.visual.invalidate()
}

// openAllThoughts shows every thought's text.
func openAllThoughts(m *replModel) {
	for id := range m.reasoningRecords.all() {
		openThoughts(m, id)
	}
}

// expandLines is the expanded state with the given thoughts' text showing:
// every call row, every thought row and those thoughts' text.
func expandLines(m *replModel, reasoningIDs []int64) {
	m.depth, m.chunkFolds = depthExpanded, nil
	openThoughts(m, reasoningIDs...)
	m.visual.invalidate()
}

// openAllOutputs opens every ordinary call's output by hand, as clicks
// would.
func openAllOutputs(m *replModel) {
	for _, record := range m.toolDisclosures.all() {
		for i := range record.rows {
			if !record.rows[i].isAgent() {
				record.rows[i].outputExpanded = true
			}
		}
		m.refreshToolDisclosure(record)
	}
	m.visual.invalidate()
}

// openAllLines opens every chunk by hand.
func openAllLines(m *replModel) {
	for i := range m.transcript {
		if m.reasoningRecords.idAt(i) != 0 || m.toolDisclosures.idAt(i) != 0 {
			m.setLineOpen(i, 0, true)
		}
	}
}

// foldEveryLine folds every chunk by hand.
func foldEveryLine(m *replModel) {
	for i := range m.transcript {
		if m.reasoningRecords.idAt(i) != 0 || m.toolDisclosures.idAt(i) != 0 {
			m.setLineOpen(i, 0, false)
		}
	}
}

// thoughtLink lays the transcript out at width and returns the row of the
// given thought part as the pointer and the keyboard would reach it.
func thoughtLink(t *testing.T, m *replModel, width int, recordID int64, part int) toolOutputLink {
	t.Helper()
	m.refreshReasoningRecords(width)
	rows := m.transcriptRows(width)
	record := m.reasoningRecords.get(recordID)
	for _, link := range m.visibleToolOutputLinks(fullViewport(len(rows), width), 0) {
		if link.action == thoughtPartAction && link.recordID == recordID && link.key == m.thoughtPartKey(record, part) {
			return link
		}
	}
	t.Fatalf("no row on screen for thought %d part %d", recordID, part)
	return toolOutputLink{}
}

// clickThought shows or hides one thought part's text by its own row.
func clickThought(t *testing.T, m *replModel, width int, recordID int64, part int) {
	t.Helper()
	if !m.toggleThoughtPart(thoughtLink(t, m, width, recordID, part)) {
		t.Fatalf("thought %d part %d did not toggle", recordID, part)
	}
	m.refreshReasoningRecords(width)
}

// activityText is the plain text of the nth activity line at width.
func activityText(t *testing.T, m *replModel, width, n int) string {
	t.Helper()
	blocks := activityBlocks(m, width)
	if n >= len(blocks) {
		t.Fatalf("transcript has %d activity lines, want at least %d", len(blocks), n+1)
	}
	return plainStyledText(blocks[n].text)
}

// shown is the transcript as painted at width, plain, with every fold as it
// stands: what a reader sees.
func shown(m *replModel, width int) string {
	m.renderPendingMarkdown()
	return strings.Join(transcriptRowsText((conversationView{}).Rows(m, width)), "\n")
}
