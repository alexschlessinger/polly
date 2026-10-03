package main

import (
	"image"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/termimg"
	ui "github.com/metaspartan/gotui/v5"
)

type inspectorButton struct {
	rect   image.Rectangle
	action string
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
	i.keyboardPaintedTarget = i.target.key()
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
		r.ensureInlineToolOutputs(v.model, &i.target)
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
	viewport := (frameLayout{width: g.width, transcriptHeight: height}).transcriptViewport(len(rows), s.top, pin, len(r.inspectorW.OverlayBottom))
	viewport.logoRows = y + r.inspectorHeaderRows
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
	m.toolOutputLinks = m.visibleToolOutputLinks(viewport, x)
	return placements
}
