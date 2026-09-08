package main

import (
	"image"
	"time"

	ui "github.com/metaspartan/gotui/v5"
)

// Inspector chrome: the frame, its geometry, and the layer that paints it.
// Every rectangle the chrome needs is derived once per frame from the layout,
// so no caller re-derives an inset.

const (
	// inspectorMinWidth is the readable minimum for either pane of a split.
	inspectorMinWidth = 50
	// splitThreshold is the narrowest terminal that shows both panes.
	splitThreshold = 120
)

// chromeGeometry holds absolute screen rectangles for one frame.
type chromeGeometry struct {
	// main is the conversation pane; empty while the inspector fills the width.
	main image.Rectangle
	// frame is the inspector frame including its borders; empty when the
	// inspector is closed or drawn plain.
	frame image.Rectangle
	// inner is the frame interior: the header rows and the body beneath them.
	inner image.Rectangle
	// divider is the frame's left edge over the inner rows in split mode: the
	// drag target for resizing.
	divider image.Rectangle
	// edge is the frame's right edge over the inner rows: the scrollbar track.
	edge image.Rectangle
	// plain marks an inspector that is open but unframed because the region
	// is too small for borders.
	plain bool
	// joined marks a frame whose bottom border sits on the composer rule, so
	// the interior keeps the transcript region's last row.
	joined bool
}

// split divides the interior into the header, the body beneath it, and the
// scrollbar track beside the body.
func (g chromeGeometry) split(headerRows int) (header, body, track image.Rectangle) {
	if g.inner.Empty() {
		return
	}
	rows := max(0, min(headerRows, g.inner.Dy()))
	header = image.Rect(g.inner.Min.X, g.inner.Min.Y, g.inner.Max.X, g.inner.Min.Y+rows)
	body = image.Rect(g.inner.Min.X, header.Max.Y, g.inner.Max.X, g.inner.Max.Y)
	if !g.edge.Empty() {
		track = image.Rect(g.edge.Min.X, body.Min.Y, g.edge.Max.X, body.Max.Y)
	}
	return header, body, track
}

// splitLimits bounds the divider column so both panes keep inspectorMinWidth
// readable columns; the inspector also spends two columns on its borders.
func splitLimits(width int) (lo, hi int) {
	return inspectorMinWidth, width - inspectorMinWidth - 2
}

// splitColumn is the divider column for a split of the given width.
func (r *managedREPL) splitColumn(width int) int {
	ratio := r.inspectorRatio
	if ratio == 0 {
		ratio = .7
	}
	lo, hi := splitLimits(width)
	return max(lo, min(hi, int(float64(width-1)*ratio)))
}

// chromeGeometryFor derives the chrome for a transcript region of the given
// width that starts at row top and spans rows. When the composer rule sits
// directly under the region (joined), the frame's bottom border lands on the
// rule row instead of spending a region row. It reads loop-owned state only,
// so it is safe without the model lock.
func (r *managedREPL) chromeGeometryFor(width, top, rows int, joined bool) chromeGeometry {
	g := chromeGeometry{}
	region := image.Rect(0, top, width, top+rows)
	open, maximized := false, false
	if len(r.tabs) > 0 {
		i := &r.workspace().inspector
		open, maximized = i.open, i.maximized
	}
	if !open {
		g.main = region
		return g
	}
	// Two border rows plus at least one content row; anything shorter would
	// spend the whole region on chrome.
	if rows < 3 || width < 24 {
		g.inner = region
		g.plain = true
		return g
	}
	x := 0
	if !maximized && width >= splitThreshold {
		x = r.splitColumn(width)
		g.main = image.Rect(0, top, x, top+rows)
	}
	bottom := top + rows
	if joined {
		g.joined = true
		bottom++
	}
	g.frame = image.Rect(x, top, width, bottom)
	g.inner = image.Rect(x+1, top+1, width-1, bottom-1)
	if x > 0 {
		g.divider = image.Rect(x, g.inner.Min.Y, x+1, g.inner.Max.Y)
	}
	g.edge = image.Rect(width-1, g.inner.Min.Y, width, g.inner.Max.Y)
	return g
}

