package main

import (
	"slices"
)

// Placements: mapping visible disclosure rows to screen cells and toggling them.

// disclosureKinds lists the clickable activity kinds in hit-test priority
// order: the same cell is offered to each in turn.
var disclosureKinds = [...]activityKind{activityThought, activityTools, activityAgents, activityImages}

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
		recordIDs := block.reasoningIDs
		if kind != activityThought {
			recordIDs = block.toolDisclosureIDs
		}
		row := rowOffset
		rowOffset += len(block.rows)
		if len(recordIDs) == 0 || len(block.rows) == 0 || !v.contains(row) {
			continue
		}
		for _, field := range block.activityFields {
			if field.kind != kind || field.X >= v.width {
				continue
			}
			placements = append(placements, disclosurePlacement{
				recordID:  recordIDs[0],
				recordIDs: append([]int64(nil), recordIDs...),
				X:         field.X,
				Y:         v.screenY(row),
				Cols:      min(field.Cols, v.width-field.X),
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
			ids := p.recordIDs
			if len(ids) == 0 {
				ids = []int64{p.recordID}
			}
			return m.toggleDisclosureGroup(kind, ids, width)
		}
	}
	return false
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
	// The cue lights the control that was clicked, which the placement keys
	// by the block's first record whether or not it takes part in kind.
	m.noteDisclosure(kind, ids[0])
	if width > 0 {
		m.reasoningWidth = width
	}
	layoutWidth := m.disclosureLayoutWidth(width)
	expand := !anyExpanded
	m.mutateAnchored(layoutWidth, ops.match(valid), func(held bool) {
		ops.apply(m, valid, expand, layoutWidth, held)
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
