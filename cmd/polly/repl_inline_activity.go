package main

import (
	"fmt"
	"strings"
	"time"

	rw "github.com/mattn/go-runewidth"
	ui "github.com/metaspartan/gotui/v5"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/termimg"
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

// inlineToolFields counts the ordinary tool rows, including hidden, running,
// and unsuccessful calls, names the files edited (distinct paths, with their
// line counts), and ends with the latest call, which takes the row under a
// folded line with callWidth columns to itself; an open line leaves the
// latest call to its rows. Agent rows have their own field. Every field is
// the Tools disclosure; expanded is whether any of the line's tool
// disclosures is open.
func (m *replModel) inlineToolFields(ids []int64, expanded bool, callWidth int) []turnDockField {
	edits := map[string]bool{}
	calls, adds, dels := 0, 0, 0
	// latest is the call the line names at its end: the first still
	// running, so a parallel batch shows each call in turn, else the last
	// one settled.
	var latest, running *toolDisclosureRow
	for _, id := range ids {
		record := m.toolDisclosures.get(id)
		if record == nil {
			continue
		}
		rows := ordinaryToolRows(record.rows)
		for i := range rows {
			row := rows[i]
			if row.line == "" {
				continue
			}
			calls++
			// Image calls still own tool rows, but their viewed images have
			// a separate field instead of a latest-call label.
			if len(row.inspectionImages) > 0 {
				continue
			}
			latest = &rows[i]
			if !row.settled {
				if running == nil {
					running = &rows[i]
				}
				continue
			}
			// A file is counted once however often it was edited; a call
			// that named no file counts on its own.
			switch compactToolName(row.toolName) {
			case "edit", "write":
				key := row.callID
				if row.file != nil {
					key = row.file.path
				}
				edits[key] = true
				a, d := row.pres.changes.totals()
				adds, dels = adds+a, dels+d
			}
		}
	}
	var fields []turnDockField
	add := func(raw, rendered string) {
		field := activityField(raw, activityTools, expanded)
		if rendered != "" {
			field.rendered = rendered
		}
		fields = append(fields, field)
	}
	if calls > 0 {
		add(countNoun(calls, "tool call"), "")
	}
	if n := len(edits); n > 0 {
		raw, styled := changeTotalsText(adds, dels)
		label := "edited " + countNoun(n, "file")
		if raw != "" {
			add(label+" "+raw, style.Styled(label, "muted", "")+" "+styled)
		} else {
			add(label, "")
		}
	}
	if running != nil {
		latest = running
	}
	if latest != nil && !expanded {
		fields = append(fields, m.latestToolField(*latest, callWidth))
	}
	return fields
}

// latestToolField is the line's last call as it stands: swept and bright
// while it runs, then behind its outcome glyph until the next call takes
// its place, so a call too quick to watch still shows what it was and how
// it ended. The label fits width columns.
func (m *replModel) latestToolField(row toolDisclosureRow, width int) turnDockField {
	label := style.StripImageMarkers(m.latestToolLabel(row, width-2))
	var field turnDockField
	if !row.settled {
		field = activityField(label, activityRunning, false)
		field.rendered = style.Styled(label, "", "")
	} else {
		glyph, color := "✓", "ok"
		if row.pres.outcome != toolOutcomeOK {
			glyph, color = "✗", "err"
		}
		field = activityField(glyph+" "+label, activityTools, false)
		field.rendered = style.Styled(glyph, color, "bold") + " " + style.Styled(label, "muted", "")
	}
	field.latest = true
	return field
}

// latestToolLabel names a call for the activity line: the command, the
// file, or the tool, fit to width columns.
func (m *replModel) latestToolLabel(row toolDisclosureRow, width int) string {
	switch {
	case row.bash != nil:
		return "$ " + row.bash.fit(max(8, width-2))
	case row.file != nil:
		name := compactToolName(row.toolName)
		return name + " " + fitToolPath(relativeToolPath(row.file.path, m.toolBaseDir), max(8, width-rw.StringWidth(name)-1))
	default:
		if row.toolName == "" {
			return row.label
		}
		return compactToolName(row.toolName)
	}
}

func countNoun(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// toolsExpanded reports whether any of the tool disclosures is open.
func (m *replModel) toolsExpanded(ids []int64) bool {
	for _, id := range ids {
		if record := m.toolDisclosures.get(id); record != nil && record.expanded {
			return true
		}
	}
	return false
}

// toolRowsRevealed reports whether any of the line's tool disclosures shows
// every row; the reveal is the line's, so one record's flag speaks for all.
func (m *replModel) toolRowsRevealed(ids []int64) bool {
	for _, id := range ids {
		if record := m.toolDisclosures.get(id); record != nil && record.allRows {
			return true
		}
	}
	return false
}

// activityRowWindow is how many of its newest tool rows an open activity
// line shows. The rest hide behind an "N earlier calls" link above the
// window, so an open line stays within a screen however long the turn ran.
// Agent rows and viewed images are few and stay whole.
const activityRowWindow = 5

// earlierRowsKey is the output-link key of the "earlier calls" link; its
// action is earlierRowsAction. toolOutputRow finds no row by it.
const (
	earlierRowsKey    = "earlier"
	earlierRowsAction = "earlier"
)

// earlierLinesKey and earlierLinesAction name the thought's "earlier lines"
// link, the thought's counterpart of the tool rows' fold mark.
const (
	earlierLinesKey    = "earlier-lines"
	earlierLinesAction = "earlier-lines"
)

// windowReasoningDetail keeps the newest reasoningPreviewLines rows of a
// line's thought behind a "⋯ N earlier lines" link, unless one of its
// records is revealed; a revealed thought whose retained text lost its
// beginning says so on its first row. The target's line counts from the
// thought section's first row.
func (m *replModel) windowReasoningDetail(detail string, ids []int64) (string, []toolOutputTarget) {
	if detail == "" {
		return "", nil
	}
	revealed, cut := false, false
	for _, id := range ids {
		if record := m.reasoningRecords.get(id); record != nil {
			revealed, cut = revealed || record.allLines, cut || record.tailCut
		}
	}
	lines := strings.Split(detail, "\n")
	if revealed {
		if cut {
			lines = append([]string{reasoningBlockIndent + style.Styled("… earlier text not kept", "muted", "")}, lines...)
		}
		return strings.Join(lines, "\n"), nil
	}
	hidden := len(lines) - reasoningPreviewLines
	if hidden <= 0 {
		return detail, nil
	}
	label := "⋯ " + countNoun(hidden, "earlier line")
	lines = append([]string{reasoningBlockIndent + style.Styled(label, "muted", "")}, lines[hidden:]...)
	target := toolOutputTarget{recordID: ids[0], key: earlierLinesKey, action: earlierLinesAction, cols: rw.StringWidth(label)}
	return strings.Join(lines, "\n"), []toolOutputTarget{target}
}

// windowToolDetail keeps the newest activityRowWindow rows of a line's tool
// detail, each with the arguments, output or diff open under it, and puts
// the link that reveals the rest where the hidden rows were. Targets keep
// their logical-line offsets into the returned detail.
func windowToolDetail(detail string, targets []toolOutputTarget, recordID int64) (string, []toolOutputTarget) {
	var rows []int
	for i, target := range targets {
		if target.action == "" {
			rows = append(rows, i)
		}
	}
	hidden := len(rows) - activityRowWindow
	if hidden <= 0 {
		return detail, targets
	}
	cut := targets[rows[hidden]].line
	lines := strings.Split(detail, "\n")
	if cut <= 0 || cut >= len(lines) {
		return detail, targets
	}
	// The fold mark sits in the glyph column, so the link reads as a fold
	// rather than another call.
	label := "⋯ " + countNoun(hidden, "earlier call")
	kept := append([]string{reasoningBlockIndent + style.Styled(label, "muted", "")}, lines[cut:]...)
	windowed := []toolOutputTarget{{recordID: recordID, key: earlierRowsKey, action: earlierRowsAction, cols: rw.StringWidth(label)}}
	for _, target := range targets {
		if target.line >= cut {
			target.line += 1 - cut
			windowed = append(windowed, target)
		}
	}
	return strings.Join(kept, "\n"), windowed
}

func (m *replModel) inlineImageField(ids []int64, expanded bool) (turnDockField, []style.Image, bool) {
	images := m.toolInspectionImages(ids)
	if len(images) == 0 {
		return turnDockField{}, nil, false
	}
	return activityField(turnImageLabel(len(images)), activityImages, expanded), images, true
}

// renderInspectionImages lays the Images disclosure out in horizontal strips
// when thumbnails draw natively. Each slot is sized from the same
// CellGeometry inputs the transcript cell pass uses, so every thumbnail fits
// the slot layout gave it. Without a native backend only the captions
// survive, and they stack one per row.
func (m *replModel) renderInspectionImages(images []style.Image, width int) string {
	if !m.nativeImages || width < style.MinimumThumbnailCols {
		return style.RenderInspectionImages(images)
	}
	content := width - activityRailCols
	cols := make([]int, len(images))
	for i, img := range images {
		maxCols, maxRows := style.ImageBounds(img)
		cols[i], _, _ = termimg.CellGeometry(img, min(maxCols, content), maxRows, m.imageCellWidth, m.imageCellHeight)
		if cols[i] <= 0 {
			return style.RenderInspectionImages(images)
		}
	}
	return style.RenderInspectionImageStrips(images, cols, content)
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
				detail, block.activityToolTargets = inlineToolDetail(detail, record.displayRows, activityRailContentWidth(width), m.toolBaseDir, record.id)
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
						shift := strings.Count(previous.activityToolDetail, "\n") + 1
						for i := range block.activityToolTargets {
							block.activityToolTargets[i].line += shift
						}
						previous.activityToolDetail += "\n"
					}
					previous.activityToolDetail += block.activityToolDetail
					previous.activityToolTargets = append(previous.activityToolTargets, block.activityToolTargets...)
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
	fields = append(fields, m.inlineToolFields(block.toolDisclosureIDs, m.toolsExpanded(block.toolDisclosureIDs), width-len(activityIndent))...)
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
					m.renderInspectionImages(inspectionImages, width), len(block.images),
				)
				block.images = append(block.images, inspectionImages...)
			}
		}
	}
	header, placements, labels := renderActivityRow(fields, m.activityTrailerField(block.reasoningIDs, block.toolDisclosureIDs), width)
	block.text = header
	block.activityReasoningDetail, block.activityThoughtTargets = m.windowReasoningDetail(block.activityReasoningDetail, block.reasoningIDs)
	// Open sections stack under the line in control order, indented under
	// it, one blank row between neighbours so the thought, the calls and
	// the agents read apart; the latest call's row, when the line carries
	// one, is part of its head and stands apart the same way. section
	// returns the byte offset where the content starts.
	sections := 0
	section := func(content string) int {
		if sections > 0 || strings.Contains(block.text, "\n") {
			block.text += "\n" + style.Rail
		}
		sections++
		block.text += "\n"
		start := len(block.text)
		block.text += content
		return start
	}
	if block.activityReasoningDetail != "" {
		content := indentLines(block.activityReasoningDetail)
		start := section(content)
		block.thoughtFirstLine = strings.Count(block.text[:start], "\n")
		block.thoughtEndLine = block.thoughtFirstLine + strings.Count(content, "\n") + 1
	}
	if block.activityToolDetail != "" && !m.toolRowsRevealed(block.toolDisclosureIDs) {
		block.activityToolDetail, block.activityToolTargets = windowToolDetail(block.activityToolDetail, block.activityToolTargets, block.toolDisclosureIDs[0])
	}
	if block.activityToolDetail != "" {
		content := indentLines(block.activityToolDetail)
		start := section(content)
		block.toolFirstLine = strings.Count(block.text[:start], "\n")
		block.toolEndLine = block.toolFirstLine + strings.Count(content, "\n") + 1
	}
	if agentsExpanded {
		if detail, links := m.agentDetail(block.toolDisclosureIDs, width, style.Rail); detail != "" {
			start := section(detail)
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
		section(block.activityImageDetail)
	}
	block.activitySections = sections
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
	// Adjacent activity keeps its first section's identity as more calls arrive.
	// Numeric registry IDs can be reused when a saved history window shifts.
	if !strings.HasPrefix(block.key, "section:") {
		block.key = fmt.Sprintf("activity:r%v:t%v", block.reasoningIDs, block.toolDisclosureIDs)
	}
}

