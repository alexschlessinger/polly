package main

import (
	"fmt"
	"strings"
	"time"
)

// Inline activity: reasoning, tool, and image fields laid out within the transcript.

func (m *replModel) inlineReasoningField(ids []int64) (turnDockField, bool) {
	var elapsed time.Duration
	active, complete, unsaved, expanded, found := false, true, false, false, false
	for _, id := range ids {
		record := m.reasoningRecords[id]
		if record == nil {
			continue
		}
		found = true
		elapsed += record.elapsed
		if record.active && !m.thinkingSegmentStart.IsZero() && record.id == m.turnReasoningID {
			elapsed += time.Since(m.thinkingSegmentStart)
		}
		active = active || record.active
		complete = complete && record.complete
		unsaved = unsaved || record.unsaved
		expanded = expanded || record.expanded
	}
	if !found {
		return turnDockField{}, false
	}
	label := reasoningDisclosureLabel(active, unsaved, elapsed)
	glyph := "▸"
	if expanded {
		glyph = "▾"
	}
	return turnDockField{
		raw: glyph + " " + label, rendered: inlineActivityControl(glyph, label, complete && !active), overlay: turnDockOverlayThought,
	}, true
}

func (m *replModel) inlineToolField(ids []int64) (turnDockField, bool) {
	total, expanded, complete, found := 0, false, true, false
	for _, id := range ids {
		if record := m.toolDisclosures[id]; record != nil {
			found = true
			total += len(ordinaryToolRows(record.rows))
			expanded = expanded || record.expanded
			complete = complete && record.complete
		}
	}
	if !found || total == 0 {
		return turnDockField{}, false
	}
	label := turnToolLabel(total)
	glyph := "▸"
	if expanded {
		glyph = "▾"
	}
	return turnDockField{
		raw: glyph + " " + label, rendered: inlineActivityControl(glyph, label, complete), overlay: turnDockOverlayTools,
	}, true
}

func (m *replModel) inlineImageField(ids []int64) (turnDockField, []transcriptImage, bool) {
	images := m.toolInspectionImages(ids)
	if len(images) == 0 {
		return turnDockField{}, nil, false
	}
	label := turnImageLabel(len(images))
	glyph := "▸"
	if m.toolInspectionExpanded(ids) {
		glyph = "▾"
	}
	return turnDockField{
		raw: glyph + " " + label, rendered: inlineActivityControl(glyph, label, false), overlay: turnDockOverlayImages,
	}, images, true
}

func inlineActivityDetail(text string) string {
	_, detail, ok := strings.Cut(text, "\n")
	if !ok {
		return ""
	}
	return detail
}

// layoutInlineActivityBlocks gives inline controls the exact one-line layout
// used by the trailer. Adjacent thought/tool controls become one row while
// their independently expandable detail remains beneath it.
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
		if len(block.reasoningIDs) > 0 {
			block.activityReasoningDetail = detail
		} else {
			block.activityToolDetail = detail
		}

		if n := len(laidOut); n > 0 {
			previous := &laidOut[n-1]
			if previous.turnTrailerID == 0 && previous.isActivity() {
				if len(previous.images) > 0 && len(block.images) > 0 {
					block.activityToolDetail = offsetTranscriptImageMarkers(block.activityToolDetail, len(previous.images))
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
	// Running children count as progress in one Agents control at a time: the
	// launch row until its turn has a trailer, then that trailer.
	lastTrailer := -1
	for i := range laidOut {
		if laidOut[i].turnTrailerID != 0 {
			lastTrailer = i
		}
	}
	for i := range laidOut {
		block := &laidOut[i]
		if !block.isActivity() {
			continue
		}
		movedOn := !m.busy
		if !movedOn {
			for j := i + 1; j < len(laidOut); j++ {
				if laidOut[j].key == "slash" {
					continue
				}
				movedOn = true
				break
			}
		}
		m.layoutInlineActivityBlock(block, width, movedOn, !m.busy || i < lastTrailer)
	}
	return laidOut
}

func (m *replModel) layoutInlineActivityBlock(block *transcriptDisplayBlock, width int, settled, agentsHandedOff bool) {
	var fields []turnDockField
	if field, ok := m.inlineReasoningField(block.reasoningIDs); ok {
		fields = append(fields, field)
	}
	if field, ok := m.inlineToolField(block.toolDisclosureIDs); ok {
		fields = append(fields, field)
	}
	if field, ok := m.agentField(block.toolDisclosureIDs, m.agentsExpanded(block.toolDisclosureIDs), agentsHandedOff); ok {
		fields = append(fields, field)
	}
	block.activityImageDetail = ""
	if field, inspectionImages, ok := m.inlineImageField(block.toolDisclosureIDs); ok {
		fields = append(fields, field)
		if m.toolInspectionExpanded(block.toolDisclosureIDs) {
			remaining := maxTranscriptImagesPerBlock - len(block.images)
			if remaining > 0 {
				inspectionImages = inspectionImages[:min(len(inspectionImages), remaining)]
				block.activityImageDetail = offsetTranscriptImageMarkers(
					renderInspectionTranscriptImages(inspectionImages), len(block.images),
				)
				block.images = append(block.images, inspectionImages...)
			}
		}
	}
	for i := range fields {
		glyph, label, ok := strings.Cut(fields[i].raw, " ")
		if ok {
			fields[i].rendered = inlineActivityControl(glyph, label, settled)
		}
	}
	header, placements := renderTurnActivityRow(fields, width)
	block.text = header
	block.activityReasoningDetail = boundedReasoningDetail(block.activityReasoningDetail, reasoningPreviewLines)
	for _, detail := range []string{block.activityReasoningDetail, block.activityToolDetail} {
		if detail != "" {
			block.text += "\n" + detail
		}
	}
	if m.agentsExpanded(block.toolDisclosureIDs) {
		m.appendAgentDetail(block, block.toolDisclosureIDs, width)
	}
	if block.activityImageDetail != "" {
		block.text += "\n" + block.activityImageDetail
	}
	block.activityFields = placements
	block.key = fmt.Sprintf("activity:r%v:t%v", block.reasoningIDs, block.toolDisclosureIDs)
}
