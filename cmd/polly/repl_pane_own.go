package main

import (
	"context"
	"image"
	"slices"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/termimg"
	ui "github.com/metaspartan/gotui/v5"
)

// Polly's own screen as a host. The panels that float in a host's windows
// (see repl_pane_float.go) and the questions a host asks in its list (see
// repl_pane_ask.go) are opened by the same calls wherever polly runs. With no
// host outside its screen, or one that could not do it, polly is the host: it
// draws the windows over the conversation and the list in a dialog, as near
// to what the outside host shows as a terminal's cells come.
//
// A window here is the panel's dialog in a box at the right of the
// conversation: several at once, in one column where none covers another,
// standing while the conversation goes on. It has no terminal of its own, so
// it borrows polly's keys rather than having its own. A window takes them
// when it opens and when it is clicked or scrolled, and holds them as the
// host's focused window (paneHost.focused), which is not a dialog: a dialog
// that opens takes them from it. The arrows, Enter, Tab and Escape are its
// own, and Escape closes it. Tab moves on to the next open window once the
// window's own action is selected, and Alt-P docks it or floats it. Typing
// hands the keys back to the composer, where the rune lands, and so does a
// click anywhere else; the window stays up either way.

// ownWindowMinRows is a box with one row inside its border, and
// ownPinMinWidth the narrowest window whose border has room for the mark
// that docks it beside its title.
const (
	ownWindowMinRows = 3
	ownPinMinWidth   = 30
	// ownMinConversationCols keeps at least this many conversation columns
	// uncovered when floating windows coexist beside a docked column on a
	// 120-140 column terminal.
	ownMinConversationCols = 32
)

// The marks on a window's border that dock it beside the conversation and
// float it over it again: a pane at the side, and a window with its shadow.
// Each is one cell wide in every terminal.
const (
	pinDockMark  = "◨"
	pinFloatMark = "❐"
)

// pinRect is where one of those marks goes on a window's top border: a cell
// clear of the mark that closes it.
func pinRect(close image.Rectangle) image.Rectangle {
	return image.Rect(close.Min.X-2, close.Min.Y, close.Min.X-1, close.Max.Y)
}

// viewFloats reports whether the inspected view stands over the conversation
// in a window of polly's own, rather than in the frame beside it: it does
// unless the user docked it (see repl_pane_dock.go). It reads loop-owned
// state only.
func (r *managedREPL) viewFloats() bool {
	h := &r.panes
	return r.framed() && h.docks != nil && !h.docks[floatView]
}

// viewFloatWidth is how wide that window is on a terminal of width columns:
// what a host's window for the view asks for.
func viewFloatWidth(width int) int {
	share := min(viewWant.most[0], max(viewWant.least[0], float64(viewWant.columns)/float64(max(1, width))))
	return max(inspectorMinWidth+2, min(width, int(share*float64(width))))
}

// viewWidgets is the view's header and body, seated on its surface.
func (r *managedREPL) viewWidgets() []ui.Drawable {
	var widgets []ui.Drawable
	header, body, _ := r.surface.split()
	if !header.Empty() {
		setWidgetRect(r.surface.header, header)
		widgets = append(widgets, r.surface.header)
	}
	if !body.Empty() {
		setWidgetRect(r.surface.body, body)
		widgets = append(widgets, r.surface.body)
	}
	return widgets
}

// ownWindow paints one window: its dialog's page, and the mark on its border
// that closes it.
type ownWindow struct {
	*modalParagraph
	close image.Rectangle
	// mark docks the window, or floats it, and pin is where it goes.
	pin  image.Rectangle
	mark string
}

func (w *ownWindow) Draw(buf *ui.Buffer) {
	w.modalParagraph.Draw(buf)
	if !w.close.Empty() {
		buf.SetCell(ui.Cell{Rune: '×', Style: ui.NewStyle(chromeColor("accent"))}, w.close.Min)
	}
	if !w.pin.Empty() {
		// The mark sits in the border, with a space either side of it.
		buf.SetString(" "+w.mark+" ", ui.NewStyle(chromeColor("accent")), image.Pt(w.pin.Min.X-1, w.pin.Min.Y))
	}
}

