package main

import (
	"image"
	"time"

	ui "github.com/metaspartan/gotui/v5"
)

// Pane bounds are absolute; transcript rows and disclosure x coordinates stay
// local to content. Header buttons, agent links and images use screen coordinates.
type haloPane struct{ header, content, track image.Rectangle }
type haloGeometry struct {
	outer, divider  image.Rectangle
	main, inspector haloPane
}

func (r *managedREPL) haloSplitColumn(width int) int {
	ratio := r.inspectorRatio
	if ratio == 0 {
		ratio = .7
	}
	return max(50, min(width-53, int(float64(width-1)*ratio)))
}

func (r *managedREPL) haloGeometry(l frameLayout) haloGeometry {
	g := haloGeometry{}
	if !l.halo {
		return g
	}
	x := 0
	i := &r.workspace().inspector
	if !i.maximized && l.width >= 120 {
		x = r.haloSplitColumn(l.width)
		g.main.content = image.Rect(0, l.logoRows, x, l.logoRows+l.transcriptHeight)
	}
	g.outer = image.Rect(x, l.logoRows, l.width, l.logoRows+l.transcriptHeight)
	g.inspector = haloPane{
		header:  image.Rect(x+1, g.outer.Min.Y+1, l.width-2, g.outer.Min.Y+2),
		content: image.Rect(x+1, g.outer.Min.Y+2, l.width-2, g.outer.Max.Y-1),
		track:   image.Rect(l.width-2, g.outer.Min.Y+2, l.width-1, g.outer.Max.Y-1),
	}
	if x > 0 {
		g.divider = image.Rect(x, g.outer.Min.Y+1, x+1, g.outer.Max.Y-1)
	}
	return g
}

// A fixed geometry group avoids Flex's extra insets in framed layouts.
type frameGroup struct {
	ui.Block
	items []ui.Drawable
}

func (g *frameGroup) Draw(buf *ui.Buffer) {
	for _, item := range g.items {
		item.Draw(buf)
	}
}

func setWidgetRect(w ui.Drawable, rect image.Rectangle) {
	w.SetRect(rect.Min.X, rect.Min.Y, rect.Max.X, rect.Max.Y)
}

func (r *managedREPL) layoutHalo(l frameLayout) {
	g := r.haloGeometry(l)
	r.haloBounds = g
	group := &frameGroup{Block: *ui.NewBlock()}
	noBorder(&group.Block)
	group.SetRect(0, 0, l.width, l.height)
	add := func(w ui.Drawable, rect image.Rectangle) {
		if !rect.Empty() {
			setWidgetRect(w, rect)
			group.items = append(group.items, w)
		}
	}
	add(r.logoW, image.Rect(0, 0, l.width, l.logoRows))
	add(r.transcriptW, g.main.content)
	r.mainTranscriptBounds = g.main.content
	if !g.outer.Empty() {
		top := g.inspector.header.Min.Y
		header := image.Rect(g.inspector.content.Min.X, top, g.inspector.content.Max.X, top+r.inspectorHeaderRows)
		body := image.Rect(header.Min.X, header.Max.Y, header.Max.X, g.outer.Max.Y-1)
		r.haloBounds.inspector.header = header
		r.haloBounds.inspector.content = body
		r.haloBounds.inspector.track.Min.Y = body.Min.Y
		add(r.inspectorHeaderW, header)
		add(r.inspectorW, body)
		r.inspectorBounds = image.Rect(header.Min.X, header.Min.Y, header.Max.X, body.Max.Y)
	}
	r.inspectorDivider = g.divider
	y := g.outer.Max.Y
	if l.dockRows > 0 {
		add(r.turnDockW, image.Rect(0, y, l.width, y+1))
		y++
	}
	if l.dividerRows > 0 {
		add(r.dividerW, image.Rect(0, y, l.width, y+1))
	}
	add(r.inputW, image.Rect(0, l.composerRow(0), l.width, l.composerRow(l.inputRows)))
	if l.statusRows > 0 {
		add(r.statusW, image.Rect(0, l.height-1, l.width, l.height))
	}
	r.rootFlex = group
}

type haloChromeLayer struct {
	ui.Drawable
	r   *managedREPL
	now time.Time
}

func (c *haloChromeLayer) Draw(buf *ui.Buffer) {
	c.Drawable.Draw(buf)
	r := c.r
	r.haloOrbit.draw(buf, c.now)
	r.inspectorScrollbar.draw(buf)
}

// Snapshot only visible work. A historical completed tool does not inherit
// the source conversation's currently-running status.
func (r *managedREPL) haloActivity() (active, attention bool) {
	m := r.model
	m.mu.Lock()
	allowed := !m.quiet && !m.hidden && m.modal == nil && (!m.focusKnown || m.focused)
	m.mu.Unlock()
	i := &r.workspace().inspector
	if i.open && !r.inspectorBounds.Empty() {
		if tab := r.inspectionTab(i.target); tab != nil {
			m = tab.model
			m.mu.Lock()
			busy := m.busy
			switch i.target.kind {
			case toolViewKind:
				busy = false
				for _, t := range m.inspections.tools {
					if t.key == i.target.item {
						busy = m.busy && !t.complete
						break
					}
				}
			case thoughtViewKind:
				busy = false
				for _, t := range m.inspections.thoughts {
					if t.key == i.target.item {
						busy = m.busy && !t.complete
						break
					}
				}
			}
			active = active || busy
			attention = attention || m.approval != nil
			m.mu.Unlock()
		}
	}
	return active && allowed && !attention, attention
}

func (r *managedREPL) haloFrame(drawable ui.Drawable, l frameLayout, now time.Time, modal bool) ui.Drawable {
	if !r.haloEnabled() {
		return drawable
	}
	if r.themeW == nil {
		r.themeW = &themeLayer{}
	}
	t := r.themeW
	t.halo = true
	t.colors = ui.DefaultBackend.Screen.Colors()
	t.panes = nil
	t.modal = nil
	if l.halo {
		g := r.haloBounds
		t.panes = []image.Rectangle{g.outer}
		r.haloOrbit.geometry(g.outer)
		active, attention := r.haloActivity()
		r.haloOrbit.active = active
		for n := range r.haloOrbit.cells {
			cell := &r.haloOrbit.cells[n]
			if cell.base.Rune == '⋮' {
				cell.base.Rune = '│'
			}
			r.haloOrbit.cells[n].base.Style.Fg = haloPalette.edge
			if attention {
				r.haloOrbit.cells[n].base.Style.Fg = haloPalette.attention
			}
			if !g.divider.Empty() && cell.point == image.Pt(g.divider.Min.X, g.divider.Min.Y+g.divider.Dy()/2) {
				cell.base.Rune = '⋮'
				if r.inspectorDragging || r.mousePositionKnown && r.mousePosition.In(g.divider) {
					cell.base.Style.Fg = haloPalette.accent
				}
			}
		}
		drawable = &haloChromeLayer{Drawable: drawable, r: r, now: now}
	} else {
		r.haloOrbit.geometry(image.Rectangle{})
		r.haloOrbit.active = false
		if r.workspace().inspector.open {
			t.panes = []image.Rectangle{r.inspectorBounds}
		}
	}
	t.Drawable = drawable
	if modal {
		t.modal = r.modalW
	}
	return t
}