// viewGeometryFor is the projection geometry for the inspector's content.
func (r *managedREPL) viewGeometryFor(g chromeGeometry, width int) viewGeometry {
	if !g.inner.Empty() {
		width = g.inner.Dx()
	}
	vg := viewGeometry{width: width}
	if r.images != nil {
		vg.cellWidth, vg.cellHeight = r.images.cellDimensions()
		vg.nativeImages = true
	}
	return vg
}

// inspectorGeometry is the projection geometry for a terminal of the given
// width, assuming the transcript region has room for the frame. Callers
// without a layout (refreshInspector, tests) use it; render corrects the
// width from the real layout.
func (r *managedREPL) inspectorGeometry(width int) viewGeometry {
	return r.viewGeometryFor(r.chromeGeometryFor(width, 0, 3, false), width)
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

// chromeLayer paints the frame and the scrollbar over the seated widgets.
type chromeLayer struct {
	ui.Drawable
	r   *managedREPL
	now time.Time
}

func (c *chromeLayer) Draw(buf *ui.Buffer) {
	c.Drawable.Draw(buf)
	c.r.orbit.draw(buf, c.now)
	c.r.inspectorScrollbar.draw(buf)
}

// inspectorActivity snapshots only visible work. A historical completed tool
// does not inherit the source conversation's currently-running status.
func (r *managedREPL) inspectorActivity(l frameLayout) (active, attention bool) {
	m := r.model
	m.mu.Lock()
	allowed := !m.quiet && !m.hidden && m.modal == nil && (!m.focusKnown || m.focused)
	m.mu.Unlock()
	i := &r.workspace().inspector
	if i.open && !l.chrome.inner.Empty() {
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

// refreshChrome prepares the frame for this paint: its perimeter, base colors,
// the drag grip, and the cells the scrollbar thumb covers. It returns the
// drawable to render, wrapped in the chrome layer when a frame exists.
func (r *managedREPL) refreshChrome(drawable ui.Drawable, l frameLayout, now time.Time) ui.Drawable {
	g := l.chrome
	if g.frame.Empty() {
		r.orbit.geometry(image.Rectangle{})
		r.orbit.active = false
		return drawable
	}
	r.orbit.geometry(g.frame)
	active, attention := r.inspectorActivity(l)
	r.orbit.active = active
	// The frame brightens to the text color while the inspector owns the
	// keys; a pending approval's attention color wins over both.
	base := chromeColor("muted")
	if r.inspectorFocused() {
		base = ui.ColorClear
	}
	if attention {
		base = chromeColor("active")
	}
	hover := r.inspectorDragging || r.mousePositionKnown && r.mousePosition.In(g.divider)
	grip := image.Pt(g.divider.Min.X, g.divider.Min.Y+g.divider.Dy()/2)
	// On the composer rule the conversation's rule meets the frame's corner.
	corner := image.Pt(g.frame.Min.X, g.frame.Max.Y-1)
	cornerRune := '╰'
	if g.joined && !g.divider.Empty() {
		cornerRune = '┴'
	}
	for n := range r.orbit.cells {
		cell := &r.orbit.cells[n]
		if cell.base.Rune == '⋮' {
			cell.base.Rune = '│'
		}
		cell.base.Style = ui.NewStyle(base)
		switch {
		case !g.divider.Empty() && hover && cell.point == grip:
			cell.base.Rune = '⋮'
			cell.base.Style.Fg = chromeColor("accent")
		case cell.point == corner:
			cell.base.Rune = cornerRune
		}
	}
	// The thumb and the caller link own their cells; the frame never paints them.
	r.orbit.masks = []image.Rectangle{r.inspectorScrollbar.thumb, r.model.parentLink}
	return &chromeLayer{Drawable: drawable, r: r, now: now}
}
