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
// disclosures out as one row, each control with its own triangle and its own
// independently expandable detail beneath the row.
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
				detail = inlineToolDetail(detail, record.displayRows, activityRailContentWidth(width), m.toolBaseDir)
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
	header, placements, labels := renderActivityRow(fields, width)
	block.text = header
	block.activityReasoningDetail = boundedReasoningDetail(block.activityReasoningDetail, reasoningPreviewLines)
	// Open sections stack under the row in control order behind one rail,
	// one bare rail row apart. A short thought reserves an empty second row,
	// which is already bare, so no break follows it. section returns the
	// byte offset where the content starts.
	needBreak := false
	section := func(content string, endsBlank bool) int {
		if needBreak {
			block.text += "\n" + style.RailBar
		}
		block.text += "\n"
		start := len(block.text)
		block.text += content
		needBreak = !endsBlank
		return start
	}
	block.thoughtSpan = [2]int{}
	if block.activityReasoningDetail != "" {
		content, endsBlank := railLines(block.activityReasoningDetail)
		start := section(content, endsBlank)
		block.thoughtSpan = [2]int{start, len(block.text)}
	}
	if block.activityToolDetail != "" {
		section(railLines(block.activityToolDetail))
	}
	if agentsExpanded {
		if detail, links := m.agentDetail(block.toolDisclosureIDs, width, style.Rail); detail != "" {
			start := section(detail, false)
			// Link rows count from the section's first row; the text above
			// it, laid out the same way, says which display row that is.
			native := m.nativeImages && width >= style.MinimumThumbnailCols
			above, _ := transcriptBlockRowsWithImages(block.text[:start-1], false, width, block.images, native, m.imageCellWidth, m.imageCellHeight)
			for i := range links {
				links[i].Y += len(above)
			}
			block.agentLinks = links
		}
	}
	if block.activityImageDetail != "" {
		section(block.activityImageDetail, false)
	}
	block.activityFields = placements
	// Labels stay paintable while partially clipped without being clickable.
	// Only an agents label's leading running count sweeps; outcome tails
	// stay steady.
	block.activityLabels = nil
	for i, label := range labels {
		if fields[i].kind == activityAgents {
			leading, _, _ := strings.Cut(fields[i].raw, ",")
			label.Cols = min(label.Cols, rw.StringWidth(leading))
		}
		if label.Cols > 0 {
			block.activityLabels = append(block.activityLabels, label)
		}
	}
	block.key = fmt.Sprintf("activity:r%v:t%v", block.reasoningIDs, block.toolDisclosureIDs)
}

// The open sections of an activity row hang from one quiet rail: style.Rail
// before every line, a bare style.RailBar row between sections, and no corner
// or foot, so a lone open thought costs no rows beyond its own lines. The
// rail is muted, not accent, because accent is the palette's clickable slot
// and the rail is not a control.

// activityRailCols is how many columns the rail takes before a section's
// content.
var activityRailCols = style.TextWidth(style.Rail)

// activityRailContentWidth is the width a section's tool and agent rows are
// laid out at: the rail stands in for their two-column indent.
func activityRailContentWidth(width int) int {
	return max(1, width-activityRailCols+2)
}

// railLines puts every line of detail behind the rail in place of the indent
// its renderer gave it: the four-column block indent of thought text and
// image rows, which the rail matches exactly, or the two-column indent of
// tool and agent rows. An empty line becomes a bare rail row; endsBlank
// reports whether the last line is one.
func railLines(detail string) (text string, endsBlank bool) {
	lines := strings.Split(detail, "\n")
	for i, line := range lines {
		content, ok := strings.CutPrefix(line, reasoningBlockIndent)
		if !ok {
			content = strings.TrimPrefix(line, "  ")
		}
		endsBlank = content == ""
		if endsBlank {
			lines[i] = style.RailBar
			continue
		}
		lines[i] = style.Rail + content
	}
	return strings.Join(lines, "\n"), endsBlank
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