// hosts reports whether a host outside polly's screen has a window to give
// the panel kind: a pane where the user keeps that panel docked, a floating
// window otherwise. A host may have one and not the other.
func (h *paneHost) hosts(kind floatKind) bool {
	if _, ok := h.windows(); !ok {
		return false
	}
	if h.docks[kind] {
		return !h.dockless
	}
	return !h.floatless
}

// focusedOwnWindow returns the own-screen panel that currently owns keyboard
// focus, or nil when keys belong to a dialog, the inspector, or the composer.
func (r *managedREPL) focusedOwnWindow() *satellitePane {
	p := r.panes.focused
	if p == nil || !p.own || !r.panes.has(p) || p.panel == nil || p.panel.owner != r.model {
		return nil
	}
	return p
}

// focusedPanelModal returns the modal backing the focused own-screen panel,
// or nil when no own-screen panel holds focus.
func (r *managedREPL) focusedPanelModal() *replModal {
	if p := r.focusedOwnWindow(); p != nil {
		return p.panel.modal
	}
	return nil
}

// ownWindows is the modal panels polly draws itself, in canonical order from
// the top down.
func (h *paneHost) ownWindows() []*satellitePane {
	var windows []*satellitePane
	for _, kind := range panelOrder {
		if kind == floatView {
			continue
		}
		if p := h.floatOf(kind); p != nil && p.own {
			windows = append(windows, p)
		}
	}
	return windows
}

// ownWindowAt finds the window drawn under point: the one with the keys
// first, which is drawn last.
func (r *managedREPL) ownWindowAt(point image.Point) *satellitePane {
	windows := r.panes.ownWindows()
	if held := r.focusedOwnWindow(); held != nil {
		windows = append([]*satellitePane{held}, windows...)
	}
	for _, p := range windows {
		if point.In(p.rect) {
			return p
		}
	}
	return nil
}

// dialogOpen reports whether a blocking dialog owns the input.
// Caller holds the visible model's lock.
func (r *managedREPL) dialogOpen() bool {
	return r.model.modal != nil
}

// makeOwnWindow turns p into a window of polly's own screen. Caller holds the
// visible model's lock.
func (r *managedREPL) makeOwnWindow(p *satellitePane) {
	p.own, p.docked, p.loose = true, false, false
	p.panel.page = newModalParagraph()
	plainPanel(p.panel.modal)
}

// plainPanel takes the letter keys out of a panel's rows: in a window without
// a terminal of its own, letters belong to the composer.
func plainPanel(dialog *replModal) {
	if d := dialog.todo; d != nil && !d.plain {
		d.plain, d.painted = true, nil
	}
}

// giveKeys hands polly's keys to the own-screen window p, unless a blocking
// dialog has them or p is about a session that is not on screen. Caller holds
// the visible model's lock.
func (r *managedREPL) giveKeys(p *satellitePane) bool {
	if p == nil || !p.own || p.panel == nil || p.panel.owner != r.model || r.model.modal != nil {
		return false
	}
	r.panes.focused = p
	if r.framed() {
		r.workspace().inspector.focused = false
		r.workspace().inspector.keyboardAction = ""
	}
	r.panelScrollbar = p.panel.page.scrollbar
	if r.scrollDrag.pane == "panel" {
		r.scrollDrag = scrollDragState{}
	}
	return true
}

// yieldPanelKeys takes the keys back from any focused own-screen window,
// leaving the window open.
func (r *managedREPL) yieldPanelKeys() {
	if r.panes.focused == nil {
		return
	}
	r.panes.focused = nil
	r.panelScrollbar = scrollbar{}
	if r.scrollDrag.pane == "panel" {
		r.scrollDrag = scrollDragState{}
	}
}

