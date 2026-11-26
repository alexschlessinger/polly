package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"image"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	ui "github.com/metaspartan/gotui/v5"
)

// Floating windows. Where a pane host can float a window over polly's tab,
// the panels that would open over polly's own screen open in one instead:
// the checklist, the context popover, and the inspected view (see
// repl_panes.go). A window stays up beside the conversation without taking
// the keys, shows what this process knows as it changes, and is the host's
// to move, minimize or close.
//
// A panel here is the dialog polly would have opened, painted on the
// window's screen instead: the same rows, kept by the same code, for the
// session on screen. Whatever opens a panel toggles its window; a host that
// cannot float one hands the panel back to polly's screen for the rest of
// the run.

// floatKind names a side panel that can float or dock across hosts.
type floatKind int

const (
	floatChecklist floatKind = iota
	floatContext
	// floatView is the inspected view, whose external window is paneHost.pane.
	floatView
)

// panelOrder is the canonical top-to-bottom column order for the three side
// panels across every host (Polly's own screen, Rex, tmux, and Zellij).
var panelOrder = []floatKind{floatChecklist, floatView, floatContext}

// panelSpec describes one side panel's persistent name, host title, and
// baseline layout dimensions.
type panelSpec struct {
	kind  floatKind
	name  string
	label string
	want  floatWant
}

// floatInset is how far a window sits from the tab's edges, as a share of
// the tab, floatGap the space between two windows, and floatFooterRows the
// rows left clear under the windows: the composer and the status row.
const (
	floatInset      = 0.03
	floatGap        = 0.012
	floatFooterRows = 5
)

// arrangeTries is how many moves in a row a host may fail before polly stops
// placing the windows it would not move, and arrangeRetryEvery the time
// between two of those tries.
const arrangeTries = 3

var arrangeRetryEvery = 2 * time.Second

// floatPanel is a window's content: modal holds the dialog's rows and scroll
// position, built for owner's session, and page paints them.
type floatPanel struct {
	kind  floatKind
	modal *replModal
	owner *replModel
	page  *modalParagraph
}

// floatWant is the room a window asks for: columns and rows of text, within
// least and most shares of the tab's width and height. One that fills takes
// the height the others leave, and one at the bottom sits above the footer
// rather than under the tab's top.
type floatWant struct {
	columns, rows int
	least, most   [2]float64
	fill, bottom  bool
}

// The room each window asks for. The checklist adds a row per task.
var (
	checklistWant = floatWant{columns: 60, rows: 4, least: [2]float64{0.2, 0.1}, most: [2]float64{0.6, 0.7}}
	viewWant      = floatWant{columns: 100, least: [2]float64{0.4, 0.4}, most: [2]float64{0.6, 1}, fill: true}
	contextWant   = floatWant{columns: 54, rows: 34, least: [2]float64{0.2, 0.1}, most: [2]float64{0.6, 0.8}, bottom: true}

	panelSpecs = map[floatKind]panelSpec{
		floatChecklist: {kind: floatChecklist, name: "todo", label: "polly todo", want: checklistWant},
		floatView:      {kind: floatView, name: "agents", label: "polly view", want: viewWant},
		floatContext:   {kind: floatContext, name: "context", label: "polly context", want: contextWant},
	}
)

// floatLayout places windows in one column at the tab's right, leaving at
// least floatFooterRows clear at the bottom.
func floatLayout(width, height int, wants []floatWant) [][4]float64 {
	return floatLayoutWithFooter(width, height, floatFooterRows, wants)
}

