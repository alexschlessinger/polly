package main

import (
	"fmt"
	"strings"
	"time"

	rw "github.com/mattn/go-runewidth"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
)

// Inline activity: reasoning, tool, and image fields laid out within the transcript.

func (m *replModel) inlineReasoningField(ids []int64) (turnDockField, bool) {
	var elapsed time.Duration
	active, unsaved, expanded, found := false, false, false, false
	for _, id := range ids {
		record := m.reasoningRecords.get(id)
		if record == nil {
			continue
		}
		found = true
		elapsed += record.elapsed
		if record.active && !m.thinkingSegmentStart.IsZero() && record.id == m.turnReasoningID {
			elapsed += time.Since(m.thinkingSegmentStart)
		}
		active = active || record.active
		unsaved = unsaved || record.unsaved
		expanded = expanded || record.expanded
	}
	if !found {
		return turnDockField{}, false
	}
	return activityField(reasoningDisclosureLabel(active, unsaved, elapsed), activityThought, expanded), true
}

func (m *replModel) inlineToolField(ids []int64) (turnDockField, bool) {
	total, expanded, found := 0, false, false
	for _, id := range ids {
		if record := m.toolDisclosures.get(id); record != nil {
			found = true
			total += len(ordinaryToolRows(record.rows))
			expanded = expanded || record.expanded
		}
	}
	if !found || total == 0 {
		return turnDockField{}, false
	}
	return activityField(turnToolLabel(total), activityTools, expanded), true
}

func (m *replModel) inlineImageField(ids []int64, expanded bool) (turnDockField, []style.Image, bool) {
	images := m.toolInspectionImages(ids)
	if len(images) == 0 {
		return turnDockField{}, nil, false
	}
	return activityField(turnImageLabel(len(images)), activityImages, expanded), images, true
}

func inlineActivityDetail(text string) string {
	_, detail, ok := strings.Cut(text, "\n")
	if !ok {
		return ""
	}
	return detail
}

// layoutInlineActivityBlocks lays adjacent thought, tool, agent, and image
// disclosures out as one row with one triangle, while each keeps its own
// independently expandable detail beneath it.
func (m *replModel) layoutInlineActivityBlocks(blocks []transcriptDisplayBlock, width int) []transcriptDisplayBlock {
	if width < 1 {
		width = 80
	}
	laidOut := make([]transcriptDisplayBlock, 0, len(blocks))
	for _, block := range blocks {
		if !block.isActivity() {
			laidOut = append(laidOut, block)
			continue
		}
		detail := inlineActivityDetail(block.text)
		if len(block.toolDisclosureIDs) == 1 {
			if record := m.toolDisclosures.get(block.toolDisclosureIDs[0]); record != nil {
				detail = inlineToolDetail(detail, record.displayRows, width, m.toolBaseDir)
			}
		}
		if len(block.reasoningIDs) > 0 {
			block.activityReasoningDetail = detail
		} else {
			block.activityToolDetail = detail
		}

		if n := len(laidOut); n > 0 {
			previous := &laidOut[n-1]
			if previous.turnTrailerID == 0 && previous.isActivity() {
				if len(previous.images) > 0 && len(block.images) > 0 {
					block.activityToolDetail = style.OffsetImageMarkers(block.activityToolDetail, len(previous.images))
				}
				previous.reasoningIDs = append(previous.reasoningIDs, block.reasoningIDs...)
				previous.toolDisclosureIDs = append(previous.toolDisclosureIDs, block.toolDisclosureIDs...)
				if block.activityReasoningDetail != "" {
					if previous.activityReasoningDetail != "" {
						previous.activityReasoningDetail += "\n"
					}
					previous.activityReasoningDetail += block.activityReasoningDetail
				}
				if block.activityToolDetail != "" {
					if previous.activityToolDetail != "" {
						previous.activityToolDetail += "\n"
					}
					previous.activityToolDetail += block.activityToolDetail
				}
				previous.images = append(previous.images, block.images...)
				continue
			}
		}

		laidOut = append(laidOut, block)
	}
	for i := range laidOut {
		if laidOut[i].isActivity() {
			m.layoutInlineActivityBlock(&laidOut[i], width)
		}
	}
	return laidOut
}

