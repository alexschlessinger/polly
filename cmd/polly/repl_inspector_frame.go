package main

import (
	"image"
	"strings"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/termimg"
	ui "github.com/metaspartan/gotui/v5"
)

type inspectorButton struct {
	rect   image.Rectangle
	action string
}

// inspectionLink is one clickable detail row. rect is the whole row, rail
// included, so a pointer resting on the rail still addresses the row; mark is
// the content the hover underline covers.
type inspectionLink struct {
	rect, mark image.Rectangle
	kind       viewKind
	key        string
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
func (r *managedREPL) renderInspector(l frameLayout) []termimg.Placement {
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
	// A re-wrap at another width changes the row count without any new
	// output. Carry the baseline across by proportion: a fully seen view
	// stays fully seen, and unseen rows stay unseen.
	if s.lastRows >= 0 && s.lastWidth > 0 && s.lastWidth != g.width && s.lastTotal > 0 && len(rows) != s.lastTotal {
		s.lastRows = min(s.lastRows, s.lastTotal) * len(rows) / s.lastTotal
	}
	height := max(0, paneHeight-r.inspectorHeaderRows)
	if i.target.kind == toolViewKind && s.toolJump != "" && v != nil && v.model != nil && !v.loading {
		// Centre the selected item when it fits the pane; a taller item
		// starts at its title so the body reads downward from there.
		start, end, offset := -1, 0, 0
		for _, block := range v.model.visual.blocks {
			if strings.HasSuffix(block.key, "/"+s.toolJump) {
				if start < 0 {
					start = offset
				}
				end = offset + len(block.rows)
			}
			offset += len(block.rows)
		}
		if start >= 0 {
			top := start
			if span := end - start; span < height {
				top = start - (height-span)/2
			}
			s.top = max(0, min(top, len(rows)-height))
			s.follow = s.top >= max(0, len(rows)-height)
			s.lastRows = len(rows)
		}
		s.toolJump = ""
	}

	if s.follow {
		s.top = max(0, len(rows)-height)
		if v != nil && v.model != nil && !v.loading {
			s.lastRows = len(rows)
		}
	} else {
		s.top = min(s.top, max(0, len(rows)-1))
	}
	s.lastWidth, s.lastTotal = g.width, len(rows)
	pin := s.follow && (i.target.kind == conversationViewKind || len(rows) > height)
	r.inspectorW.Rows, r.inspectorW.TopRow, r.inspectorW.PinBottom = rows, s.top, pin
	r.inspectorW.OverlayBottom = nil
	if i.target.kind != agentsViewKind && !s.follow && s.lastRows >= 0 && len(rows) > s.lastRows {
		r.inspectorW.OverlayBottom = [][]ui.Cell{style.ParseCells(style.Styled("↓ new output · End to follow", "accent", ""), ui.StyleClear)}
		r.inspectorButtons = append(r.inspectorButtons, inspectorButton{image.Rect(x, y+paneHeight-1, x+g.width, y+paneHeight), "follow"})
	}
	if v == nil || v.model == nil {
		return nil
	}
	viewport := (frameLayout{width: g.width, logoRows: y + r.inspectorHeaderRows, transcriptHeight: height}).transcriptViewport(len(rows), s.top, pin, len(r.inspectorW.OverlayBottom))
	m := v.model
	if i.target.kind == agentsViewKind {
		for row, action := range v.agentsActions {
			if action != "" && viewport.contains(row) {
				y := viewport.screenY(row)
				r.inspectorButtons = append(r.inspectorButtons, inspectorButton{image.Rect(x, y, x+g.width, y+1), action})
			}
		}
	}
	offset := 0
	for _, block := range m.visual.blocks {
		action := ""
		if block.key == "initial-prompt" {
			action = "prompt"
		}
		if rest, ok := strings.CutPrefix(block.key, "tool-list/"); ok {
			section, _, _ := strings.Cut(rest, "/")
			switch section {
			case "title", "agent":
				action = block.key
			}
		}
		if strings.HasPrefix(block.key, "change-list/title/") {
			action = block.key
		}
		if action != "" && viewport.contains(offset) {
			row := viewport.screenY(offset)
			r.inspectorButtons = append(r.inspectorButtons, inspectorButton{image.Rect(x, row, x+g.width, row+1), action})
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
	// Detail rows sit behind the rail: the whole row is the target, and the
	// mark starts where the row's content does.
	left := x + activityRailCols
	for _, block := range m.visual.blocks {
		// A block wholly off screen has no visible links; skip it before any
		// text work, which otherwise runs for every open block each paint.
		if offset+len(block.rows) <= v.start || offset >= v.end {
			offset += len(block.rows)
			continue
		}
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
					y := v.screenY(row)
					links = append(links, inspectionLink{
						rect: image.Rect(x, y, x+v.width, y+1),
						mark: image.Rect(left, y, x+v.width, y+1),
						kind: kind,
						key:  key,
					})
				}
			}
		}
		searchAt := 0
		for _, id := range block.toolDisclosureIDs {
			r := m.toolDisclosures.get(id)
			if r == nil || !r.expanded {
				continue
			}
			rows := ordinaryToolRows(r.rows)
			rows = rows[max(0, len(rows)-toolPreviewRows):]
			for _, row := range rows {
				line := row.inlineLineAt(activityRailContentWidth(v.width), m.toolBaseDir)
				if line == "" {
					continue
				}
				// The row sits behind the rail in the laid-out block.
				line, _ = railLines(line)
				n := strings.Index(block.text[searchAt:], line)
				if n < 0 {
					continue
				}
				n += searchAt
				end := n + len(line)
				// A change detail under the row opens the same call.
				if row.changeText != "" {
					detail, _ := railLines(row.changeDetail(activityRailContentWidth(v.width) - 2))
					if strings.HasPrefix(block.text[end:], "\n"+detail) {
						end += 1 + len(detail)
					}
				}
				add(n, end, toolViewKind, row.inspectionKey)
				searchAt = end
			}
		}
		// The bounded thought tail is the first section under the activity
		// header; layout recorded its byte range, since a rail break and an
		// empty thought line look alike in the text.
		for n := len(block.reasoningIDs) - 1; n >= 0; n-- {
			r := m.reasoningRecords.get(block.reasoningIDs[n])
			if r == nil || !r.expanded {
				continue
			}
			span := block.thoughtSpan
			if span[1] <= span[0] || span[1] > len(block.text) {
				break
			}
			add(span[0], span[1], thoughtViewKind, r.inspectionKey)
			break
		}
		offset += len(block.rows)
	}
	return links
}
