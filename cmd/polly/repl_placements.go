package main

import (
	"image"
	"slices"
	"strings"
)

// Placements: mapping visible fold rows to screen cells and toggling them.

// A transcript has two bulk states. Collapsed, the default, shows each
// chunk's header; expanded shows the calls and thought rows behind every
// header. Neither opens a thought's text, an output or a diff: those open
// only by a click on their own row, and stay as the reader left them
// through every fold. A click on a header folds or opens that chunk alone,
// so several can be open at once; Ctrl-O puts every chunk in one state.
type disclosureDepth uint8

// The zero value is the default: a view nobody has pressed Ctrl-O in shows
// headers.
const (
	depthCollapsed disclosureDepth = iota
	depthExpanded
)

// label names a state in the status bar.
func (depth disclosureDepth) label() string {
	if depth == depthExpanded {
		return "expanded"
	}
	return "collapsed"
}

func (depth disclosureDepth) other() disclosureDepth {
	if depth == depthExpanded {
		return depthCollapsed
	}
	return depthExpanded
}

// Chunk headers share one placement list in reading order.
var disclosureKinds = [...]activityKind{activityAll}

// chunkOpen reports whether the chunk named key shows its rows: as chosen by
// hand since the last Ctrl-O, else as the view's bulk state has it. A chunk
// that begins later takes the bulk state; a call that joins a chunk takes
// that chunk's.
func (m *replModel) chunkOpen(key string) bool {
	if open, ok := m.chunkFolds[key]; ok {
		return open
	}
	return m.depth == depthExpanded
}

// showsRows reports whether a chunk listing one of record's calls is open,
// so that the calls' rows are on screen.
func (m *replModel) showsRows(record *toolDisclosureRecord) bool {
	// With no hand fold against the bulk state, every chunk is in it.
	expanded, uniform := m.depth == depthExpanded, true
	for _, open := range m.chunkFolds {
		if open != expanded {
			uniform = false
			break
		}
	}
	if uniform {
		return expanded
	}
	chunks, _, _ := m.lineChunks(record.transcriptIndex)
	for i := range chunks {
		c := &chunks[i]
		if c.group == chunkThought || !m.chunkOpen(c.key) {
			continue
		}
		for _, p := range c.parts {
			if p.tools == record {
				return true
			}
		}
	}
	return false
}

// rect is the header's click target at pane offset x: its triangle and its
// label.
func (p disclosurePlacement) rect(x int) image.Rectangle {
	return image.Rect(x+p.X, p.Y, x+p.X+p.Cols, p.Y+1)
}

// mark is the part of the header a pointer or the keyboard underlines: the
// label, past the triangle and its space, or the triangle when that is all
// that fit.
func (p disclosurePlacement) mark(x int) image.Rectangle {
	rect := p.rect(x)
	if p.Cols > chunkLabelLead {
		rect.Min.X += chunkLabelLead
	}
	return rect
}

// placeDisclosures records every kind's hit-testable controls for the frame
// just laid out against viewport.
func (m *replModel) placeDisclosures(v transcriptViewport) {
	for _, kind := range disclosureKinds {
		m.disclosurePlacements[kind] = m.visibleDisclosurePlacements(v, kind)
	}
}

// visibleDisclosurePlacements projects the chunk headers on screen into
// absolute screen cells for mouse hit-testing.
func (m *replModel) visibleDisclosurePlacements(v transcriptViewport, kind activityKind) []disclosurePlacement {
	// Only a header is a fold target. Truncation may leave none fully
	// visible; a call row never stands in for one.
	var placements []disclosurePlacement
	rowOffset := 0
	for _, block := range m.visual.blocks {
		row := rowOffset
		rowOffset += len(block.rows)
		if !block.isActivity() || len(block.rows) == 0 {
			continue
		}
		for _, field := range block.activityFields {
			if kind != activityAll || field.chunk < 1 || field.chunk > len(block.chunks) || field.X >= v.width || !v.contains(row+field.Y) {
				continue
			}
			placements = append(placements, disclosurePlacement{
				key:        block.chunks[field.chunk-1].key,
				recordIDs:  block.toolDisclosureIDs,
				thoughtIDs: block.reasoningIDs,
				X:          field.X,
				Y:          v.screenY(row + field.Y),
				Cols:       min(field.Cols, v.width-field.X),
			})
		}
	}
	return placements
}

func (p disclosurePlacement) keyboardKey() string { return "chunk:" + p.key }

// toggleDisclosureAt folds or opens the chunk whose header is under screen
// cell (x, y), laid out at width. Returns whether a header was hit.
func (m *replModel) toggleDisclosureAt(x, y, width int) bool {
	for _, kind := range disclosureKinds {
		for _, p := range m.disclosurePlacements[kind] {
			if y != p.Y || x < p.X || x >= p.X+p.Cols {
				continue
			}
			m.setChunkOpen(p.key, p.thoughtIDs, p.recordIDs, width, !m.chunkOpen(p.key))
			return true
		}
	}
	return false
}

// setChunkOpen records a fold chosen by hand for one chunk of the line that
// carries the records given. Nothing under the header changes with it: a
// thought's text or an output opened earlier is there when the chunk opens
// again, and one left closed stays closed.
func (m *replModel) setChunkOpen(key string, thoughtIDs, toolIDs []int64, width int, open bool) {
	m.applyDisclosureToggle(width, matchActivityGroup(thoughtIDs, toolIDs), func(int, bool) {
		if m.chunkFolds == nil {
			m.chunkFolds = make(map[string]bool)
		}
		m.chunkFolds[key] = open
		m.visual.invalidate()
	})
}

