package main

import (
	"image"

	ui "github.com/metaspartan/gotui/v5"
)

// A reserved rail never changes wrapping when content grows or shrinks.
type haloScrollbar struct {
	track, thumb        image.Rectangle
	total, visible, top int
	hover, dragging     bool
}
type haloScrollDrag struct {
	pane   string
	offset int
}

func newHaloScrollbar(track image.Rectangle, total, visible, top int) haloScrollbar {
	b := haloScrollbar{track: track, total: total, visible: visible, top: max(0, min(top, total-visible))}
	if track.Empty() || total <= visible || visible <= 0 {
		return b
	}
	size := max(1, min(track.Dy(), track.Dy()*visible/max(1, total)))
	y := track.Min.Y + (track.Dy()-size)*b.top/max(1, total-visible)
	b.thumb = image.Rect(track.Min.X, y, track.Max.X, y+size)
	return b
}

func (b haloScrollbar) draw(buf *ui.Buffer) {
	if b.thumb.Empty() {
		return
	}
	for y := b.track.Min.Y; y < b.track.Max.Y; y++ {
		pt := image.Pt(b.track.Min.X, y)
		ch, fg := '│', haloPalette.edge
		if pt.In(b.thumb) {
			ch, fg = '┃', haloPalette.muted
			if b.hover || b.dragging {
				fg = haloPalette.accent
			}
		}
		buf.SetCell(ui.Cell{Rune: ch, Style: ui.NewStyle(fg)}, pt)
	}
}

func (r *managedREPL) setHaloScrollbars() {
	g := r.haloBounds
	top := r.inspectorW.TopRow
	if r.inspectorW.PinBottom {
		top = max(0, len(r.inspectorW.Rows)-g.inspector.content.Dy())
	}
	r.inspectorScrollbar = newHaloScrollbar(g.inspector.track, len(r.inspectorW.Rows), g.inspector.content.Dy(), top)
	r.inspectorScrollbar.hover = r.mousePositionKnown && r.mousePosition.In(r.inspectorScrollbar.track)
	r.inspectorScrollbar.dragging = r.scrollDrag.pane == "inspector"
}

func (r *managedREPL) setHaloScrollTop(pane string, top int) {
	switch pane {
	case "inspector":
		b := r.inspectorScrollbar
		s := r.workspace().viewState(r.workspace().inspector.target)
		s.top = max(0, min(top, b.total-b.visible))
		s.follow = s.top >= max(0, b.total-b.visible)
		s.lastRows = b.total
	case "modal":
		if m := r.model.modal; m != nil {
			b := r.modalScrollbar
			m.top = max(0, min(top, b.total-b.visible))
			m.selected = max(m.top, min(m.selected, m.top+b.visible-1))
		}
	}
}

// Called with the main model lock held before pane/editor dispatch.
func (r *managedREPL) handleHaloScrollbar(e ui.Event, modal bool) bool {
	if !r.haloEnabled() || e.Type != ui.MouseEvent {
		return false
	}
	mouse, ok := e.Payload.(ui.Mouse)
	if !ok {
		return false
	}
	pt := image.Pt(mouse.X, mouse.Y)
	if e.ID == "<MouseRelease>" {
		r.scrollDrag = haloScrollDrag{}
		return false
	}
	bars := []struct {
		name string
		bar  haloScrollbar
	}{{"inspector", r.inspectorScrollbar}}
	if modal {
		bars = []struct {
			name string
			bar  haloScrollbar
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
			r.setHaloScrollTop(v.name, top)
			return true
		}
		if !pt.In(b.track) {
			continue
		}
		switch e.ID {
		case "<MouseWheelUp>":
			r.setHaloScrollTop(v.name, b.top-3)
			return true
		case "<MouseWheelDown>":
			r.setHaloScrollTop(v.name, b.top+3)
			return true
		case "<MouseLeft>":
			if pt.In(b.thumb) {
				r.scrollDrag = haloScrollDrag{v.name, pt.Y - b.thumb.Min.Y}
			} else {
				delta := max(1, b.visible-1)
				if pt.Y < b.thumb.Min.Y {
					delta = -delta
				}
				r.setHaloScrollTop(v.name, b.top+delta)
			}
			return true
		}
	}
	return false
}
