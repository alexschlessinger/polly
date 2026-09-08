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
// seated; there is no bar without a frame.
func (r *managedREPL) setInspectorScrollbar(l frameLayout) {
	r.inspectorScrollbar = scrollbar{}
	if !r.workspace().inspector.open || l.chrome.frame.Empty() {
		return
	}
	_, body, track := l.chrome.split(r.inspectorHeaderRows)
	top := r.inspectorW.TopRow
	if r.inspectorW.PinBottom {
		top = max(0, len(r.inspectorW.Rows)-body.Dy())
	}
	r.inspectorScrollbar = newScrollbar(track, len(r.inspectorW.Rows), body.Dy(), top)
	r.inspectorScrollbar.hover = r.mousePositionKnown && r.mousePosition.In(track)
	r.inspectorScrollbar.dragging = r.scrollDrag.pane == "inspector"
}

func (r *managedREPL) setScrollTop(pane string, top int) {
	switch pane {
	case "inspector":
		b := r.inspectorScrollbar
		s := r.workspace().viewState(r.workspace().inspector.target)
		s.top = max(0, min(top, b.total-b.visible))
		s.follow = s.top >= max(0, b.total-b.visible)
		// Like inspectorScroll: only a follow position marks output as seen.
		if s.follow {
			s.lastRows = b.total
		}
	case "modal":
		if m := r.model.modal; m != nil {
			b := r.modalScrollbar
			m.top = max(0, min(top, b.total-b.visible))
			m.selected = max(m.top, min(m.selected, m.top+b.visible-1))
		}
	}
}

// Called with the main model lock held before pane/editor dispatch.
func (r *managedREPL) handleScrollbar(e ui.Event, modal bool) bool {
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
	bars := []struct {
		name string
		bar  scrollbar
	}{{"inspector", r.inspectorScrollbar}}
	if modal {
		bars = []struct {
			name string
			bar  scrollbar
		}{{"modal", r.modalScrollbar}}
	}
	for _, v := range bars {
		b := v.bar
		if b.thumb.Empty() {
			continue
		}
		if r.scrollDrag.pane == v.name && e.ID == "<MouseLeft>" {
			travel := b.track.Dy() - b.thumb.Dy()
			y := max(0, min(travel, pt.Y-b.track.Min.Y-r.scrollDrag.offset))
			top := 0
			if travel > 0 {
				top = (y*(b.total-b.visible) + travel/2) / travel
			}
			r.setScrollTop(v.name, top)
			return true
		}
		if !pt.In(b.track) {
			continue
		}
		switch e.ID {
		case "<MouseWheelUp>":
			r.setScrollTop(v.name, b.top-3)
			return true
		case "<MouseWheelDown>":
			r.setScrollTop(v.name, b.top+3)
			return true
		case "<MouseLeft>":
			if pt.In(b.thumb) {
				r.scrollDrag = scrollDragState{v.name, pt.Y - b.thumb.Min.Y}
			} else {
				delta := max(1, b.visible-1)
				if pt.Y < b.thumb.Min.Y {
					delta = -delta
				}
				r.setScrollTop(v.name, b.top+delta)
			}
			return true
		}
	}
	return false
}
