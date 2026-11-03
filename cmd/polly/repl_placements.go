package main

import (
	"slices"
)

// Placements: mapping visible disclosure rows to screen cells and toggling them.

// The default leaves source/manual choices intact until Ctrl-O selects a level.
// All three explicit levels are sticky for blocks that arrive later.
type disclosureMode uint8

const (
	disclosuresDefault disclosureMode = iota
	disclosuresFolded
	disclosuresOpen
	disclosuresDetailed
)

func (mode disclosureMode) next() disclosureMode {
	switch mode {
	case disclosuresOpen:
		return disclosuresDetailed
	case disclosuresDetailed:
		return disclosuresFolded
	default:
		return disclosuresOpen
	}
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
	for _, kind := range []activityKind{activityTools, activityAgents, activityImages} {
		for _, id := range toolIDs {
			if expanded, ok := disclosureOps[kind].expanded(m, id); ok {
				anyExpanded, found = anyExpanded || expanded, true
			}
		}
	}
	if !found {
		return false
	}
	expand := !anyExpanded
	m.applyDisclosureToggle(width, matchActivityGroup(thoughtIDs, toolIDs), func(layoutWidth int, held bool) {
		disclosureOps[activityTools].apply(m, toolIDs, expand, layoutWidth, held)
		disclosureOps[activityThought].apply(m, thoughtIDs, expand, layoutWidth, held)
		disclosureOps[activityAgents].apply(m, toolIDs, expand, layoutWidth, held)
		disclosureOps[activityImages].apply(m, toolIDs, expand, layoutWidth, held)
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
				if record := m.reasoningRecords.get(id); record.expanded != expand {
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
			var changed []*toolDisclosureRecord
			for _, id := range ids {
				if record := m.toolDisclosures.get(id); record.expanded != expand {
					record.expanded = expand
					changed = append(changed, record)
				}
			}
			for _, record := range changed {
				m.refreshToolDisclosureWithAnchor(record, !held)
			}
		},
		match: matchToolGroup,
	},
	activityAgents: {
		expanded: func(m *replModel, id int64) (bool, bool) {
			record := m.toolDisclosures.get(id)
			return record != nil && record.agentsExpanded, record != nil && m.agentCounts([]int64{id}).Total > 0
		},
		apply: func(m *replModel, ids []int64, expand bool, _ int, _ bool) {
			for _, id := range ids {
				m.toolDisclosures.get(id).agentsExpanded = expand
			}
			m.visual.invalidate()
		},
		match: matchToolGroup,
	},
	activityImages: {
		expanded: func(m *replModel, id int64) (bool, bool) {
			record := m.toolDisclosures.get(id)
			return record != nil && record.imagesExpanded, record != nil && len(m.toolInspectionImages([]int64{id})) > 0
		},
		apply: func(m *replModel, ids []int64, expand bool, _ int, _ bool) {
			for _, id := range ids {
				m.toolDisclosures.get(id).imagesExpanded = expand
			}
			m.visual.invalidate()
		},
		match: matchToolGroup,
	},
}

// toggleDisclosureGroup expands or collapses the records in ids that take
// part in kind, as one control. The group header points down whenever any
// member is expanded, so its first click collapses the whole group; only an
// entirely closed group expands on click. width is the layout width when
// known (mouse clicks and shortcuts), else 0 for the last renderer width.
func (m *replModel) toggleDisclosureGroup(kind activityKind, ids []int64, width int) bool {
	ops := disclosureOps[kind]
	anyExpanded := false
	valid := make([]int64, 0, len(ids))
	for _, id := range ids {
		if expanded, ok := ops.expanded(m, id); ok {
			anyExpanded = anyExpanded || expanded
			valid = append(valid, id)
		}
	}
	if len(valid) == 0 {
		return false
	}
	expand := !anyExpanded
	m.applyDisclosureToggle(width, ops.match(valid), func(layoutWidth int, held bool) {
		ops.apply(m, valid, expand, layoutWidth, held)
	})
	return true
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

// toggleAllDisclosures cycles folded → opened → opened with details. Opened
// shows reasoning, tool/agent rows and media; details also shows outputs
// and diffs. Manual choices do not change the cycle, and new blocks inherit its
// selected level. width is 0 for the last renderer width. Caller holds m.mu.
func (m *replModel) toggleAllDisclosures(width int) bool {
	var thoughtIDs, toolIDs []int64
	for id := range m.reasoningRecords.all() {
		thoughtIDs = append(thoughtIDs, id)
	}
	for id := range m.toolDisclosures.all() {
		toolIDs = append(toolIDs, id)
	}
	if len(thoughtIDs)+len(toolIDs) == 0 && !m.collapseInitialPrompt {
		return false
	}
	m.disclosures = m.disclosures.next()
	expand, details := m.disclosures >= disclosuresOpen, m.disclosures == disclosuresDetailed
	// The prompt header and its optional body are separate visual blocks.
	// Anchor their combined entry before measuring the activity blocks below.
	if m.collapseInitialPrompt && m.initialPromptExpanded != expand && !m.followBottom {
		layoutWidth := m.disclosureLayoutWidth(width)
		for index, entry := range m.transcript {
			if entry.initialPrompt {
				start, before := m.entryVisualStart(index, layoutWidth), m.entryVisualLineCount(index, layoutWidth)
				m.setInitialPromptExpanded(expand)
				m.anchorForResizedEntry(start, before, m.entryVisualLineCount(index, layoutWidth))
				break
			}
		}
	}
	m.setInitialPromptExpanded(expand)
	// A whole-view toggle resizes every activity block at once, including
	// outputs whose enclosing group stays open, so anchor across all of them.
	m.applyDisclosureToggle(width, (*transcriptVisualBlock).isActivity, func(layoutWidth int, held bool) {
		for _, id := range toolIDs {
			record := m.toolDisclosures.get(id)
			for i := range record.rows {
				row := &record.rows[i]
				row.outputExpanded = details && !row.isAgent()
			}
			record.expanded = expand
			m.refreshToolDisclosureWithAnchor(record, !held)
		}
		disclosureOps[activityThought].apply(m, thoughtIDs, expand, layoutWidth, held)
		disclosureOps[activityAgents].apply(m, toolIDs, expand, layoutWidth, held)
		disclosureOps[activityImages].apply(m, toolIDs, expand, layoutWidth, held)
	})
	return true
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
