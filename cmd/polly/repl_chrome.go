package main

import (
	"image"

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
	// width is the terminal width the geometry was derived for.
	width int
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
	// close is the close button's cell on the frame's top border, beside the
	// top-right corner.
	close image.Rectangle
	// plain marks an inspector that is open but unframed because the region
	// is too small for borders.
	plain bool
	// joined marks a frame whose bottom border sits on the composer rule, so
	// the interior keeps the transcript region's last row.
	joined bool
	// floating marks a frame that stands over the conversation instead of
	// beside it (see repl_pane_own.go): main is then the whole region.
	floating bool
	// pin is the mark on the frame's top border that docks a floating frame
	// beside the conversation, or floats a docked one.
	pin image.Rectangle
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
		ratio = .6
	}
	lo, hi := splitLimits(width)
	return max(lo, min(hi, int(float64(width-1)*ratio)))
}

// framed reports whether the inspected view is open in the frame polly paints
// beside the conversation. Everything in this file hangs on it: a view shown
// on a screen of its own (viewDetached) is open with no frame, and the main
// screen is laid out, painted and clicked as if it had no inspector.
func (r *managedREPL) framed() bool {
	return len(r.tabs) > 0 && r.workspace().inspector.open && !r.viewDetached
}

// handleFramedEvent applies a main-screen event to the framed view: the keys
// it holds while focused or searching, the frame's divider and border, and
// clicks on its content. Caller holds r.model.mu.
func (r *managedREPL) handleFramedEvent(e ui.Event) bool {
	i := &r.workspace().inspector
	if r.viewSearchKey(e) {
		return true
	}
	// Search and approval own Escape first: cancel the search, deny the call.
	// A focused inspector gives the keys back before it closes.
	if e.ID == "<Escape>" && !r.model.hist.searching && r.model.approval == nil {
		if i.focused {
			i.focused = false
			i.keyboardAction = ""
			return true
		}
		r.closeInspector()
		return true
	}
	if i.focused && e.Type == ui.KeyboardEvent {
		if e.ID == "<M-p>" && r.panes.own.pins {
			r.toggleDock(floatView)
			return true
		}
		if e.ID == "<Tab>" {
			// On to the panel after this one, or back to the composer.
			r.tabToNextPanel(floatView, false)
			return true
		}
		// Typing means the composer: hand the keys back and let the rune land.
		if _, ok := printableRune(e); ok {
			i.keyboardAction = ""
			i.focused = false
		}
	}
	if r.handleFocusedNavigation(e) {
		return true
	}
	if e.Type != ui.MouseEvent {
		return false
	}
	mouse, ok := e.Payload.(ui.Mouse)
	if !ok {
		return false
	}
	point := image.Pt(mouse.X, mouse.Y)
	if e.ID == "<MouseLeft>" && point.In(r.chrome.divider) {
		r.inspectorDragging = true
		return true
	}
	if r.inspectorDragging {
		if e.ID == "<MouseRelease>" {
			r.inspectorDragging = false
			return true
		}
		if e.ID == "<MouseLeft>" {
			r.inspectorRatio = float64(mouse.X) / float64(max(1, r.paintedWidth()-1))
			return true
		}
	}
	if point.In(r.chrome.frame) && !point.In(r.chrome.inner) {
		// The frame belongs to the inspector, including in full-width
		// mode; clicks must not reach the hidden conversation underneath.
		if e.ID == "<MouseLeft>" && point.In(r.chrome.pin) {
			r.toggleDock(floatView)
			return true
		}
		switch e.ID {
		case "<MouseWheelUp>":
			r.inspectorScroll(-3)
		case "<MouseWheelDown>":
			r.inspectorScroll(3)
		case "<MouseLeft>":
			if point.In(r.model.todoLink) {
				return false // The task marks belong to the composer, not the frame.
			}
			if point.In(r.chrome.close) {
				r.inspectorAction("close")
			}
		}
		return true
	}
	// Nothing under a floating frame is there to be clicked.
	return r.viewContentMouse(e, point) || r.chrome.floating && point.In(r.chrome.frame)
}

func (r *managedREPL) resizeInspector(direction int) {
	i := &r.workspace().inspector
	width := r.paintedWidth()
	if !r.framed() || r.chrome.floating || i.maximized || width < splitThreshold {
		return
	}
	// Start from the displayed divider, including after a drag or terminal
	// resize clamps a pane to its minimum. A width control must never reverse.
	lo, hi := splitLimits(width)
	left := max(lo, min(hi, r.splitColumn(width)-direction*max(1, (width-1)/20)))
	// Store the middle of this column's interval so float rounding does not
	// place the divider one column short when geometry converts back to int.
	r.inspectorRatio = (float64(left) + .5) / float64(width-1)
}