// syncPanels keeps every panel about the session on screen and on the right
// host: every render starts here, rebuilding any panel whose session left the
// screen, and migrating an own-screen panel onto an external host when the
// user toggles docking on a floatless host (such as tmux < 3.8). Runs on the
// event loop with no lock held.
func (r *managedREPL) syncPanels() {
	h := &r.panes
	for _, p := range slices.Clone(h.floats) {
		panel := p.panel
		if panel.owner != r.model {
			if h.focused == p {
				r.yieldPanelKeys()
			}
			r.model.mu.Lock()
			panel.modal.wipe()
			panel.modal, panel.owner = r.floatPanelFor(panel.kind), r.model
			if p.own {
				plainPanel(panel.modal)
			}
			r.model.mu.Unlock()
		}
		// Mirror syncViewPane: if an own-screen panel is now eligible for an
		// external host (e.g. user docked it on a floatless host), promote it.
		if p.own && h.hosts(panel.kind) {
			r.model.mu.Lock()
			if h.focused == p {
				r.yieldPanelKeys()
			}
			p.own = false
			if d := panel.modal.todo; d != nil && d.plain {
				d.plain, d.painted = false, nil
			}
			page := newModalParagraph()
			noBorder(&page.Block)
			panel.page = page
			if !r.hostFloat(p, panelSpecs[panel.kind].label) {
				r.makeOwnWindow(p)
			}
			r.model.mu.Unlock()
		}
	}
}

// panelRows is how many rows a panel's dialog has to show at width.
func panelRows(m *replModal, width int) int {
	switch {
	case m.todo != nil:
		m.todo.refreshItems(m, width)
	case m.context != nil:
		m.context.refreshItems(m, width)
		if m.context.usageOpen {
			// The breakdown keeps the overview's height, and scrolls in it.
			return m.context.overviewRows
		}
	}
	return len(m.items)
}

// ownWindowRects places windows in a column at the right of region, so that
// none covers another: those at the top stack down from it and those at the
// bottom up from its last row, each as tall as it asks until they ask for
// more than room rows between them: then the tallest give way first (see
// floatLayout, which places a host's windows by the same rule).
func ownWindowRects(region image.Rectangle, widths, rows []int, bottom []bool, room int) []image.Rectangle {
	heights := make([]float64, len(rows))
	asked := 0.0
	for n, ask := range rows {
		heights[n] = float64(min(room, max(ownWindowMinRows, ask)))
		asked += heights[n]
	}
	if asked > float64(room) {
		squeezeHeights(heights, float64(room), false)
	}
	rects := make([]image.Rectangle, len(rows))
	top, floor := region.Min.Y, region.Max.Y
	place := func(n, y int) {
		rects[n] = image.Rect(region.Max.X-widths[n], y, region.Max.X, y+int(heights[n]))
	}
	for n := range rows {
		if !bottom[n] {
			place(n, top)
			top += int(heights[n])
		}
	}
	for n := len(rows) - 1; n >= 0; n-- {
		if bottom[n] {
			floor -= int(heights[n])
			place(n, floor)
		}
	}
	return rects
}

// ownColumnWidth is the one width the windows in the column take, so that
// their left edges line up: the widest any of their dialogs asks for of a
// region width columns wide, and no narrower than a view floating in the
// same column, which is view columns wide.
func ownColumnWidth(width int, windows []*satellitePane, view int) int {
	wide := min(view, width)
	for _, p := range windows {
		wide = max(wide, modalWidthForTerminal(width, p.panel.modal.width))
	}
	return wide
}

// ownLayout is where the windows of polly's own screen go in one frame.
type ownLayout struct {
	// dock is how many columns the docked panels take from the right of the
	// conversation, when the view is not docked there to set the width.
	dock int
	// floatWidth is the unified width of the floating column (covering both
	// modal panels and a floating view so their left edges always align).
	floatWidth int
	// viewTop and viewBottom are the rows the windows above and below the
	// view take in its column: the docked ones' beside a docked view, the
	// floating ones' over a floating one.
	viewTop, viewBottom int
	// rects is each window's place, and docked which of them are panes of
	// the column beside the conversation.
	rects  map[*satellitePane]image.Rectangle
	docked map[*satellitePane]bool
	// roomy says whether there is room to dock at all; pins says whether the
	// windows carry the dock/float mark.
	roomy, pins bool
}

