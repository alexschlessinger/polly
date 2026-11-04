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
	agent := r.inspectingAgent()
	if v != nil && v.model != nil {
		// While a refreshed projection is loading, the previous model still
		// has to fit the current pane (especially during divider dragging).
		state := w.viewState(i.target)
		if agent {
			rememberViewPosition(v.model, state)
			// Running calls the owning runtime reported tick here: nothing
			// else paints an inspected display.
			v.model.refreshActiveTools()
		}
		rows = v.view.Rows(v.model, g.width)
		if agent {
			restoreViewPosition(v.model, state)
		}
	}
	s := w.viewState(i.target)
	height := max(0, paneHeight-r.inspectorHeaderRows)
	seedBaseline := s.lastRows < 0
	// The bottom overlay holds the new-output notice.
	noticeRows := 0
	settleViewport := func() transcriptViewport {
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
		r.inspectorW.OverlayBottom, noticeRows = nil, 0
		if i.target.kind != agentsViewKind && !s.follow && s.lastRows >= 0 && len(rows) > s.lastRows {
			r.inspectorW.OverlayBottom = [][]ui.Cell{style.ParseCells(style.Styled("↓ new output · End to follow", "accent", ""), ui.StyleClear)}
			noticeRows = 1
		}
		layout := frameLayout{width: g.width, transcriptHeight: height}
		viewport := layout.transcriptViewport(len(rows), s.top, pin, noticeRows)
		viewport.logoRows = y + r.inspectorHeaderRows
		return viewport
	}
	viewport := settleViewport()
	if v != nil && v.model != nil && height > 0 && r.ensureInlineToolOutputs(v.model, &i.target, viewport) {
		rows = v.view.Rows(v.model, g.width)
		if agent {
			restoreViewPosition(v.model, s)
		}
		if seedBaseline {
			s.lastRows = -1
		}
		// Account for Loading once; never cascade another load pass this paint.
		viewport = settleViewport()
	}
	if noticeRows > 0 {
		row := y + paneHeight - noticeRows
		r.inspectorButtons = append(r.inspectorButtons, inspectorButton{image.Rect(x, row, x+g.width, row+1), "follow"})
	}
	if v == nil || v.model == nil {
		return nil
	}
	m := v.model
	if i.target.kind == agentsViewKind {
		for row, action := range v.agentsActions {
			if action != "" && viewport.contains(row) {
				y := viewport.screenY(row)
				r.inspectorButtons = append(r.inspectorButtons, inspectorButton{image.Rect(x, y, x+g.width, y+1), action})
			}
		}
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
