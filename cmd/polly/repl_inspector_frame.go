package main

import (
	"image"
	"strings"

	ui "github.com/metaspartan/gotui/v5"
	"github.com/metaspartan/gotui/v5/widgets"
)

type inspectorButton struct {
	rect   image.Rectangle
	action string
}
type inspectionLink struct {
	rect image.Rectangle
	kind viewKind
	key  string
}

func (r *managedREPL) setupInspectorWidgets() {
	r.inspectorW = newTranscriptParagraph()
	noBorder(&r.inspectorW.Block)
	r.inspectorW.UseRows = true
	r.inspectorHeaderW = newLiteralParagraph()
	noBorder(&r.inspectorHeaderW.Block)
	r.inspectorHeaderW.WrapText = false
}

func (r *managedREPL) inspectorTranscriptWidth(width int) int {
	i := &r.workspace().inspector
	if !i.open || i.maximized || width < 120 {
		return width
	}
	return width - r.inspectorGeometry(width).width - 1
}

// Only the transcript region splits. The composer retains one explicit
// recipient across narrow/full-width inspection and ordinary split mode.
func (r *managedREPL) inspectorLayout(l frameLayout) *widgets.Flex {
	outer := widgets.NewFlex()
	noBorder(&outer.Block)
	outer.Direction = widgets.FlexRow
	i := &r.workspace().inspector
	width := r.inspectorGeometry(l.width).width
	x := l.width - width
	r.inspectorBounds = image.Rect(x, l.logoRows, l.width, l.logoRows+l.transcriptHeight)
	r.inspectorDivider = image.Rectangle{}
	if !i.maximized && l.width >= 120 {
		outer.AddItem(r.transcriptW, x-1, 0, false)
		divider := newLiteralParagraph()
		noBorder(&divider.Block)
		divider.Text = strings.Repeat("│\n", max(0, l.transcriptHeight-1)) + "│"
		outer.AddItem(divider, 1, 0, false)
		r.inspectorDivider = image.Rect(x-1, l.logoRows, x, l.logoRows+l.transcriptHeight)
	}
	right := widgets.NewFlex()
	noBorder(&right.Block)
	right.Direction = widgets.FlexColumn
	right.AddItem(r.inspectorHeaderW, r.inspectorHeaderRows, 0, false)
	right.AddItem(r.inspectorW, 0, 1, false)
	outer.AddItem(right, width, 0, false)
	return outer
}

func (r *managedREPL) renderInspector(l frameLayout) []terminalImagePlacement {
	w := r.workspace()
	i := &w.inspector
	if !i.open {
		r.inspectorBounds = image.Rectangle{}
		r.inspectorButtons = nil
		return nil
	}
	g := r.inspectorGeometry(l.width)
	x := l.width - g.width
	header := r.inspectorHeader(g.width, l.transcriptHeight, x, l.logoRows)
	r.inspectorHeaderW.Text = header.text
	r.inspectorButtons = header.buttons
	r.inspectorHeaderRows = header.rows
	rows := transcriptVisualRows("Loading…", ui.StyleClear, g.width)
	v := i.current
	if v != nil && v.model != nil {
		rows = v.view.Rows(v.model, v.geometry.width)
	}
	s := w.viewState(i.target)
	height := max(0, l.transcriptHeight-r.inspectorHeaderRows)
	if s.follow {
		s.top = max(0, len(rows)-height)
		s.lastRows = len(rows)
	} else {
		s.top = min(s.top, max(0, len(rows)-1))
	}
	pin := s.follow && (i.target.kind == conversationViewKind || len(rows) > height)
	r.inspectorW.Rows, r.inspectorW.TopRow, r.inspectorW.PinBottom = rows, s.top, pin
	r.inspectorW.OverlayBottom = nil
	if !s.follow && len(rows) > s.lastRows {
		r.inspectorW.OverlayBottom = [][]ui.Cell{parseStyledCells(styled("↓ new output · End to follow", "accent", ""), ui.StyleClear)}
		r.inspectorButtons = append(r.inspectorButtons, inspectorButton{image.Rect(x, l.logoRows+l.transcriptHeight-1, l.width, l.logoRows+l.transcriptHeight), "follow"})
	}
	if v == nil || v.model == nil {
		return nil
	}
	viewport := (frameLayout{width: g.width, logoRows: l.logoRows + r.inspectorHeaderRows, transcriptHeight: height}).transcriptViewport(len(rows), s.top, pin, len(r.inspectorW.OverlayBottom))
	m := v.model
	placements := m.visibleImagePlacements(viewport)
	for n := range placements {
		placements[n].X += x
		placements[n].Key = "inspector:" + i.target.key() + ":" + placements[n].Key
	}
	m.imagePlacements = placements
	m.agentLinkPlacements = m.visibleAgentLinks(viewport)
	for n := range m.agentLinkPlacements {
		m.agentLinkPlacements[n].X += x
	}
	m.reasoningPlacements = m.visibleReasoningPlacements(viewport)
	m.toolDisclosurePlacements = m.visibleToolDisclosurePlacements(viewport)
	m.agentDisclosurePlacements = m.visibleDisclosurePlacements(viewport, turnDockOverlayAgents)
	m.imageDisclosurePlacements = m.visibleImageDisclosurePlacements(viewport)
	m.turnTrailerPlacements = m.visibleTurnTrailerPlacements(viewport)
	m.inspectionLinks = m.visibleInspectionLinks(viewport, x)
	return placements
}