// layoutToolOutputLinks uses logical-line offsets from the renderer's one wrap
// pass, rather than rewrapping a growing prefix for every tool/control. Rows
// covers the body until the next target (or the tool section end), so an output
// can load on view even when its header is above the viewport.
func layoutToolOutputLinks(starts []int, firstLine, endLine int, targets []toolOutputTarget, width int) []toolOutputLink {
	if len(targets) == 0 {
		return nil
	}
	links := make([]toolOutputLink, 0, len(targets))
	for i, target := range targets {
		end := starts[endLine]
		if i+1 < len(targets) {
			end = starts[firstLine+targets[i+1].line]
		}
		y := starts[firstLine+target.line]
		x := activityRailCols + target.x
		links = append(links, toolOutputLink{recordID: target.recordID, key: target.key, action: target.action,
			X: x, Y: y, Cols: min(target.cols, width-x), Rows: end - y})
	}
	return links
}

// spaceExpandedActivityBlocks adds boundary rows to the display projection.
// Keep them inside the activity block so viewport anchoring measures the full
// height change when a disclosure opens or closes.
func spaceExpandedActivityBlocks(blocks []transcriptDisplayBlock) {
	for i := range blocks {
		block := &blocks[i]
		if !block.isActivity() || block.activitySections == 0 {
			continue
		}
		before, after := "", ""
		if i > 0 {
			previous := blocks[i-1].text
			previous = previous[strings.LastIndex(previous, "\n")+1:]
			if strings.TrimSpace(ui.CellsToString(style.ParseCells(previous, ui.StyleClear))) != "" {
				before = "\n"
				block.toolFirstLine++
				block.toolEndLine++
				block.thoughtFirstLine++
				block.thoughtEndLine++
				for j := range block.activityFields {
					block.activityFields[j].Y++
				}
				for j := range block.activityLabels {
					block.activityLabels[j].Y++
				}
				for j := range block.agentLinks {
					block.agentLinks[j].Y++
				}
			}
		}
		if i+1 < len(blocks) {
			next, _, _ := strings.Cut(blocks[i+1].text, "\n")
			if strings.TrimSpace(ui.CellsToString(style.ParseCells(next, ui.StyleClear))) != "" && !strings.HasSuffix(block.text, "\n") {
				after = "\n"
			}
		}
		if before != "" || after != "" {
			block.text = before + block.text + after
		}
	}
}

