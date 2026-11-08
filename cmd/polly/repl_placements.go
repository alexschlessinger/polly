package main

import (
	"slices"
	"strings"
)

// Placements: mapping visible disclosure rows to screen cells and toggling them.

// Two controls disclose a turn's activity. A click opens or folds one
// activity line. Ctrl-O sets disclosureDepth, what the view's lines list:
// abridged, each step's thought as its newest rows of text and the line's
// newest calls; full, every thought row and every call; calls alone, no
// thinking; or collapsed, every line folded. Every line is open at the
// other depths unless folded by hand; entering or leaving collapsed folds
// or opens them all, and the other depth changes rewrite the thought
// defaults alone. A click on a thought overrides one part. A call's output
// and diff open only by a click on the call, at any depth. A line opened by
// hand in a collapsed view lists as abridged.
type disclosureDepth uint8

const (
	depthAbridged disclosureDepth = iota
	depthFull
	depthCalls
	depthCollapsed
)

// label names a depth in the status bar.
func (depth disclosureDepth) label() string {
	switch depth {
	case depthFull:
		return "full"
	case depthCalls:
		return "calls only"
	case depthCollapsed:
		return "collapsed"
	}
	return "abridged"
}

// next is Ctrl-O's cycle: abridged, full, calls alone, collapsed, abridged
// again.
func (depth disclosureDepth) next() disclosureDepth {
	switch depth {
	case depthAbridged:
		return depthFull
	case depthFull:
		return depthCalls
	case depthCalls:
		return depthCollapsed
	}
	return depthAbridged
}

// disclosureKinds lists the clickable activity kinds: the activity line is
// one control, so there is one.
var disclosureKinds = [...]activityKind{activityAll}

// placeDisclosures records every kind's hit-testable controls for the frame
// just laid out against viewport.
func (m *replModel) placeDisclosures(v transcriptViewport) {
	for _, kind := range disclosureKinds {
		m.disclosurePlacements[kind] = m.visibleDisclosurePlacements(v, kind)
	}
}

// visibleDisclosurePlacements projects one kind of activity control into
// absolute screen cells for mouse hit-testing.
func (m *replModel) visibleDisclosurePlacements(v transcriptViewport, kind activityKind) []disclosurePlacement {
	// Only the block's activity controls are click targets. Truncation may
	// leave no fully visible control; the header never stands in for one.
	var placements []disclosurePlacement
	rowOffset := 0
	for _, block := range m.visual.blocks {
		row := rowOffset
		rowOffset += len(block.rows)
		if !block.isActivity() || len(block.rows) == 0 {
			continue
		}
		// The keyboard names a control by its record: a tool disclosure
		// when the line has one, else its reasoning record, negated so the
		// two spaces never collide.
		recordID := int64(0)
		if len(block.toolDisclosureIDs) > 0 {
			recordID = block.toolDisclosureIDs[0]
		} else if len(block.reasoningIDs) > 0 {
			recordID = -block.reasoningIDs[0]
		}
		for _, field := range block.activityFields {
			if field.kind != kind || field.X >= v.width || !v.contains(row+field.Y) {
				continue
			}
			placements = append(placements, disclosurePlacement{
				recordID:   recordID,
				recordIDs:  append([]int64(nil), block.toolDisclosureIDs...),
				thoughtIDs: append([]int64(nil), block.reasoningIDs...),
				X:          field.X,
				Y:          v.screenY(row + field.Y),
				Cols:       min(field.Cols, v.width-field.X),
			})
		}
	}
	return placements
}

// toggleDisclosureAt toggles the activity control under screen cell (x, y),
// if any, laid out at width. Returns whether a control was hit.
func (m *replModel) toggleDisclosureAt(x, y, width int) bool {
	for _, kind := range disclosureKinds {
		for _, p := range m.disclosurePlacements[kind] {
			if y != p.Y || x < p.X || x >= p.X+p.Cols {
				continue
			}
			if kind == activityAll {
				return m.toggleActivityLine(p.thoughtIDs, p.recordIDs, width)
			}
			ids := p.recordIDs
			if len(ids) == 0 {
				ids = []int64{p.recordID}
			}
			return m.toggleDisclosureGroup(kind, ids, width)
		}
	}
	return false
}