// Links are attached to detail rows, never to the existing disclosure header.
// Prefix layout uses the same image-aware wrapper as the actual transcript.
func (m *replModel) visibleInspectionLinks(v transcriptViewport, x int) []inspectionLink {
	var links []inspectionLink
	offset := 0
	for _, block := range m.visual.blocks {
		add := func(start, end int, kind viewKind, key string) {
			if key == "" || start < 0 {
				return
			}
			prefix, _ := transcriptCellRowsWithImages(parseStyledCells(block.text[:start], ui.StyleClear), false, v.width, block.images, m.nativeImages, m.imageCellWidth, m.imageCellHeight)
			last, _ := transcriptCellRowsWithImages(parseStyledCells(block.text[:end], ui.StyleClear), false, v.width, block.images, m.nativeImages, m.imageCellWidth, m.imageCellHeight)
			firstRow := max(0, len(prefix)-1)
			if strings.HasSuffix(block.text[:start], "\n") {
				firstRow = len(prefix)
			}
			for row := offset + firstRow; row < offset+len(last); row++ {
				if v.contains(row) {
					links = append(links, inspectionLink{image.Rect(x, v.screenY(row), x+v.width, v.screenY(row)+1), kind, key})
				}
			}
		}
		if trailer := m.turnTrailers[block.turnTrailerID]; trailer != nil {
			switch trailer.dock.overlay {
			case turnDockOverlayTools:
				searchAt := strings.IndexByte(block.text, '\n') + 1
				for _, record := range m.turnDockToolRecords(trailer.dock) {
					for _, row := range ordinaryToolRows(record.rows) {
						if row.line == "" {
							continue
						}
						if n := strings.Index(block.text[searchAt:], row.line); n >= 0 {
							n += searchAt
							add(n, n+len(row.line), toolViewKind, row.inspectionKey)
							searchAt = n + len(row.line)
						}
					}
				}
			case turnDockOverlayThought:
				records := m.turnDockThoughtRecords(trailer.dock)
				if len(records) > 0 {
					add(strings.IndexByte(block.text, '\n')+1, len(block.text), thoughtViewKind, records[len(records)-1].inspectionKey)
				}
			}
		}
		searchAt := 0
		for _, id := range block.toolDisclosureIDs {
			r := m.toolDisclosures[id]
			if r == nil || !r.expanded {
				continue
			}
			for _, row := range ordinaryToolRows(r.rows) {
				if row.line == "" {
					continue
				}
				n := strings.Index(block.text[searchAt:], row.line)
				if n >= 0 {
					n += searchAt
					add(n, n+len(row.line), toolViewKind, row.inspectionKey)
					searchAt = n + len(row.line)
				}
			}
		}
		// The bounded thought tail immediately follows the activity header.
		for n := len(block.reasoningIDs) - 1; n >= 0; n-- {
			r := m.reasoningRecords[block.reasoningIDs[n]]
			if r == nil || !r.expanded {
				continue
			}
			start := strings.IndexByte(block.text, '\n') + 1
			if start <= 0 {
				break
			}
			end := start
			for count := 0; count < reasoningPreviewLines && end < len(block.text); count++ {
				next := strings.IndexByte(block.text[end:], '\n')
				if next < 0 {
					end = len(block.text)
					break
				}
				end += next + 1
			}
			add(start, end, thoughtViewKind, r.inspectionKey)
			break
		}
		offset += len(block.rows)
	}
	return links
}