// floatLayoutWithFooter places windows in one column at the tab's right,
// clearing footerRows rows at the bottom (composer, docked form, divider, and
// status bar) so none covers another or the active input area.
func floatLayoutWithFooter(width, height, footerRows int, wants []floatWant) [][4]float64 {
	cells := [2]float64{float64(max(1, width)), float64(max(1, height))}
	footer := float64(max(floatFooterRows, footerRows))
	room := max(0.2, 1-floatInset-footer/cells[1]) - floatGap*float64(max(0, len(wants)-1))
	heights := make([]float64, len(wants))
	asked, filling := 0.0, false
	for n, want := range wants {
		if want.fill {
			filling = true
			continue
		}
		heights[n] = min(want.most[1], room, max(want.least[1], float64(want.rows)/cells[1]))
		asked += heights[n]
	}
	left := room
	if filling {
		left = room / 2
	}
	if asked > left {
		squeezeHeights(heights, left, filling)
	}
	spots := make([][4]float64, len(wants))
	top, bottom := floatInset, floatInset+room+floatGap*float64(max(0, len(wants)-1))
	// One width for the column, the widest any of them asks for, so that
	// their left edges line up.
	wide := 0.0
	for _, want := range wants {
		wide = max(wide, min(want.most[0], max(want.least[0], float64(want.columns)/cells[0])))
	}
	place := func(n int, y float64) {
		spots[n] = [4]float64{1 - wide - floatInset, y, wide, heights[n]}
	}
	for n, want := range wants {
		if !want.fill && !want.bottom {
			place(n, top)
			top += heights[n] + floatGap
		}
	}
	for n := len(wants) - 1; n >= 0; n-- {
		if !wants[n].fill && wants[n].bottom {
			bottom -= heights[n]
			place(n, bottom)
			bottom -= floatGap
		}
	}
	for n, want := range wants {
		if want.fill {
			heights[n] = bottom - top
			place(n, top)
		}
	}
	return spots
}

// squeezeHeights fits heights that ask for more than left between them:
// shortest first, each keeps what it asked for while the ones after it can
// still be given as much. With filling, one of them asked for nothing and is
// no part of the sharing: it fills what the others leave.
func squeezeHeights(heights []float64, left float64, filling bool) {
	order := slices.Sorted(slices.Values(heights))
	limit, sharing := left, len(order)
	for _, h := range order {
		if h == 0 && filling && sharing == len(order) {
			sharing--
			continue
		}
		if h*float64(sharing) > limit {
			break
		}
		limit, sharing = limit-h, sharing-1
	}
	for n := range heights {
		if sharing > 0 {
			heights[n] = min(heights[n], limit/float64(sharing))
		}
	}
}

// panelOf returns the open satellitePane for kind, whether view or modal panel.
func (h *paneHost) panelOf(kind floatKind) *satellitePane {
	if kind == floatView {
		return h.pane
	}
	return h.floatOf(kind)
}

// floatWindows is the windows polly places, in canonical order from the top
// down: the checklist, the inspected view, the context popover. One the user
// has moved or docked is theirs to place, and is left out.
func (h *paneHost) floatWindows() []*satellitePane {
	var windows []*satellitePane
	for _, kind := range panelOrder {
		if p := h.panelOf(kind); p != nil && !p.loose && !p.docked && !p.own {
			windows = append(windows, p)
		}
	}
	return windows
}

// floatSpots is where each of those windows belongs now, clearing footerRows at
// the bottom of polly's pane.
func (h *paneHost) floatSpots(footerRows int) ([]*satellitePane, [][4]float64) {
	windows := h.floatWindows()
	wants := make([]floatWant, len(windows))
	for n, p := range windows {
		wants[n] = p.want
	}
	width, height := ui.TerminalDimensions()
	return windows, floatLayoutWithFooter(width, height, footerRows, wants)
}

// spotFor is where p, just added to the windows, opens.
func (h *paneHost) spotFor(p *satellitePane, footerRows int) [4]float64 {
	windows, spots := h.floatSpots(footerRows)
	if n := slices.Index(windows, p); n >= 0 {
		p.spot = spots[n]
	}
	return p.spot
}

