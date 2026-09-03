package main

import (
	"slices"
)

// Placements: mapping visible disclosure rows to screen cells and toggling them.

func (m *replModel) visibleReasoningPlacements(v transcriptViewport) []disclosurePlacement {
	return m.visibleDisclosurePlacements(v, turnDockOverlayThought)
}

func (m *replModel) visibleToolDisclosurePlacements(v transcriptViewport) []disclosurePlacement {
	return m.visibleDisclosurePlacements(v, turnDockOverlayTools)
}

func (m *replModel) visibleImageDisclosurePlacements(v transcriptViewport) []disclosurePlacement {
	return m.visibleDisclosurePlacements(v, turnDockOverlayImages)
}

func (m *replModel) visibleTurnTrailerPlacements(v transcriptViewport) []turnTrailerPlacement {
	var placements []turnTrailerPlacement
	rowOffset := 0
	for _, block := range m.visual.blocks {
		if block.turnTrailerID != 0 && len(block.rows) > 0 {
			row := rowOffset
			if v.contains(row) {
				if record := m.turnTrailers[block.turnTrailerID]; record != nil {
					for _, field := range record.fields {
						if field.X >= v.width {
							continue
						}
						field.Y = v.screenY(row)
						field.Cols = min(field.Cols, v.width-field.X)
						placements = append(placements, turnTrailerPlacement{
							recordID: record.id, turnDockPlacement: field,
						})
					}
				}
			}
		}
		rowOffset += len(block.rows)
	}
	return placements
}

// visibleDisclosurePlacements projects one kind of activity control into
// absolute screen cells for mouse hit-testing.
func (m *replModel) visibleDisclosurePlacements(v transcriptViewport, overlay turnDockOverlay) []disclosurePlacement {
	// Only the block's activity controls are click targets. Truncation may
	// leave no fully visible control; the header never stands in for one.
	var placements []disclosurePlacement
	rowOffset := 0
	for _, block := range m.visual.blocks {
		recordIDs := block.reasoningIDs
		if overlay == turnDockOverlayTools || overlay == turnDockOverlayImages {
			recordIDs = block.toolDisclosureIDs
		}
		row := rowOffset
		rowOffset += len(block.rows)
		if len(recordIDs) == 0 || len(block.rows) == 0 || !v.contains(row) {
			continue
		}
		for _, field := range block.activityFields {
			if field.overlay != overlay || field.X >= v.width {
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

func (m *replModel) toggleReasoningAt(x, y, width int) bool {
	for _, placement := range m.reasoningPlacements {
		if y != placement.Y || x < placement.X || x >= placement.X+placement.Cols {
			continue
		}
		if len(placement.recordIDs) > 1 {
			return m.toggleReasoningGroup(placement.recordIDs, width)
		}
		return m.toggleReasoning(placement.recordID, width)
	}
	return false
}

func (m *replModel) toggleToolDisclosureAt(x, y int) bool {
	for _, placement := range m.toolDisclosurePlacements {
		if y != placement.Y || x < placement.X || x >= placement.X+placement.Cols {
			continue
		}
		if len(placement.recordIDs) > 1 {
			return m.toggleToolDisclosureGroup(placement.recordIDs)
		}
		return m.toggleToolDisclosure(placement.recordID)
	}
	return false
}

func (m *replModel) toggleImageDisclosureAt(x, y int) bool {
	for _, placement := range m.imageDisclosurePlacements {
		if y != placement.Y || x < placement.X || x >= placement.X+placement.Cols {
			continue
		}
		ids := placement.recordIDs
		if len(ids) == 0 && placement.recordID != 0 {
			ids = []int64{placement.recordID}
		}
		return m.toggleImageDisclosureGroup(ids)
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

func (m *replModel) toggleReasoningGroup(ids []int64, width int) bool {
	anyExpanded, found := false, false
	validIDs := make([]int64, 0, len(ids))
	for _, id := range ids {
		if record := m.reasoningRecords[id]; record != nil {
			found = true
			anyExpanded = anyExpanded || record.expanded
			validIDs = append(validIDs, id)
		}
	}
	if !found {
		return false
	}
	if width > 0 {
		m.reasoningWidth = width
	}
	layoutWidth := m.disclosureLayoutWidth(width)
	// The group header points down whenever any member is expanded. Its first
	// click therefore collapses the whole group; only an entirely closed group
	// expands on click.
	expand := !anyExpanded
	m.mutateAnchored(layoutWidth, matchReasoningGroup(validIDs), func(held bool) {
		// Apply the new state to the whole group before refreshing any record:
		// each refresh re-lays-out the merged activity row, so refreshing
		// mid-loop would render intermediate frames from a half-toggled group.
		var changed []*reasoningRecord
		for _, id := range ids {
			if record := m.reasoningRecords[id]; record != nil && record.expanded != expand {
				record.expanded = expand
				changed = append(changed, record)
			}
		}
		for _, record := range changed {
			m.refreshReasoningRecordWithAnchor(record, layoutWidth, !held)
		}
	})
	return true
}

func (m *replModel) toggleToolDisclosureGroup(ids []int64) bool {
	anyExpanded, found := false, false
	validIDs := make([]int64, 0, len(ids))
	for _, id := range ids {
		if record := m.toolDisclosures[id]; record != nil {
			found = true
			anyExpanded = anyExpanded || record.expanded
			validIDs = append(validIDs, id)
		}
	}
	if !found {
		return false
	}
	expand := !anyExpanded
	m.mutateAnchored(m.disclosureLayoutWidth(0), matchToolGroup(validIDs), func(held bool) {
		// Apply-then-refresh: see toggleReasoningGroup.
		var changed []*toolDisclosureRecord
		for _, id := range ids {
			if record := m.toolDisclosures[id]; record != nil && record.expanded != expand {
				record.expanded = expand
				changed = append(changed, record)
			}
		}
		for _, record := range changed {
			m.refreshToolDisclosureWithAnchor(record, !held)
		}
	})
	return true
}

func (m *replModel) toggleImageDisclosureGroup(ids []int64) bool {
	anyExpanded, found := false, false
	validIDs := make([]int64, 0, len(ids))
	for _, id := range ids {
		record := m.toolDisclosures[id]
		if record == nil || len(m.toolInspectionImages([]int64{id})) == 0 {
			continue
		}
		found = true
		anyExpanded = anyExpanded || record.imagesExpanded
		validIDs = append(validIDs, id)
	}
	if !found {
		return false
	}
	expand := !anyExpanded
	m.mutateAnchored(m.disclosureLayoutWidth(0), matchToolGroup(validIDs), func(bool) {
		for _, id := range validIDs {
			m.toolDisclosures[id].imagesExpanded = expand
		}
		m.visual.invalidate()
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
			r.model.appendNoticeLine("open failed: " + err.Error())
		}
		return
	}
}