// refreshOwnPanels refreshes live content (such as context snapshots) on
// own-screen panels before the frame's single layout pass so panel heights and
// chrome geometry are derived from the same snapshot. Caller holds r.model.mu.
func (r *managedREPL) refreshOwnPanels(now time.Time) {
	windows := r.panes.ownWindows()
	if len(windows) == 0 {
		return
	}
	// Sixel cannot composite the chart's partial alpha onto an unknown
	// terminal background. Keep the Unicode chart until the theme supplies
	// an opaque surface; Kitty and render surfaces preserve the alpha.
	_, bg := style.Surface()
	native := r.images != nil && (r.images.Protocol() != termimg.ProtocolSixel || bg.Valid())
	for _, p := range windows {
		if m := p.panel.modal; m.context != nil {
			r.refreshContext(m, now, native)
		}
	}
}

// layoutOwnWindows places the windows polly draws on a terminal width
// columns wide with rows of conversation, and remembers the outline of it
// for the view's frame (see chromeGeometryFor). A panel the user keeps docked
// is a pane of a column beside the conversation, which is that much
// narrower: the docked view's column where the view is docked, and otherwise
// one as wide as the panels ask. The other windows float over the
// conversation, at its right. Both columns are stacked by the same rule (see
// ownWindowRects), and a docked column is filled: by the view, or by its
// last panel. Caller holds the visible model's lock.
func (r *managedREPL) layoutOwnWindows(width, rows int) ownLayout {
	h := &r.panes
	own := ownLayout{rects: map[*satellitePane]image.Rectangle{}, docked: map[*satellitePane]bool{}}
	defer func() { h.own = own }()
	framed := r.framed()
	// A terminal with room for two columns can dock, unless the view has
	// taken all of it.
	roomy := width >= splitThreshold && rows >= 2*ownWindowMinRows && !(framed && r.workspace().inspector.maximized)
	own.roomy = roomy
	own.pins = roomy && h.docks != nil
	viewDocked, viewFloating := framed && roomy && !r.viewFloats(), framed && roomy && r.viewFloats()
	var docked, floating []*satellitePane
	for _, p := range h.ownWindows() {
		switch {
		case p.panel.owner != r.model:
			// Built for another session: the next frame's are placed.
		case roomy && h.docks[p.panel.kind]:
			docked = append(docked, p)
			own.docked[p] = true
		default:
			floating = append(floating, p)
		}
	}
	// stack places windows in column, all wide columns wide, and reports the
	// rows those at its top and at its bottom take.
	stack := func(windows []*satellitePane, column image.Rectangle, wide, room int) (top, bottom int) {
		widths, asks, low := make([]int, len(windows)), make([]int, len(windows)), make([]bool, len(windows))
		for n, p := range windows {
			widths[n] = wide
			asks[n], low[n] = panelRows(p.panel.modal, wide)+2, p.want.bottom
		}
		for n, rect := range ownWindowRects(column, widths, asks, low, room) {
			own.rects[windows[n]] = rect
			if low[n] {
				bottom += rect.Dy()
			} else {
				top += rect.Dy()
			}
		}
		return top, bottom
	}
	edge := width
	if viewDocked {
		edge = r.splitColumn(width)
	} else if len(docked) > 0 {
		own.dock = ownColumnWidth(width, docked, 0)
		edge = width - own.dock
	}
	if len(docked) > 0 {
		column, room := image.Rect(edge, 0, width, rows), rows
		if viewDocked {
			room = rows / 2
		}
		top, bottom := stack(docked, column, column.Dx(), room)
		if viewDocked {
			own.viewTop, own.viewBottom = top, bottom
		} else {
			// The column's last pane takes the rows nothing asked for.
			last := docked[len(docked)-1]
			rect := own.rects[last]
			if last.want.bottom {
				rect.Min.Y = top
			} else {
				rect.Max.Y = rows - bottom
			}
			own.rects[last] = rect
		}
	}
	if len(floating) > 0 || viewFloating {
		over, view, room := image.Rect(0, 0, edge, rows), 0, rows
		if viewFloating {
			view, room = viewFloatWidth(edge), rows/2
		}
		wide := ownColumnWidth(edge, floating, view)
		// When a docked column already narrows the conversation (edge < width),
		// cap the floating column so the conversation stays readable instead of
		// squeezing to a sliver on 120-140 column terminals.
		if edge < width {
			wide = min(wide, max(24, edge-ownMinConversationCols))
		}
		own.floatWidth = wide
		if len(floating) > 0 {
			top, bottom := stack(floating, over, wide, room)
			if viewFloating {
				own.viewTop, own.viewBottom = top, bottom
			}
		}
	}
	return own
}