// syncFloatLayout keeps the windows clear of each other and the composer as
// they come, go, or change height: every render starts here, and a window
// that is not where the layout puts it is moved there. The host moves only a
// window that is still where polly last put it; one the user has moved or
// docked stays theirs.
func (r *managedREPL) syncFloatLayout() {
	h := &r.panes
	opener, ok := h.windows()
	if !ok || h.arranging || r.work == nil || time.Now().Before(h.arrangeAfter) {
		return
	}
	width, height := ui.TerminalDimensions()
	r.model.mu.Lock()
	if todo := h.floatOf(floatChecklist); todo != nil && !todo.own {
		todo.want = r.checklistWantLocked()
	}
	inputRows, dividerRows, statusRows := r.footerRowCounts(width, height)
	r.model.mu.Unlock()
	h.footerRows = max(floatFooterRows, inputRows+dividerRows+statusRows)
	windows, spots := h.floatSpots(h.footerRows)
	var moved []*satellitePane
	var moves []paneMove
	for n, p := range windows {
		// A window whose host has yet to answer opens where it belongs.
		if p.blockID == "" || !h.reflow && nearSpot(p.spot, spots[n]) {
			continue
		}
		moved, moves = append(moved, p), append(moves, paneMove{id: p.blockID, to: spots[n]})
	}
	h.reflow = false
	if len(moves) == 0 {
		return
	}
	h.arranging = true
	if !r.background(func() {
		ctx, cancel := context.WithTimeout(r.work.ctx, paneOpenTimeout)
		defer cancel()
		done, err := opener.arrange(ctx, moves)
		r.postUI(r.work.ctx, func() {
			h.arranging = false
			if err != nil {
				// A host that could not move them this once is asked again on
				// a later frame, and the windows stay polly's to place. The
				// task that brings this answer paints a frame, which would ask
				// at once: a host that keeps failing is given time between
				// tries, and after arrangeTries of them the windows are left
				// where they are, as ones the user moved.
				if h.arrangeFails++; h.arrangeFails < arrangeTries {
					h.arrangeAfter = time.Now().Add(arrangeRetryEvery)
					return
				}
				h.arrangeFails = 0
				for _, p := range moved {
					p.loose = true
				}
				return
			}
			h.arrangeFails = 0
			left := false
			for n, p := range moved {
				if n >= len(done) || !done[n] {
					p.loose, left = true, true
				} else {
					p.spot = moves[n].to
				}
			}
			if left {
				// The user has one: docked, perhaps, which is worth knowing.
				r.locateWindows()
			}
		})
	}) {
		h.arranging = false
	}
}

// nearSpot reports whether two spots are the same place, as near as a host
// that rounds to cells keeps them.
func nearSpot(a, b [4]float64) bool {
	for n := range a {
		if d := a[n] - b[n]; d > 0.004 || d < -0.004 {
			return false
		}
	}
	return true
}

// floatOf finds the open window showing kind.
func (h *paneHost) floatOf(kind floatKind) *satellitePane {
	for _, p := range h.floats {
		if p.panel.kind == kind {
			return p
		}
	}
	return nil
}

// closeFloat closes the window showing kind and reports whether there was
// one: whatever opens a panel closes the one that is up. Caller holds the
// visible model's lock.
func (r *managedREPL) closeFloat(kind floatKind) bool {
	p := r.panes.floatOf(kind)
	if p != nil {
		r.endFloat(p, true)
	}
	return p != nil
}

// endFloat lets go of a window, and of its dialog unless polly's own screen
// is taking that over.
func (r *managedREPL) endFloat(p *satellitePane, wipe bool) {
	h := &r.panes
	if h.focused == p {
		r.yieldPanelKeys()
	}
	i := slices.Index(h.floats, p)
	if i < 0 {
		return
	}
	h.floats = slices.Delete(h.floats, i, i+1)
	if wipe {
		p.panel.modal.wipe()
	}
	r.endPane(p)
}

func (r *managedREPL) closeFloats() {
	for len(r.panes.floats) > 0 {
		r.endFloat(r.panes.floats[0], true)
	}
}

