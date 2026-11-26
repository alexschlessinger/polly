package main

import (
	"image"

	ui "github.com/metaspartan/gotui/v5"
)

// scrollbar lives on a frame's right edge: the track is the border column
// beside the body, and only the thumb is painted over the border glyphs, so
// wrapping never changes when content grows or shrinks.
type scrollbar struct {
	track, thumb        image.Rectangle
	total, visible, top int
	hover, dragging     bool
}

type scrollDragState struct {
	pane   string
	offset int
}

func newScrollbar(track image.Rectangle, total, visible, top int) scrollbar {
	b := scrollbar{track: track, total: total, visible: visible, top: max(0, min(top, total-visible))}
	if track.Empty() || total <= visible || visible <= 0 {
		return b
	}
	size := max(1, min(track.Dy(), track.Dy()*visible/max(1, total)))
	y := track.Min.Y + (track.Dy()-size)*b.top/max(1, total-visible)
	b.thumb = image.Rect(track.Min.X, y, track.Max.X, y+size)
	return b
}

func (b scrollbar) draw(buf *ui.Buffer) {
	if b.thumb.Empty() {
		return
	}
	fg := chromeColor("muted")
	if b.hover || b.dragging {
		fg = chromeColor("accent")
	}
	for y := b.thumb.Min.Y; y < b.thumb.Max.Y; y++ {
		buf.SetCell(ui.Cell{Rune: '┃', Style: ui.NewStyle(fg)}, image.Pt(b.thumb.Min.X, y))
	}
}

// setInspectorScrollbar sizes the inspector's thumb from the rows renderInspector
// seated; there is no bar on a surface without a track.
func (r *managedREPL) setInspectorScrollbar(pointer image.Point, pointerKnown bool) {
	r.surface.scrollbar = scrollbar{}
	if !r.workspace().inspector.open || r.surface.track.Empty() {
		return
	}
	_, body, track := r.surface.split()
	top := r.surface.body.TopRow
	if r.surface.body.PinBottom {
		top = max(0, len(r.surface.body.Rows)-body.Dy())
	}
	r.surface.scrollbar = newScrollbar(track, len(r.surface.body.Rows), body.Dy(), top)
	r.surface.scrollbar.hover = pointerKnown && pointer.In(track)
	r.surface.scrollbar.dragging = r.scrollDrag.pane == "inspector"
}

func (r *managedREPL) setScrollTop(pane string, top int) {
	switch pane {
	case "inspector":
		b := r.surface.scrollbar
		s := r.workspace().viewState(r.workspace().inspector.target)
		s.top = max(0, min(top, b.total-b.visible))
		s.follow = s.top >= max(0, b.total-b.visible)
		// Like inspectorScroll: only a follow position marks output as seen.
		if s.follow {
			s.lastRows = b.total
		}
		r.surface.scrollbar.top = s.top
	case "modal":
		if m := r.model.modal; m != nil {
			b := r.modalScrollbar
			m.top = max(0, min(top, b.total-b.visible))
			m.selected = max(m.top, min(m.selected, m.top+b.visible-1))
			r.modalScrollbar.top = m.top
		}
	case "panel":
		if p := r.focusedOwnWindow(); p != nil {
			m := p.panel.modal
			b := r.panelScrollbar
			m.top = max(0, min(top, b.total-b.visible))
			m.selected = max(m.top, min(m.selected, m.top+b.visible-1))
			r.panelScrollbar.top = m.top
		}
	}
}

// Called with the main model lock held before pane/editor dispatch.
func (r *managedREPL) handleScrollbar(e ui.Event, modal bool) bool {
	if modal {
		return r.handleScrollbarFor(e, "modal")
	}
	return r.handleScrollbarFor(e, "inspector")
}

func (r *managedREPL) handleScrollbarFor(e ui.Event, pane string) bool {
	if e.Type != ui.MouseEvent {
		return false
	}
	mouse, ok := e.Payload.(ui.Mouse)
	if !ok {
		return false
	}
	pt := image.Pt(mouse.X, mouse.Y)
	if e.ID == "<MouseRelease>" {
		r.scrollDrag = scrollDragState{}
		return false
	}
	var b scrollbar
	switch pane {
	case "inspector":
		b = r.surface.scrollbar
	case "modal":
		b = r.modalScrollbar
	case "panel":
		b = r.panelScrollbar
	default:
		return false
	}
	if b.thumb.Empty() {
		return false
	}
	if r.scrollDrag.pane == pane && e.ID == "<MouseLeft>" {
		travel := b.track.Dy() - b.thumb.Dy()
		y := max(0, min(travel, pt.Y-b.track.Min.Y-r.scrollDrag.offset))
		top := 0
		if travel > 0 {
			top = (y*(b.total-b.visible) + travel/2) / travel
		}
		r.setScrollTop(pane, top)
		return true
	}
	if !pt.In(b.track) {
		return false
	}
	// Several wheel events can precede a paint, including events that moved
	// from the body onto its scrollbar. Geometry is still valid, but the
	// last-painted top is no longer necessarily the current scroll position.
	switch pane {
	case "modal":
		b.top = r.model.modal.top
	case "panel":
		if p := r.focusedOwnWindow(); p != nil {
			b.top = p.panel.modal.top
		}
	default:
		s := r.workspace().viewState(r.workspace().inspector.target)
		b.top = s.top
		if s.follow {
			b.top = max(0, b.total-b.visible)
		}
	}
	switch e.ID {
	case "<MouseWheelUp>":
		r.setScrollTop(pane, b.top-3)
		return true
	case "<MouseWheelDown>":
		r.setScrollTop(pane, b.top+3)
		return true
	case "<MouseLeft>":
		if pt.In(b.thumb) {
			r.scrollDrag = scrollDragState{pane, pt.Y - b.thumb.Min.Y}
		} else {
			delta := max(1, b.visible-1)
			if pt.Y < b.thumb.Min.Y {
				delta = -delta
			}
			r.setScrollTop(pane, b.top+delta)
		}
		return true
	}
	return false
}