// The open sections of an activity line sit under it behind style.Rail, a
// muted run of spaces: nothing drawn, no corner, no rows between sections,
// so a lone open thought costs no rows beyond its own lines. The style is
// what lets the wrapper repeat the indent on continuation rows.

// activityIndent is the rail as it reads: the plain indent before a
// section's content.
const activityIndent = "    "

// activityRailCols is how many columns the rail takes before a section's
// content.
var activityRailCols = style.TextWidth(style.Rail)

// activityRailContentWidth is the width a section's tool and agent rows are
// laid out at: the indent stands in for their two-column indent.
func activityRailContentWidth(width int) int {
	return max(1, width-activityRailCols+2)
}

// indentLines puts every line of detail behind the indent in place of the
// one its renderer gave it: the four-column block indent of thought text
// and image rows, which the indent matches exactly, or the two-column
// indent of tool and agent rows. An empty line is a bare rail row.
func indentLines(detail string) string {
	var b strings.Builder
	b.Grow(len(detail) + (strings.Count(detail, "\n")+1)*len(style.Rail))
	first := true
	for line := range strings.SplitSeq(detail, "\n") {
		if !first {
			b.WriteByte('\n')
		}
		first = false
		content, ok := strings.CutPrefix(line, reasoningBlockIndent)
		if !ok {
			content = strings.TrimPrefix(line, "  ")
		}
		b.WriteString(style.Rail)
		b.WriteString(content)
	}
	// A streaming thought reserves a second row; a settled one has no use
	// for it.
	text := b.String()
	for strings.HasSuffix(text, "\n"+style.Rail) {
		text = strings.TrimSuffix(text, "\n"+style.Rail)
	}
	return text
}

// Resolve activity from live records, never from cached label text or parent busy state.
func (m *replModel) inlineActivityRunning(kind activityKind, reasoningIDs, toolIDs []int64) bool {
	switch kind {
	case activityAll:
		return m.inlineActivityRunning(activityThought, reasoningIDs, toolIDs) ||
			m.inlineActivityRunning(activityRunning, reasoningIDs, toolIDs) ||
			m.inlineActivityRunning(activityAgents, reasoningIDs, toolIDs)
	case activityThought:
		for _, id := range reasoningIDs {
			if record := m.reasoningRecords.get(id); record != nil && record.active {
				return true
			}
		}
	case activityRunning:
		// The verb labels and a settled latest call hold still; only a
		// running call sweeps.
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