// lineChunks are the chunks of the activity line holding the entry at
// index, with the line's records.
func (m *replModel) lineChunks(index int) (chunks []activityChunk, thoughtIDs, toolIDs []int64) {
	thoughtIDs, toolIDs = m.lineRecords(index)
	return m.activityChunks(thoughtIDs, toolIDs), thoughtIDs, toolIDs
}

func activityGroupContains(blockIDs, ids []int64) bool {
	if len(ids) == 0 {
		return false
	}
	for _, id := range ids {
		if !slices.Contains(blockIDs, id) {
			return false
		}
	}
	return true
}

// lineRecords are the records of the activity line holding the entry at
// index: the run of adjacent activity entries around it, which the layout
// merges into one line. An empty streaming assistant entry, which the
// layout leaves out, does not break the run.
func (m *replModel) lineRecords(index int) (thoughtIDs, toolIDs []int64) {
	if index < 0 || index >= len(m.transcript) {
		return nil, nil
	}
	turn := m.transcript[index].activityTurn
	activity := func(i int) bool {
		return m.reasoningRecords.idAt(i) != 0 || m.toolDisclosures.idAt(i) != 0
	}
	joins := func(i int) bool {
		return m.transcript[i].activityTurn == turn && (activity(i) || transparentLineEntry(m, i))
	}
	start, end := index, index
	for start > 0 && joins(start-1) {
		start--
	}
	for end+1 < len(m.transcript) && joins(end+1) {
		end++
	}
	for i := start; i <= end; i++ {
		if id := m.reasoningRecords.idAt(i); id != 0 {
			thoughtIDs = append(thoughtIDs, id)
		}
		if id := m.toolDisclosures.idAt(i); id != 0 {
			toolIDs = append(toolIDs, id)
		}
	}
	return thoughtIDs, toolIDs
}

// applyDisclosureToggle records width as the last renderer width when known,
// then runs apply under the anchored mutation every fold uses, at the width
// the transcript is laid out with. Caller must hold m.mu.
func (m *replModel) applyDisclosureToggle(width int, match func(*transcriptVisualBlock) bool, apply func(layoutWidth int, held bool)) {
	if width > 0 {
		m.reasoningWidth = width
	}
	layoutWidth := m.disclosureLayoutWidth(width)
	m.mutateAnchored(layoutWidth, match, func(held bool) { apply(layoutWidth, held) })
}

// toggleAllDisclosures is Ctrl-O: it puts every chunk of the view in the
// bulk state opposite the last one set, whatever was folded or opened by
// hand since. The one exception keeps a press from looking dead. If every
// chunk on screen is already in that state, as when the only chunk in view
// was opened by hand, the press applies the other state instead; chunks
// above and below the screen do not count, since a change there moves
// nothing the reader can see. No press opens a thought's text, an output or
// a diff. width is 0 for the last renderer width. Caller holds m.mu.
func (m *replModel) toggleAllDisclosures(width int) bool {
	if !m.hasActivityRecords() {
		return false
	}
	next := m.depth.other()
	if visible := m.disclosurePlacements[activityAll]; len(visible) > 0 {
		moves := false
		for _, p := range visible {
			moves = moves || m.chunkOpen(p.key) != (next == depthExpanded)
		}
		if !moves {
			next = m.depth
		}
	}
	// A bulk change resizes every activity block at once, so anchor across
	// all of them.
	m.applyDisclosureToggle(width, (*transcriptVisualBlock).isActivity, func(int, bool) {
		m.depth, m.chunkFolds = next, nil
		m.visual.invalidate()
	})
	return true
}

// newestTurnStart is the transcript index of the newest prompt; the newest
// activity line is whatever follows it.
func (m *replModel) newestTurnStart() int {
	for i := len(m.transcript) - 1; i >= 0; i-- {
		if strings.HasPrefix(m.transcript[i].text, userGutter()) {
			return i
		}
	}
	return 0
}

func (m *replModel) hasActivityRecords() bool {
	for range m.reasoningRecords.all() {
		return true
	}
	for range m.toolDisclosures.all() {
		return true
	}
	return false
}

// transparentLineEntry reports an entry that neither carries activity nor
// separates it: an empty entry, such as a record's placeholder or a
// streaming assistant entry with nothing in it yet. Assistant prose
// separates lines even before its Markdown is materialized.
func transparentLineEntry(m *replModel, i int) bool {
	entry := &m.transcript[i]
	if m.turnTrailers.idAt(i) != 0 || strings.TrimRight(entry.text, "\r\n") != "" {
		return false
	}
	return !entry.assistant || entry.markdown == "" && entry.markdownSource == ""
}

// openImageAt opens the transcript thumbnail under the given screen cell, if
// any, in the OS image viewer. Placements come from the last rendered frame;
// the embedded splash logo (no backing file) is skipped. Caller must hold m.mu.
func (r *managedREPL) openImageAt(x, y int) {
	if r.openImage == nil {
		return
	}
	for _, p := range r.model.imagePlacements {
		if p.Path == "" || x < p.X || x >= p.X+p.Cols || y < p.Y || y >= p.Y+p.Rows {
			continue
		}
		if err := r.openImage(p.Path); err != nil {
			r.model.appendNoticeLine("Could not open the image · " + err.Error())
		}
		return
	}
}