// floatModal shows modal in a window with the room want asks for: one of the
// host's, floating or docked where the user keeps that panel docked, and
// with no host to give one a window of polly's own screen (see
// repl_pane_own.go), which takes the keys as it opens. Caller holds the
// visible model's lock.
func (r *managedREPL) floatModal(kind floatKind, modal *replModal, label string, want floatWant) {
	h := &r.panes
	var token [16]byte
	_, err := rand.Read(token[:])
	p := newSatellitePane(hex.EncodeToString(token[:]))
	page := newModalParagraph()
	noBorder(&page.Block)
	p.panel, p.want = &floatPanel{kind: kind, modal: modal, owner: r.model, page: page}, want
	h.floats = append(h.floats, p)
	if err != nil || !h.hosts(kind) || !r.hostFloat(p, label) {
		r.makeOwnWindow(p)
		r.giveKeys(p)
	}
}

// hostFloat asks the host outside polly's screen for p's window. False means
// it has none to give.
func (r *managedREPL) hostFloat(p *satellitePane, label string) bool {
	h := &r.panes
	exe, err := os.Executable()
	if err == nil {
		err = r.listenPanes()
	}
	if err != nil {
		return false
	}
	h.await(p.token)
	if !r.hostWindow(p, []string{exe, "pane", h.socket, p.token}, label, func(id string, err error) {
		if err != nil {
			r.failFloat(p, err)
			return
		}
		if p.own {
			// The proxy never connected, and polly drew the window itself.
			r.closePaneBlock(id)
			return
		}
		p.blockID = id
		if !r.panes.has(p) && p.native == nil {
			// Closed before its proxy connected: nothing else ends it.
			r.closePaneBlock(id)
			return
		}
		r.watchPaneConnect(p)
	}) {
		h.forget(p.token)
		return false
	}
	return true
}

// failFloat gives up on the host's windows for this run and draws p's on
// polly's own screen instead. A window already closed is not the run's
// failure. No lock is held.
func (r *managedREPL) failFloat(p *satellitePane, err error) {
	if !r.panes.has(p) || p.own {
		return
	}
	if p.connectTimer != nil {
		p.connectTimer.Stop()
		p.connectTimer = nil
	}
	what := "floating window"
	if p.docked {
		r.panes.dockless, what = true, "pane"
	} else {
		r.panes.floatless = true
	}
	name := r.panes.opener.words().name
	r.panes.forget(p.token)
	if p.blockID != "" {
		r.closePaneBlock(p.blockID)
		p.blockID = ""
	}
	r.model.mu.Lock()
	defer r.model.mu.Unlock()
	r.model.appendNoticeLine(name + " " + what + " unavailable, showing it here · " + err.Error())
	r.makeOwnWindow(p)
}

// floatPanelFor builds kind's dialog for the session on screen. Caller holds
// the visible model's lock.
func (r *managedREPL) floatPanelFor(kind floatKind) *replModal {
	if kind == floatContext {
		return r.newContextModal()
	}
	return r.newTodoModal()
}

// renderFloats paints each window's dialog on its screen, after the main
// frame and from the same state.
func (r *managedREPL) renderFloats() {
	now := time.Now()
	for _, p := range r.panes.floats {
		if p.screen == nil {
			continue
		}
		width, height := p.screen.Size()
		if width <= 0 || height <= 0 {
			continue
		}
		panel := p.panel
		r.model.mu.Lock()
		m := panel.modal
		if m.context != nil {
			// The window's terminal draws no images of ours.
			r.refreshContext(m, now, false)
		}
		// The first row is the title, where the dialog has its border; the
		// rows are as wide as the window, where the dialog loses two columns
		// to that border.
		text := m.text(max(1, height-1), width+2)
		title := style.Styled(style.Truncate(m.title, max(1, width-1)), "accent", "bold")
		if strings.HasPrefix(text, " ") {
			// Rows that keep a margin get a title that keeps it too.
			title = " " + title
		}
		if notice := strings.TrimSpace(m.titleNotice); notice != "" {
			title += " " + style.Styled(notice, m.titleNoticeRole, "")
		}
		r.model.mu.Unlock()
		m.bounds = image.Rect(0, 0, width, height)
		m.listBounds = image.Rect(0, 1, width, min(height, 1+m.visible))
		page := panel.page
		page.refreshRoles()
		page.Text = title + "\n" + text
		page.SetRect(0, 0, width, height)
		if themed, ok := p.screen.(themedScreen); ok {
			themed.syncSurface()
		}
		p.painter.draw(p.screen, page)
		p.screen.Show()
	}
}

