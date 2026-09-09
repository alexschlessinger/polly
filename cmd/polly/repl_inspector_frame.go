package main

import (
	"image"
	"strings"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	ui "github.com/metaspartan/gotui/v5"
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
	r.inspectorHeaderW = style.NewLiteralParagraph()
	noBorder(&r.inspectorHeaderW.Block)
	r.inspectorHeaderW.WrapText = false
}

// renderInspector projects the inspected view into the frame interior. The
// header is measured first; the body takes the rows beneath it.
func (r *managedREPL) renderInspector(l frameLayout) []terminalImagePlacement {
	w := r.workspace()
	i := &w.inspector
	if !i.open {
		r.inspectorButtons = nil
		return nil
	}
	inner := l.chrome.inner
	g := r.viewGeometryFor(l.chrome, l.width)
	x, y, paneHeight := inner.Min.X, inner.Min.Y, inner.Dy()
	header := r.inspectorHeader(g.width, paneHeight, x, y)
	r.inspectorHeaderW.Text = header.text
	r.inspectorButtons = header.buttons
	r.inspectorHeaderRows = header.rows
	rows := style.VisualRows("Loading…", ui.StyleClear, g.width)
	v := i.current
	if v != nil && v.model != nil {
		// While a refreshed projection is loading, the previous model still
		// has to fit the current pane (especially during divider dragging).
		rows = v.view.Rows(v.model, g.width)
	}
	s := w.viewState(i.target)
	// Seed the new-output baseline from a settled paint; a stale model shown
	// while a fresh projection loads must not count as the starting point.
	if s.lastRows < 0 && v != nil && v.model != nil && !v.loading {
		s.lastRows = len(rows)
	}
	height := max(0, paneHeight-r.inspectorHeaderRows)
	if s.follow {
		s.top = max(0, len(rows)-height)
		s.lastRows = len(rows)
	} else {
		s.top = min(s.top, max(0, len(rows)-1))
	}
	pin := s.follow && (i.target.kind == conversationViewKind || len(rows) > height)
	r.inspectorW.Rows, r.inspectorW.TopRow, r.inspectorW.PinBottom = rows, s.top, pin
	r.inspectorW.OverlayBottom = nil
	if !s.follow && s.lastRows >= 0 && len(rows) > s.lastRows {
		r.inspectorW.OverlayBottom = [][]ui.Cell{style.ParseCells(style.Styled("↓ new output · End to follow", "accent", ""), ui.StyleClear)}
		r.inspectorButtons = append(r.inspectorButtons, inspectorButton{image.Rect(x, y+paneHeight-1, x+g.width, y+paneHeight), "follow"})
	}
	if v == nil || v.model == nil {
		return nil
	}
	viewport := (frameLayout{width: g.width, logoRows: y + r.inspectorHeaderRows, transcriptHeight: height}).transcriptViewport(len(rows), s.top, pin, len(r.inspectorW.OverlayBottom))
	m := v.model
	offset := 0
	for _, block := range m.visual.blocks {
		if block.key == "initial-prompt" && viewport.contains(offset) {
			row := viewport.screenY(offset)
			r.inspectorButtons = append(r.inspectorButtons, inspectorButton{image.Rect(x, row, x+g.width, row+1), "prompt"})
			break
		}
		offset += len(block.rows)
	}
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
	m.placeDisclosures(viewport)
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
			prefix, _ := transcriptCellRowsWithImages(style.ParseCells(block.text[:start], ui.StyleClear), false, v.width, block.images, m.nativeImages, m.imageCellWidth, m.imageCellHeight)
			last, _ := transcriptCellRowsWithImages(style.ParseCells(block.text[:end], ui.StyleClear), false, v.width, block.images, m.nativeImages, m.imageCellWidth, m.imageCellHeight)
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
		searchAt := 0
		for _, id := range block.toolDisclosureIDs {
			r := m.toolDisclosures.get(id)
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
			r := m.reasoningRecords.get(block.reasoningIDs[n])
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