// toggleActivityLine opens or folds everything one activity line carries:
// its reasoning, tool rows, agents and images, as one control. The line
// points down whenever any of them is open, so its first click folds the
// whole line; only an entirely folded line opens. Row outputs keep their
// own state. width is as for toggleDisclosureGroup.
func (m *replModel) toggleActivityLine(thoughtIDs, toolIDs []int64, width int) bool {
	anyExpanded, found := false, false
	for _, id := range thoughtIDs {
		if expanded, ok := disclosureOps[activityThought].expanded(m, id); ok {
			anyExpanded, found = anyExpanded || expanded, true
		}
	}
	for _, id := range toolIDs {
		if expanded, ok := disclosureOps[activityTools].expanded(m, id); ok {
			anyExpanded, found = anyExpanded || expanded, true
		}
	}
	if !found {
		return false
	}
	expand := !anyExpanded
	m.applyDisclosureToggle(width, matchActivityGroup(thoughtIDs, toolIDs), func(layoutWidth int, held bool) {
		disclosureOps[activityTools].apply(m, toolIDs, expand, layoutWidth, held)
		disclosureOps[activityThought].apply(m, thoughtIDs, expand, layoutWidth, held)
	})
	return true
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

// disclosureKindOps is how one activity kind reads and writes its expansion
// state. expanded reports a record's state and whether it takes part in this
// kind at all; apply writes the new state to every participating record and
// re-lays-out what changed.
type disclosureKindOps struct {
	expanded func(m *replModel, id int64) (expanded, ok bool)
	apply    func(m *replModel, ids []int64, expand bool, width int, held bool)
	match    func(ids []int64) func(*transcriptVisualBlock) bool
}

var disclosureOps = map[activityKind]disclosureKindOps{
	activityThought: {
		expanded: func(m *replModel, id int64) (bool, bool) {
			record := m.reasoningRecords.get(id)
			return record != nil && record.expanded, record != nil
		},
		apply: func(m *replModel, ids []int64, expand bool, width int, held bool) {
			// Apply the new state to the whole group before refreshing any
			// record: each refresh re-lays-out the merged activity row, so
			// refreshing mid-loop would render intermediate frames from a
			// half-toggled group.
			var changed []*reasoningRecord
			for _, id := range ids {
				record := m.reasoningRecords.get(id)
				if record.expanded != expand {
					record.expanded = expand
					changed = append(changed, record)
				}
			}
			for _, record := range changed {
				m.refreshReasoningRecordWithAnchor(record, width, !held)
			}
		},
		match: matchReasoningGroup,
	},
	activityTools: {
		expanded: func(m *replModel, id int64) (bool, bool) {
			record := m.toolDisclosures.get(id)
			return record != nil && record.expanded, record != nil
		},
		apply: func(m *replModel, ids []int64, expand bool, _ int, held bool) {
			for _, id := range ids {
				record := m.toolDisclosures.get(id)
				if record.expanded != expand {
					m.setToolDisclosureExpanded(record, expand, !held)
				}
			}
		},
		match: matchToolGroup,
	},
}

// The agents and images of a line open and fold with its tool records.
func init() {
	disclosureOps[activityAgents] = disclosureOps[activityTools]
	disclosureOps[activityImages] = disclosureOps[activityTools]
}

// toggleDisclosureGroup opens or folds the activity line that the first
// record in ids of the given kind belongs to; a line is one control however
// it is addressed. width is the layout width when known (mouse clicks and
// shortcuts), else 0 for the last renderer width.
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
			thoughtIDs, toolIDs := m.lineRecords(index)
			return m.toggleActivityLine(thoughtIDs, toolIDs, width)
		}
	}
	return false
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
// then runs apply under the anchored mutation every disclosure toggle uses,
// at the width the transcript is laid out with. Caller must hold m.mu.
func (m *replModel) applyDisclosureToggle(width int, match func(*transcriptVisualBlock) bool, apply func(layoutWidth int, held bool)) {
	if width > 0 {
		m.reasoningWidth = width
	}
	layoutWidth := m.disclosureLayoutWidth(width)
	m.mutateAnchored(layoutWidth, match, func(held bool) { apply(layoutWidth, held) })
}