func (m *replModel) layoutInlineActivityBlock(block *transcriptDisplayBlock, width int) {
	var fields []turnDockField
	if field, ok := m.inlineReasoningField(block.reasoningIDs); ok {
		fields = append(fields, field)
	}
	if field, ok := m.inlineToolField(block.toolDisclosureIDs); ok {
		fields = append(fields, field)
	}
	agentsExpanded := m.agentsExpanded(block.toolDisclosureIDs)
	if field, ok := m.agentField(block.toolDisclosureIDs, agentsExpanded); ok {
		fields = append(fields, field)
	}
	block.activityImageDetail = ""
	imagesExpanded := m.toolInspectionExpanded(block.toolDisclosureIDs)
	if field, inspectionImages, ok := m.inlineImageField(block.toolDisclosureIDs, imagesExpanded); ok {
		fields = append(fields, field)
		if imagesExpanded {
			remaining := style.MaxImagesPerBlock - len(block.images)
			if remaining > 0 {
				inspectionImages = inspectionImages[:min(len(inspectionImages), remaining)]
				block.activityImageDetail = style.OffsetImageMarkers(
					style.RenderInspectionImages(inspectionImages), len(block.images),
				)
				block.images = append(block.images, inspectionImages...)
			}
		}
	}
	expanded := false
	for _, field := range fields {
		expanded = expanded || field.expanded
	}
	header, placements := renderActivityRow(expanded, fields, width)
	block.text = header
	block.activityReasoningDetail = boundedReasoningDetail(block.activityReasoningDetail, reasoningPreviewLines)
	for _, detail := range []string{block.activityReasoningDetail, block.activityToolDetail} {
		if detail != "" {
			block.text += "\n" + detail
		}
	}
	if agentsExpanded {
		m.appendAgentDetail(block, block.toolDisclosureIDs, width)
	}
	if block.activityImageDetail != "" {
		block.text += "\n" + block.activityImageDetail
	}
	block.activityFields = placements
	// Keep even partially clipped labels paintable without making them clickable.
	block.activityLabels = nil
	x, end := 4, width
	fullWidth := x
	for i, field := range fields {
		if i > 0 {
			fullWidth += 3
		}
		fullWidth += rw.StringWidth(field.raw)
	}
	if fullWidth > width {
		end--
	} // Leave the ellipsis alone.
	for _, field := range fields {
		cols := rw.StringWidth(field.raw)
		paintCols := cols
		if field.kind == activityAgents {
			// Only the leading running count sweeps; outcome tails stay steady.
			leading, _, _ := strings.Cut(field.raw, ",")
			paintCols = rw.StringWidth(leading)
		}
		if visible := min(paintCols, end-x); visible > 0 {
			block.activityLabels = append(block.activityLabels, turnDockPlacement{kind: field.kind, X: x, Cols: visible})
		}
		x += cols + 3
	}
	block.key = fmt.Sprintf("activity:r%v:t%v", block.reasoningIDs, block.toolDisclosureIDs)
}

// Resolve activity from live records, never from cached label text or parent busy state.
func (m *replModel) inlineActivityRunning(kind activityKind, reasoningIDs, toolIDs []int64) bool {
	switch kind {
	case activityThought:
		for _, id := range reasoningIDs {
			if record := m.reasoningRecords.get(id); record != nil && record.active {
				return true
			}
		}
	case activityTools:
		for _, id := range toolIDs {
			if record := m.toolDisclosures.get(id); record != nil {
				for _, row := range ordinaryToolRows(record.rows) {
					if !row.settled {
						return true
					}
				}
			}
		}
	case activityAgents:
		return m.agentCounts(toolIDs).Running > 0
	}
	return false
}