// toggleDock docks a panel beside the conversation, or floats it over it
// again, and remembers which for the launches after this one. Caller holds
// the visible model's lock.
func (r *managedREPL) toggleDock(kind floatKind) {
	h := &r.panes
	if h.docks == nil {
		h.docks = map[floatKind]bool{}
	}
	h.docks[kind] = !h.docks[kind]
	if err := saveDockedPanels(h.docks); err != nil {
		r.model.appendNoticeLine("Docked panels not saved for later launches · " + err.Error())
	}
}

// frameOwnWindows readies the windows of polly's own screen for this frame
// using the single layout already computed by frameLayoutFor. It returns the
// layers to paint over the conversation, the cells they cover, and the images
// they place. Caller holds r.model.mu, and syncPanels/refreshOwnPanels have
// run.
func (r *managedREPL) frameOwnWindows(cellWidth, cellHeight int) (layers []ui.Drawable, covers []image.Rectangle, images []termimg.Placement) {
	windows := r.panes.ownWindows()
	if len(windows) == 0 {
		r.panelScrollbar = scrollbar{}
		return nil, nil, nil
	}
	own := r.panes.own
	held := r.focusedOwnWindow()
	if held == nil {
		r.panelScrollbar = scrollbar{}
	}
	var front *ownWindow
	for _, p := range windows {
		rect, docked := own.rects[p], own.docked[p]
		m, page := p.panel.modal, p.panel.page
		if rect.Dy() < ownWindowMinRows || rect.Dx() < 3 {
			p.rect, p.closeAt, p.pinAt = image.Rectangle{}, image.Rectangle{}, image.Rectangle{}
			m.bounds, m.listBounds = image.Rectangle{}, image.Rectangle{}
			continue
		}
		text := m.text(rect.Dy()-2, rect.Dx())
		// A floating dialog shorter than its place keeps to the edge it hangs
		// from; a docked one is a pane, as tall as its place.
		if spare := rect.Dy() - (strings.Count(text, "\n") + 3); spare > 0 && !docked {
			if p.want.bottom {
				rect.Min.Y += spare
			} else {
				rect.Max.Y -= spare
			}
		}
		page.refreshRoles()
		if p == held {
			// The window with the keys is the bright one.
			page.BorderStyle = ui.NewStyle(ui.ColorClear)
		}
		page.Text, page.Title = text, m.title
		page.titleNotice, page.titleNoticeRole = m.titleNotice, m.titleNoticeRole
		page.SetRect(rect.Min.X, rect.Min.Y, rect.Max.X, rect.Max.Y)
		m.bounds = rect
		listTop := page.Inner.Min.Y + m.bodyRows
		m.listBounds = image.Rect(page.Inner.Min.X, listTop, page.Inner.Max.X, min(page.Inner.Max.Y, listTop+m.visible))
		// The scrollbar rides the window's right border, like a dialog's.
		track := image.Rect(rect.Max.X-1, m.listBounds.Min.Y, rect.Max.X, m.listBounds.Max.Y)
		page.scrollbar = newScrollbar(track, len(m.filteredItems()), m.visible, m.top)
		page.scrollbar.hover = r.mousePositionKnown && r.mousePosition.In(track)
		if p == held {
			page.scrollbar.dragging = r.scrollDrag.pane == "panel"
			r.panelScrollbar = page.scrollbar
		}
		p.rect, p.closeAt, p.pinAt = rect, image.Rect(rect.Max.X-2, rect.Min.Y, rect.Max.X-1, rect.Min.Y+1), image.Rectangle{}
		p.docked = docked
		layer := &ownWindow{modalParagraph: page, close: p.closeAt}
		if own.pins && rect.Dx() >= ownPinMinWidth {
			layer.mark = pinDockMark
			if docked {
				layer.mark = pinFloatMark
			}
			p.pinAt = pinRect(p.closeAt)
			layer.pin = p.pinAt
		}
		covers = append(covers, rect)
		if d := m.context; d != nil {
			images = append(images, d.placement(m, rect, cellWidth, cellHeight)...)
		}
		if p == held {
			front = layer
		} else {
			layers = append(layers, layer)
		}
	}
	if front != nil {
		// The window with the keys is drawn over the others.
		layers = append(layers, front)
	}
	return layers, covers, images
}