// followPointerFocus moves the keys to the pane under the pointer: the
// inspector while the pointer is over its frame, the composer otherwise.
// Tab and Escape still move focus by hand until the pointer moves again. A
// divider or scrollbar drag, a dialog, and an inspector search hold focus
// where it is, and a frame that has not been painted yet cannot be pointed
// at. Caller must hold r.model.mu.
func (r *managedREPL) followPointerFocus(p image.Point) {
	i := &r.workspace().inspector
	if !r.framed() || i.searching || r.model.modal != nil || r.inspectorDragging || r.scrollDrag.pane != "" || r.chrome.frame.Empty() {
		return
	}
	if focused := p.In(r.chrome.frame) && !p.In(r.model.todoLink); focused != i.focused {
		i.focused = focused
		if focused {
			r.yieldPanelKeys()
		}
		// The frame brightens and the composer cursor hides with focus.
		r.chromeHoverChanged = true
	}
}

// chromeGeometryFor derives the chrome for a transcript region of the given
// width that starts at row top and spans rows. When the composer rule sits
// directly under the region (joined), the frame's bottom border lands on the
// rule row instead of spending a region row. It reads loop-owned state only,
// so it is safe without the model lock.
func (r *managedREPL) chromeGeometryFor(width, top, rows int, joined bool) chromeGeometry {
	g := chromeGeometry{width: width}
	region := image.Rect(0, top, width, top+rows)
	open, maximized := r.framed(), false
	if open {
		maximized = r.workspace().inspector.maximized
	}
	own := r.panes.own
	if !open {
		// Panels docked beside the conversation narrow it.
		g.main = image.Rect(0, top, width-own.dock, top+rows)
		return g
	}
	// Two border rows plus at least one content row; anything shorter would
	// spend the whole region on chrome.
	if rows < 3 || width < 24 {
		g.inner = region
		g.plain = true
		return g
	}
	x, above, below := 0, 0, 0
	if !maximized && width >= splitThreshold {
		if r.viewFloats() {
			// A window over the conversation, between the windows above and
			// below it in the same column, and beside any docked panels.
			edge := width - own.dock
			wide := own.floatWidth
			if wide <= 0 {
				wide = viewFloatWidth(edge)
			}
			frame := image.Rect(edge-wide, top+own.viewTop, edge, top+rows-own.viewBottom)
			if frame.Dy() < ownWindowMinRows {
				frame.Min.Y, frame.Max.Y = top, top+rows
			}
			g.main, g.frame, g.floating = image.Rect(0, top, edge, top+rows), frame, true
			g.inner = image.Rect(frame.Min.X+1, frame.Min.Y+1, frame.Max.X-1, frame.Max.Y-1)
			g.edge = image.Rect(frame.Max.X-1, g.inner.Min.Y, frame.Max.X, g.inner.Max.Y)
			g.close = image.Rect(frame.Max.X-2, frame.Min.Y, frame.Max.X-1, frame.Min.Y+1)
			if own.pins {
				g.pin = pinRect(g.close)
			}
			return g
		}
		x = r.splitColumn(width)
		g.main = image.Rect(0, top, x, top+rows)
		// Panels docked in the view's column stand above and below it.
		if rows-own.viewTop-own.viewBottom >= ownWindowMinRows {
			above, below = own.viewTop, own.viewBottom
		}
	}
	bottom := top + rows - below
	if joined && below == 0 {
		g.joined = true
		bottom++
	}
	top += above
	g.frame = image.Rect(x, top, width, bottom)
	g.inner = image.Rect(x+1, top+1, width-1, bottom-1)
	if x > 0 {
		g.divider = image.Rect(x, g.inner.Min.Y, x+1, g.inner.Max.Y)
	}
	g.edge = image.Rect(width-1, g.inner.Min.Y, width, g.inner.Max.Y)
	g.close = image.Rect(width-2, top, width-1, top+1)
	if x > 0 && own.pins {
		g.pin = pinRect(g.close)
	}
	return g
}

// paintedWidth is the terminal width of the last painted frame, which is
// what the divider's controls resize against; before the first paint it
// falls back to the terminal.
func (r *managedREPL) paintedWidth() int {
	if r.chrome.width > 0 {
		return r.chrome.width
	}
	width, _ := ui.TerminalDimensions()
	return width
}