// floatsNeedTick reports whether a window animates or reads on a timer.
func (r *managedREPL) floatsNeedTick(now time.Time) bool {
	for _, p := range r.panes.floats {
		if d := p.panel.modal.context; d != nil && (p.screen != nil || p.own) && d.wantsTick(now) {
			return true
		}
	}
	return false
}

// floatInput applies an event from a remote window's terminal to its panel.
// It reports whether anything may have changed.
func (r *managedREPL) floatInput(p *satellitePane, e paneEvent) bool {
	if e.lost {
		// The window is gone, usually closed in the host.
		r.endFloat(p, true)
		return true
	}
	ev := e.ev
	if ev.Type == ui.ResizeEvent {
		return true
	}
	r.model.mu.Lock()
	defer r.model.mu.Unlock()
	if p.panel.owner != r.model {
		return true // The next frame builds the panel for the session on screen.
	}
	if ev.Type == ui.KeyboardEvent {
		switch {
		case ev.ID == "<Tab>" && !panelHasUnselectedAction(p.panel.modal):
			// The window's own action is chosen, or it has none: on to the
			// panel after this one, or back to polly's own terminal. Nothing
			// typed here hands the keys back, as it does on polly's screen.
			r.tabToNextPanel(p.panel.kind, true)
			return true
		case ev.ID == "<M-p>":
			r.redockHostPanel(p)
			return true
		}
	}
	return r.handlePanelModalEvent(p, ev)
}

// handlePanelModalEvent applies a keyboard or mouse event to a todo or context
// panel, shared between polly's own screen and remote host windows. Caller
// holds the visible model's lock.
func (r *managedREPL) handlePanelModalEvent(p *satellitePane, ev ui.Event) bool {
	m := p.panel.modal
	if m.todo != nil && r.handleTodoPanelEvent(m, ev) {
		return true
	}
	if m.context != nil {
		wasUsage := m.context.usageOpen
		if m.context.handleEvent(m, ev) {
			if p.own && m.context.usageOpen != wasUsage {
				r.panelScrollbar = scrollbar{}
				if r.scrollDrag.pane == "panel" {
					r.scrollDrag = scrollDragState{}
				}
			}
			return true
		}
	}
	page := max(1, m.visible-1)
	switch ev.ID {
	case "<Escape>", "<C-c>":
		r.endFloat(p, true)
		return true
	case "q":
		if p.own {
			return false
		}
		r.endFloat(p, true)
		return true
	case "<Enter>":
		if m.details != nil {
			r.endFloat(p, true)
			return true
		}
		return false
	case "<Up>":
		m.selected = max(0, m.selected-1)
	case "<Down>":
		m.selected = min(max(0, len(m.items)-1), m.selected+1)
	case "<MouseWheelUp>":
		m.selected = max(0, m.selected-3)
	case "<MouseWheelDown>":
		m.selected = min(max(0, len(m.items)-1), m.selected+3)
	case "<PageUp>":
		m.selected = max(0, m.selected-page)
	case "<PageDown>":
		m.selected = min(max(0, len(m.items)-1), m.selected+page)
	case "<Space>":
		if p.own {
			return false
		}
		m.selected = min(max(0, len(m.items)-1), m.selected+page)
	case "<Home>":
		m.selected = 0
	case "<End>":
		m.selected = max(0, len(m.items)-1)
	default:
		return false
	}
	m.settleListScroll(len(m.items), m.visible)
	return true
}