// panelKeys are the keys a window keeps while it has them. Every other key
// is meant for what is behind it, and takes the keys along.
var panelKeys = []string{
	"<Escape>", "<C-c>", "<C-z>", "<Enter>", "<Tab>", "<S-Tab>", "<Backtab>", "<M-p>",
	"<Up>", "<Down>", "<Left>", "<Right>", "<PageUp>", "<PageDown>", "<Home>", "<End>",
}

// panelStop is where Tab stops for kind, if its panel is open: at a window of
// polly's own screen (own), or at the host's window for it once that has a
// terminal connected (host). Caller holds r.model.mu.
func (r *managedREPL) panelStop(kind floatKind) (own bool, host *satellitePane, ok bool) {
	h := &r.panes
	if kind == floatView {
		if r.framed() {
			return true, nil, !r.workspace().inspector.searching
		}
		if p := h.pane; p != nil && p.screen != nil && p.blockID != "" {
			return false, p, true
		}
		return false, nil, false
	}
	p := h.floatOf(kind)
	switch {
	case p == nil || p.panel.owner != r.model:
		return false, nil, false
	case p.own:
		return true, nil, true
	}
	return false, p, p.screen != nil && p.blockID != ""
}

// noPanel is the composer's place in the round Tab makes of the panels.
const noPanel floatKind = -1

// tabToNextPanel moves the keys from the panel after (noPanel for the
// composer) to the next open one in canonical order, todo, agents, context:
// a window of polly's own takes polly's keys, and one of the host's is
// brought forward by the host. With none after it the keys go back to the
// composer, and it reports false. hosted says the keys are in a host's window
// now, so polly's own terminal has to be brought forward for anything of its
// own to have them. Caller holds r.model.mu.
func (r *managedREPL) tabToNextPanel(after floatKind, hosted bool) bool {
	h := &r.panes
	leave := func() {
		r.yieldPanelKeys()
		if len(r.tabs) > 0 {
			i := &r.workspace().inspector
			i.focused, i.keyboardAction = false, ""
		}
	}
	for _, kind := range panelOrder[slices.Index(panelOrder, after)+1:] {
		own, host, ok := r.panelStop(kind)
		if !ok {
			continue
		}
		leave()
		switch {
		case host != nil:
			r.focusHostPane(host.blockID)
			return true
		case kind == floatView:
			r.workspace().inspector.focused = true
		case own:
			r.giveKeys(h.floatOf(kind))
		}
		if hosted {
			r.focusHostPane(h.block)
		}
		return true
	}
	if after == noPanel {
		return false // Nothing is open: Tab is the composer's own.
	}
	leave()
	if hosted {
		r.focusHostPane(h.block)
	}
	return false
}

// hasOtherPanelStop reports whether a panel besides kind is open for Tab to
// move on to. Caller holds r.model.mu.
func (r *managedREPL) hasOtherPanelStop(kind floatKind) bool {
	for _, other := range panelOrder {
		if _, _, ok := r.panelStop(other); ok && other != kind {
			return true
		}
	}
	return false
}

// focusHostPane has the host bring the terminal id forward with the keys:
// one of polly's panels' windows, or polly's own. A host that could not is
// reported, and the keys stay where they were.
func (r *managedREPL) focusHostPane(id string) {
	opener := r.panes.opener
	if opener == nil || id == "" || r.work == nil {
		return
	}
	name := opener.words().name
	r.background(func() {
		ctx, cancel := context.WithTimeout(r.work.ctx, paneOpenTimeout)
		defer cancel()
		err := opener.focus(ctx, id)
		if err == nil {
			return
		}
		r.postUI(r.work.ctx, func() {
			r.model.mu.Lock()
			defer r.model.mu.Unlock()
			if paneBehind(err) {
				r.noteBehindLocked("The panel is in another "+name+" "+opener.words().tab, err)
				return
			}
			r.model.appendNoticeLine(name + " did not move the keys to the panel · " + err.Error())
		})
	})
}