// viewGeometryFor is the projection geometry for the inspector's content.
func (r *managedREPL) viewGeometryFor(g chromeGeometry, width int) viewGeometry {
	if !g.inner.Empty() {
		width = g.inner.Dx()
	}
	vg := viewGeometry{width: width}
	if r.images != nil {
		vg.cellWidth, vg.cellHeight = r.images.CellDimensions()
		vg.nativeImages = true
	}
	return vg
}

// frameSearching reports whether the framed view's search prompt holds the
// main screen's keys.
func (r *managedREPL) frameSearching() bool {
	return r.framed() && r.workspace().inspector.searching
}

// projectionGeometry is what the inspected view is wrapped for before it is
// painted: the frame's interior on a terminal of the given width, or the
// surface a detached view was last placed on.
func (r *managedREPL) projectionGeometry(terminalWidth int) viewGeometry {
	if r.viewDetached {
		return viewGeometry{width: max(1, r.surface.inner.Dx())}
	}
	return r.inspectorGeometry(terminalWidth)
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
	r *managedREPL
	// close is drawn in the accent color, the palette's clickable slot.
	close image.Rectangle
	// over is the view's widgets when its frame floats: they are drawn here,
	// over a conversation that has been drawn whole.
	over []ui.Drawable
}

func (c *chromeLayer) Draw(buf *ui.Buffer) {
	c.Drawable.Draw(buf)
	if g := c.r.chrome; g.floating {
		for y := g.frame.Min.Y; y < g.frame.Max.Y; y++ {
			for x := g.frame.Min.X; x < g.frame.Max.X; x++ {
				buf.SetCell(ui.Cell{Rune: ' ', Style: ui.StyleClear}, image.Pt(x, y))
			}
		}
		for _, item := range c.over {
			item.Draw(buf)
		}
	}
	if g := c.r.chrome; g.joined && g.main.Empty() {
		// A full-width inspector shares the composer's rule row. Keep its
		// corner glyphs, but preserve the composer's short color cue beneath.
		for n := range c.r.border.cells {
			cell := &c.r.border.cells[n]
			if cell.point.Y == g.frame.Max.Y-1 && cell.point.X < composerBoundaryAccentCols {
				cell.base.Style = buf.GetCell(cell.point).Style
			}
		}
	}
	c.r.border.draw(buf)
	if !c.close.Empty() {
		buf.SetCell(ui.Cell{Rune: '×', Style: ui.NewStyle(chromeColor("accent"))}, c.close.Min)
	}
	if pin := c.r.chrome.pin; !pin.Empty() {
		mark := pinFloatMark
		if c.r.chrome.floating {
			mark = pinDockMark
		}
		buf.SetString(mark, ui.NewStyle(chromeColor("accent")), pin.Min)
	}
	c.r.surface.scrollbar.draw(buf)
}

// inspectorAttention reports whether the inspected conversation needs approval.
func (r *managedREPL) inspectorAttention(l frameLayout) bool {
	i := &r.workspace().inspector
	if i.open && !l.chrome.inner.Empty() {
		if tab := r.inspectionTab(i.target); tab != nil {
			tab.model.mu.Lock()
			defer tab.model.mu.Unlock()
			return tab.model.approval != nil
		}
	}
	return false
}

// refreshChrome prepares the frame for this paint: its perimeter, base colors,
// the drag grip, and the cells the scrollbar thumb covers. It returns the
// drawable to render, wrapped in the chrome layer when a frame exists.
func (r *managedREPL) refreshChrome(drawable ui.Drawable, l frameLayout) ui.Drawable {
	g := l.chrome
	if g.frame.Empty() {
		r.border.geometry(image.Rectangle{})
		return drawable
	}
	r.border.geometry(g.frame)
	attention := r.inspectorAttention(l)
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
	for n := range r.border.cells {
		cell := &r.border.cells[n]
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
	// Keep controls painted into the shared composer rule, including the
	// spaces that separate the task marks from the rule.
	r.border.masks = []image.Rectangle{r.surface.scrollbar.thumb, r.model.parentLink, g.close}
	if !g.pin.Empty() {
		r.border.masks = append(r.border.masks, image.Rect(g.pin.Min.X-1, g.pin.Min.Y, g.pin.Max.X+1, g.pin.Max.Y))
	}
	if link := r.model.todoLink; !link.Empty() {
		r.border.masks = append(r.border.masks, image.Rect(link.Min.X-1, link.Min.Y, link.Max.X+1, link.Max.Y))
	}
	layer := &chromeLayer{Drawable: drawable, r: r, close: g.close}
	if g.floating {
		layer.over = r.viewWidgets()
	}
	return layer
}