// toggleAllDisclosures is Ctrl-O: it moves the view's depth to the next of
// abridged, full, calls alone and collapsed, and sets every thought's parts
// to the new depth. Entering collapsed folds every line and leaving it
// opens every line; the other changes leave which lines are open as they
// are. Every output stays as it is. width is 0 for the last renderer width.
// Caller holds m.mu.
func (m *replModel) toggleAllDisclosures(width int) bool {
	if !m.hasActivityRecords() {
		return false
	}
	previous := m.depth
	m.depth = m.depth.next()
	// A depth change resizes every open activity block at once, so anchor
	// across all of them.
	m.applyDisclosureToggle(width, (*transcriptVisualBlock).isActivity, func(layoutWidth int, held bool) {
		if previous == depthCollapsed || m.depth == depthCollapsed {
			m.applyOpening(layoutWidth, held)
		}
		m.openCuts = nil
		m.applyDepth(layoutWidth, held)
	})
	return true
}

// applyDepth sets every thought part to the view's depth: whole at full,
// abridged otherwise. Caller holds m.mu, inside the anchored mutation when a
// frame is on screen.
func (m *replModel) applyDepth(layoutWidth int, held bool) {
	full := m.depth == depthFull
	for _, record := range m.reasoningRecords.all() {
		record.ensureSegments()
		for i := range record.segments {
			record.segments[i].full = full
		}
	}
	m.visual.invalidate()
}

// applyOpening puts every line in the state the view's depth opens by
// itself: open, or folded at collapsed. Caller holds m.mu.
func (m *replModel) applyOpening(layoutWidth int, held bool) {
	open := m.depth != depthCollapsed
	var toolIDs, thoughtIDs []int64
	for id := range m.toolDisclosures.all() {
		toolIDs = append(toolIDs, id)
	}
	for id := range m.reasoningRecords.all() {
		thoughtIDs = append(thoughtIDs, id)
	}
	disclosureOps[activityTools].apply(m, toolIDs, open, layoutWidth, held)
	disclosureOps[activityThought].apply(m, thoughtIDs, open, layoutWidth, held)
	m.visual.invalidate()
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

// newRecordOpen is the state a record created now starts in: the state of
// the line it joins, open or folded by hand, and for a line of its own what
// the view's depth opens by itself. Hydration decides for the whole
// transcript once it is complete.
func (m *replModel) newRecordOpen() bool {
	if m.hydrating {
		return false
	}
	// A record lands at the end of the transcript and joins the line there
	// when the entry before it is this turn's activity; prose between, or
	// another turn's activity, starts a new line, so one line's hand choice
	// never reaches the next.
	for i := len(m.transcript) - 1; i >= 0; i-- {
		if m.transcript[i].activityTurn != m.activityTurn {
			break
		}
		if id := m.reasoningRecords.idAt(i); id != 0 {
			return m.reasoningRecords.get(id).expanded
		}
		if id := m.toolDisclosures.idAt(i); id != 0 {
			return m.toolDisclosures.get(id).expanded
		}
		if !transparentLineEntry(m, i) {
			break
		}
	}
	return m.depth != depthCollapsed
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

// openHydratedLines applies the view's depth to a freshly hydrated
// transcript: every line open, or folded at collapsed. No frame is on
// screen yet, so nothing needs anchoring.
func (m *replModel) openHydratedLines() {
	m.applyOpening(max(80, m.reasoningWidth), true)
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