// redockHostPanel is Alt-P in a window of the host's: the panel is docked
// beside polly, or floated over it again, by closing its window and opening
// it the other way, and which is remembered as when the user docks one in the
// host (see repl_pane_dock.go). The new window takes the keys the old one
// had. A host that has no window of the other kind leaves the panel to
// polly's own screen. Caller holds r.model.mu.
func (r *managedREPL) redockHostPanel(p *satellitePane) {
	h := &r.panes
	kind := h.kindOf(p)
	r.toggleDock(kind)
	h.keysFor, h.keysUntil = kind, time.Now().Add(paneOpenTimeout)
	if kind == floatView {
		// The next frame opens the view's pane again (see syncViewPane).
		r.closePane()
		return
	}
	modal, want := p.panel.modal, p.want
	if kind == floatChecklist {
		want = r.checklistWantLocked()
	}
	r.endFloat(p, false)
	r.floatModal(kind, modal, panelSpecs[kind].label, want)
}

// panelHasUnselectedAction reports whether m has an action row that is not
// the one selected: Tab is the window's own until it is.
func panelHasUnselectedAction(m *replModal) bool {
	if m.selectedValue() != "" {
		return false
	}
	for _, item := range m.items {
		if item.value != "" {
			return true
		}
	}
	return false
}

// ownWindowEvent routes an event among the windows of polly's own screen,
// before a dialog or the composer sees it: a press on a window gives it the
// keys and handles it via the shared panel dispatcher, a press elsewhere or a
// key that is not a window's takes them back, and the marks on a window's
// border dock/float or close it. It reports whether the event went no further.
// Caller holds the visible model's lock.
func (r *managedREPL) ownWindowEvent(e ui.Event) bool {
	if r.model.modal != nil {
		return false // A blocking dialog owns all input, over every window.
	}
	held := r.focusedOwnWindow()
	switch e.Type {
	case ui.KeyboardEvent:
		if held == nil {
			return false
		}
		// Approval and history search own Escape first, matching the inspector.
		if e.ID == "<Escape>" && (r.model.hist.searching || r.model.approval != nil) {
			r.yieldPanelKeys()
			return false
		}
		if e.ID == "<M-p>" {
			if r.panes.own.pins {
				r.toggleDock(held.panel.kind)
			}
			return true
		}
		if e.ID == "<Tab>" && !panelHasUnselectedAction(held.panel.modal) && r.hasOtherPanelStop(held.panel.kind) {
			r.tabToNextPanel(held.panel.kind, false)
			return true
		}
		back := held.panel.modal.context != nil && held.panel.modal.context.usageOpen && (e.ID == "<Backspace>" || e.ID == "<C-h>")
		if !back && !slices.Contains(panelKeys, e.ID) {
			r.yieldPanelKeys()
			return false
		}
		if e.ID == "<C-z>" {
			r.requestSuspend()
			return true
		}
		r.handlePanelModalEvent(held, e)
		return true
	case ui.MouseEvent:
		if r.scrollDrag.pane == "panel" {
			return r.handleScrollbarFor(e, "panel")
		}
		mouse, ok := e.Payload.(ui.Mouse)
		pressed := e.ID == "<MouseLeft>" || e.ID == "<MouseWheelUp>" || e.ID == "<MouseWheelDown>"
		if !ok || !pressed || mouse.Drag {
			return false
		}
		point := image.Pt(mouse.X, mouse.Y)
		over := r.ownWindowAt(point)
		switch {
		case over == nil:
			// The press is for what is behind the windows.
			r.yieldPanelKeys()
			return false
		case e.ID == "<MouseLeft>" && point.In(over.pinAt):
			r.toggleDock(over.panel.kind)
			return true
		case e.ID == "<MouseLeft>" && point.In(over.closeAt):
			r.endFloat(over, true)
			return true
		}
		if over != held {
			r.giveKeys(over)
		}
		if r.handleScrollbarFor(e, "panel") {
			return true
		}
		r.handlePanelModalEvent(over, e)
		return true
	}
	return false
}
