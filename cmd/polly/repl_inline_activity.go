package main

import (
	"fmt"
	"slices"
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
		expanded = expanded || record.anyOpen()
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

func (m *replModel) inlineImageField(ids []int64, expanded bool) (turnDockField, []style.Image, bool) {
	images := m.toolInspectionImages(ids)
	if len(images) == 0 {
		return turnDockField{}, nil, false
	}
	return activityField(turnImageLabel(len(images)), activityImages, expanded), images, true
}

// renderInspectionImages lays viewed images out in horizontal strips when
// thumbnails draw natively. Each slot is sized from the same CellGeometry
// inputs the transcript cell pass uses, so every thumbnail fits the slot
// layout gave it. Without a native backend only the captions survive, and
// they stack one per row.
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

// layoutInlineActivityBlocks lays adjacent thought, tool, agent, and image
// records out as one activity line: a header row for each chunk of the work,
// and under an open header the chunk's rows, as the turn made them.
func (m *replModel) layoutInlineActivityBlocks(blocks []transcriptDisplayBlock, width int, now time.Time) []transcriptDisplayBlock {
	if width < 1 {
		width = 80
	}
	laidOut := make([]transcriptDisplayBlock, 0, len(blocks))
	for _, block := range blocks {
		if !block.isActivity() {
			laidOut = append(laidOut, block)
			continue
		}
		if n := len(laidOut); n > 0 {
			previous := &laidOut[n-1]
			// A queued prompt can be echoed above the last activity of the
			// previous turn, leaving the two turns' activity adjacent.
			if previous.turnTrailerID == 0 && previous.isActivity() && previous.activityTurn == block.activityTurn {
				previous.reasoningIDs = append(previous.reasoningIDs, block.reasoningIDs...)
				previous.toolDisclosureIDs = append(previous.toolDisclosureIDs, block.toolDisclosureIDs...)
				previous.images = append(previous.images, block.images...)
				continue
			}
		}
		laidOut = append(laidOut, block)
	}
	for i := range laidOut {
		if laidOut[i].isActivity() {
			m.layoutInlineActivityBlock(&laidOut[i], width, now)
		}
	}
	return laidOut
}

// Only the newest activity block of the running turn hands a live label on
// from call to call. Activity separated from it by assistant prose is past.
func (m *replModel) isCurrentActivityBlock(block *transcriptDisplayBlock) bool {
	if !m.busy {
		return false
	}
	var tool *toolDisclosureRecord
	var thought *reasoningRecord
	if n := len(m.turnToolDisclosureIDs); n > 0 {
		tool = m.toolDisclosures.get(m.turnToolDisclosureIDs[n-1])
	}
	if n := len(m.turnReasoningIDs); n > 0 {
		thought = m.reasoningRecords.get(m.turnReasoningIDs[n-1])
	}
	if tool != nil && (thought == nil || tool.transcriptIndex >= thought.transcriptIndex) {
		return slices.Contains(block.toolDisclosureIDs, tool.id)
	}
	return thought != nil && slices.Contains(block.reasoningIDs, thought.id)
}

// layoutInlineActivityBlock writes one activity line: its chunks in the
// order the work happened, each a header row, and under an open header one
// row per call and per thought, then the chunk's agents and viewed images.
// Folding changes how much of the work shows, never its order. A thought
// with no run to join is a row of its own among the headers. A collapsed
// line past its row budget leads with a summary header that stands for its
// earlier runs. The headers, the thought rows and the call rows are the
// line's controls; where each one landed is recorded in logical lines of the
// text, which the wrap pass turns into display rows.
func (m *replModel) layoutInlineActivityBlock(block *transcriptDisplayBlock, width int, now time.Time) {
	block.chunks = m.activityChunks(block.reasoningIDs, block.toolDisclosureIDs)
	block.currentActivity = m.isCurrentActivityBlock(block)
	// A collapsed line keeps to its row budget: the runs past it fold under
	// one summary header, which is the line's first chunk. Closed, it hides
	// their headers; open, they follow it as they would without it.
	folded := m.foldedRuns(block)
	hidden := 0
	if folded > 0 {
		open := m.chunkOpen(runsSummaryKey(block.chunks[0].key))
		summary := m.runsSummary(block.chunks[:folded], folded < len(block.chunks), open)
		block.chunks = append([]activityChunk{summary}, block.chunks...)
		if !open {
			hidden = folded
		}
	}
	// The newest run has no header of its own when the summary stands for it.
	if newestShown := hidden == 0 || hidden < len(block.chunks)-1; block.currentActivity && !now.IsZero() && newestShown {
		// A following thought can have its own row without cutting short the
		// last call label. Only the newest tool run can hold a live label.
		for i := len(block.chunks) - 1; i >= 0; i-- {
			c := &block.chunks[i]
			if c.group != chunkThought {
				m.holdCollapsedActivity(c, now)
				break
			}
		}
	}
	block.activityFields, block.activityLabels, block.activityTargets, block.agentSections = nil, nil, nil, nil
	block.activityShift, block.openFirst, block.openLast = 0, false, false
	var b strings.Builder
	lines := 0
	// write appends whole rows and reports the line the first of them is.
	write := func(text string) int {
		at := lines
		if lines > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(text)
		lines += strings.Count(text, "\n") + 1
		return at
	}
	// air is owed when the rows above end in an opened detail: one bare row
	// before whatever follows, so a diff or an output does not run into it.
	air := false
	for i := range block.chunks {
		c := &block.chunks[i]
		// The chunks after a closed summary are the runs it stands for.
		if i >= 1 && i <= hidden {
			continue
		}
		last := i == len(block.chunks)-1
		if air {
			write(style.Rail)
			air = false
		}
		if c.group == chunkThought {
			p := c.parts[0]
			row := renderThoughtRow(fitLabel(sentenceCase(headerText(c.label)), max(1, width-2)), c.live)
			at := write(row)
			cols := style.TextWidth(row)
			// The row starts at the edge, a rail's width left of a chunk's rows.
			block.activityTargets = append(block.activityTargets, toolOutputTarget{recordID: p.thought.id, key: m.thoughtPartKey(p.thought, p.segment), action: thoughtPartAction, line: at, x: -activityRailCols, cols: cols})
			block.activityLabels = append(block.activityLabels, turnDockPlacement{chunk: i + 1, kind: activityThought, X: 2, Y: at, Cols: cols - 2})
			if text := thoughtTextRows(p.thought, p.segment); len(text) > 0 {
				for j := range text {
					text[j] = reasoningBlockIndent + text[j]
				}
				write(indentLines(strings.Join(text, "\n")))
				block.openFirst, block.openLast = block.openFirst || i == 0, last
				air = true
			}
			continue
		}
		open := m.chunkOpen(c.key)
		text, cols, label := renderChunkHeader(c, width, open)
		at := write(text)
		block.activityFields = append(block.activityFields, turnDockPlacement{chunk: i + 1, kind: activityAll, Y: at, Cols: cols})
		if label.Cols > 0 {
			label.chunk, label.Y = i+1, at
			block.activityLabels = append(block.activityLabels, label)
		}
		// A header ends the detail above it: the row before never claims it.
		block.activityTargets = append(block.activityTargets, toolOutputTarget{line: at})
		// A summary opens onto the headers after it and has no rows itself.
		if !open || c.summary {
			continue
		}
		// What is open under a header stacks in sections, one bare row
		// between them so the calls, the agents and the images read apart.
		sections := 0
		section := func() {
			if sections > 0 {
				write(style.Rail)
			}
			sections++
		}
		body := m.chunkBody(c, width)
		if body.text != "" {
			section()
			start := write(indentLines(body.text))
			for _, target := range body.targets {
				target.line += start
				block.activityTargets = append(block.activityTargets, target)
			}
			air = body.open
		}
		agents, images := c.details(max(0, style.MaxImagesPerBlock-len(block.images)))
		if detail, links := m.agentRowsDetail(agents, width, style.Rail); detail != "" {
			section()
			at := write(detail)
			if len(links) > 0 {
				block.agentSections = append(block.agentSections, agentSection{line: at, links: links})
			}
			air = false
		}
		if len(images) > 0 {
			section()
			write(style.OffsetImageMarkers(m.renderInspectionImages(images, width), len(block.images)))
			block.images = append(block.images, images...)
			air = true
		}
		if sections > 0 {
			block.openFirst, block.openLast = block.openFirst || i == 0, last
		}
	}
	block.text = b.String()
	block.activityLines = lines
	// Adjacent activity keeps its first section's identity as more calls arrive.
	// Numeric registry IDs can be reused when a saved history window shifts.
	if !strings.HasPrefix(block.key, "section:") {
		block.key = fmt.Sprintf("activity:r%v:t%v", block.reasoningIDs, block.toolDisclosureIDs)
	}
}

type agentSection struct {
	line  int         // logical line in the activity layout
	links []agentLink // wrapped rows relative to the start of the section
}

// layoutAgentLinks locates each section in the final wrap, including any
// spacing and fitted image rows above it. It never rewraps a block prefix.
func layoutAgentLinks(starts []int, shift int, sections []agentSection) []agentLink {
	var links []agentLink
	for _, section := range sections {
		line := shift + section.line
		if line >= len(starts) {
			continue
		}
		at := starts[line]
		for _, link := range section.links {
			link.Y += at
			links = append(links, link)
		}
	}
	return links
}

// layoutToolOutputLinks uses logical-line offsets from the renderer's one wrap
// pass, rather than rewrapping a growing prefix for every tool/control. Rows
// covers the body until the next target (or the line's end), so an output
// can load on view even when its header is above the viewport. A target with
// no key is a chunk header: it ends the body above it and is no link itself.
func layoutToolOutputLinks(starts []int, firstLine, endLine int, targets []toolOutputTarget, width int) []toolOutputLink {
	if len(targets) == 0 || len(starts) == 0 {
		return nil
	}
	at := func(line int) int { return starts[min(max(0, line), len(starts)-1)] }
	links := make([]toolOutputLink, 0, len(targets))
	for i, target := range targets {
		if target.key == "" {
			continue
		}
		end := at(endLine)
		if i+1 < len(targets) {
			end = at(firstLine + targets[i+1].line)
		}
		y := at(firstLine + target.line)
		x := activityRailCols + target.x
		links = append(links, toolOutputLink{recordID: target.recordID, key: target.key, action: target.action,
			X: x, Y: y, Cols: min(target.cols, width-x), Rows: end - y})
	}
	return links
}

// activityDisplayRows turns placements that count logical lines of a block's
// text into ones that count its display rows, by the wrap pass's line
// starts. shift is how many lines the spacing pass put above the layout's.
func activityDisplayRows(starts []int, shift int, placements []turnDockPlacement) []turnDockPlacement {
	if len(placements) == 0 {
		return nil
	}
	out := make([]turnDockPlacement, len(placements))
	for i, p := range placements {
		if line := shift + p.Y; len(starts) > 0 {
			p.Y = starts[min(max(0, line), len(starts)-1)]
		}
		out[i] = p
	}
	return out
}

// spaceExpandedActivityBlocks separates an activity line from the prose
// around it where the line shows rows there: above when its first chunk is
// open, below when its last is. A line of headers alone sits against its
// neighbours, and none takes a row above when it follows the prompt
// directly. Keep boundary rows inside the activity block so viewport
// anchoring measures the full height change when a fold opens or closes.
func spaceExpandedActivityBlocks(blocks []transcriptDisplayBlock) {
	for i := range blocks {
		block := &blocks[i]
		if !block.isActivity() || (!block.openFirst && !block.openLast) {
			continue
		}
		before, after := "", ""
		if block.openFirst && i > 0 && !strings.HasPrefix(blocks[i-1].text, userGutter()) {
			previous := blocks[i-1].text
			previous = previous[strings.LastIndex(previous, "\n")+1:]
			if strings.TrimSpace(ui.CellsToString(style.ParseCells(previous, ui.StyleClear))) != "" {
				before = "\n"
				block.activityShift++
			}
		}
		if block.openLast && i+1 < len(blocks) {
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

// What is open under a chunk's header sits behind style.Rail, a muted run
// of spaces: nothing drawn, no corner, no rows between sections beyond the
// one that sets them apart. The style is what lets the wrapper repeat the
// indent on continuation rows.

// activityIndent is the rail as it reads: the plain indent before a
// section's content.
const activityIndent = style.RailIndent

// activityRailCols is how many columns the rail takes before a section's
// content.
var activityRailCols = style.TextWidth(style.Rail)

// activityRailContentWidth is the width a section's tool and agent rows are
// laid out at: the indent stands in for their two-column indent.
func activityRailContentWidth(width int) int {
	return max(1, width-activityRailCols+2)
}

// indentLines replaces each detail row's plain two-column block indent with
// the styled rail, preserving any source indentation and tool body padding.
// An empty line is a bare rail row.
func indentLines(detail string) string {
	var b strings.Builder
	b.Grow(len(detail) + (strings.Count(detail, "\n")+1)*len(style.Rail))
	first := true
	for line := range strings.SplitSeq(detail, "\n") {
		if !first {
			b.WriteByte('\n')
		}
		first = false
		b.WriteString(style.Rail)
		b.WriteString(strings.TrimPrefix(line, activityIndent))
	}
	return b.String()
}
