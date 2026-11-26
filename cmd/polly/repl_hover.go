package main

import (
	"image"
	"slices"

	tcell "github.com/gdamore/tcell/v3"
	ui "github.com/metaspartan/gotui/v5"
)

// Hover: the one target under the pointer is underlined, and a target with
// no words names its action in the status row's idle slot. The pointer
// predicts what a click would do and moves the keys to the pane it is over
// (followPointerFocus); it never expands or selects anything.
//
// Underline is painted straight to the screen after each frame: gotui's
// markup has no underline modifier, but tcell does, and the transcript cache
// and every hitbox stay untouched.

// hoverTarget is what the pointer is over: the cells to underline (empty for
// chrome that already reacts on its own) and the hint for targets without
// words. Two equal targets need no repaint.
type hoverTarget struct {
	rect image.Rectangle
	hint string
}

const (
	hoverHintResize = "Drag to resize"
	hoverHintScroll = "Drag to scroll"
	hoverHintImage  = "Open image"
	hoverHintClose  = "Close agent panel"
	// The marks on a window of polly's own screen.
	hoverHintCloseWindow = "Close"
	hoverHintDock        = "Dock beside the conversation"
	hoverHintFloat       = "Float over the conversation"
)

// hoverUnderlineColor keeps the mark one color across a run whose text
// changes style (a green check, a bright label, muted metadata); without it
// the line would follow each cell's foreground and read as broken. Accent is
// the palette's "clickable" slot. Terminals without colored underlines fall
// back to a plain one.
func hoverUnderlineColor() ui.Color {
	return chromeColor("accent")
}

// hoverTargetAt resolves the pointer against the hitboxes of the last paint,
// in the order clicks resolve them. Caller holds r.model.mu.
func (r *managedREPL) hoverTargetAt(p image.Point) hoverTarget {
	if !r.mousePositionKnown {
		return hoverTarget{}
	}
	m := r.model
	_, height := ui.TerminalDimensions()
	if r.dialogOpen() {
		return dialogHoverTarget(m.modal, r.modalScrollbar, p)
	}
	if window := r.ownWindowAt(p); window != nil {
		// A window is over whatever else is at the pointer.
		if p.In(window.pinAt) {
			return hoverTarget{rect: window.pinAt, hint: pinHint(window.docked)}
		}
		if p.In(window.closeAt) {
			return hoverTarget{rect: window.closeAt, hint: hoverHintCloseWindow}
		}
		return dialogHoverTarget(window.panel.modal, window.panel.page.scrollbar, p)
	}
	if r.inspectorDragging || p.In(r.chrome.divider) {
		return hoverTarget{hint: hoverHintResize}
	}
	if !r.viewDetached && (r.scrollDrag.pane != "" || (!r.surface.scrollbar.thumb.Empty() && p.In(r.surface.scrollbar.track))) {
		return hoverTarget{hint: hoverHintScroll}
	}
	if p.In(m.todoLink) {
		return hoverTarget{rect: m.todoLink}
	}
	if r.framed() && p.In(r.chrome.pin) {
		return hoverTarget{rect: r.chrome.pin, hint: pinHint(!r.chrome.floating)}
	}
	if r.framed() && p.In(r.chrome.close) {
		return hoverTarget{rect: r.chrome.close, hint: hoverHintClose}
	}
	if r.framed() && p.In(r.chrome.frame) {
		return r.viewHoverTarget(p)
	}
	if p.In(m.parentLink) {
		return hoverTarget{rect: m.parentLink}
	}
	for _, f := range []statusSessionPlacement{m.status.modelField, m.status.sessionField, m.status.agentsField, m.status.contextField} {
		if rect := f.rect(height); p.In(rect) {
			return hoverTarget{rect: rect}
		}
	}
	right := r.chrome.main.Max.X
	if right <= 0 {
		right, _ = ui.TerminalDimensions()
	}
	return modelHoverTarget(m, p, 0, right)
}

// pinHint says what the mark on a docked or a floating window does.
func pinHint(docked bool) string {
	if docked {
		return hoverHintFloat
	}
	return hoverHintDock
}

// dialogHoverTarget resolves the pointer against a dialog's rows that a click
// acts on, and its scrollbar.
func dialogHoverTarget(modal *replModal, bar scrollbar, p image.Point) hoverTarget {
	if !bar.thumb.Empty() && p.In(bar.track) {
		return hoverTarget{hint: hoverHintScroll}
	}
	if p.In(modal.listBounds) {
		items := modal.filteredItems()
		if index := modal.top + p.Y - modal.listBounds.Min.Y; index >= 0 && index < len(items) &&
			(modal.details == nil || items[index].value != "") {
			return hoverTarget{rect: image.Rect(modal.listBounds.Min.X, p.Y, modal.listBounds.Max.X, p.Y+1)}
		}
	}
	return hoverTarget{}
}

// viewHoverTarget resolves a pointer on the inspected view's screen against
// its surface: the header's buttons, then the view's own hitboxes.
func (r *managedREPL) viewHoverTarget(p image.Point) hoverTarget {
	for _, b := range r.surface.buttons {
		if p.In(b.rect) {
			return hoverTarget{rect: b.rect}
		}
	}
	if i := &r.workspace().inspector; i.current != nil && i.current.model != nil {
		// Disclosure hitboxes in the inspector are pane-local in X, as
		// the click path expects; links and images are already absolute.
		return modelHoverTarget(i.current.model, p, r.surface.inner.Min.X, r.surface.inner.Max.X)
	}
	return hoverTarget{}
}

// modelHoverTarget resolves the pointer against one model's transcript
// hitboxes: agent links, tool outputs, activity labels, and
// image thumbnails (whose caption row carries the underline).
func modelHoverTarget(m *replModel, p image.Point, disclosureX, right int) hoverTarget {
	for _, link := range m.agentLinkPlacements {
		if p.Y == link.Y && p.X >= link.X && p.X < link.X+link.Cols {
			return hoverTarget{rect: image.Rect(link.X, link.Y, link.X+link.Cols, link.Y+1)}
		}
	}
	for _, link := range m.toolOutputLinks {
		if p.In(link.rect()) {
			return hoverTarget{rect: link.rect()}
		}
	}
	for _, kind := range disclosureKinds {
		placements := m.disclosurePlacements[kind]
		for _, pl := range placements {
			if p.In(pl.rect(disclosureX)) {
				return hoverTarget{rect: pl.mark(disclosureX)}
			}
		}
	}
	for _, img := range m.imagePlacements {
		if img.Path == "" || p.X < img.X || p.X >= img.X+img.Cols || p.Y < img.Y || p.Y >= img.Y+img.Rows {
			continue
		}
		target := hoverTarget{hint: hoverHintImage}
		if img.Y > 0 {
			// A caption may run wider than its thumbnail, but in an image
			// strip it stops where the next thumbnail's caption starts.
			end := max(img.X+img.Cols, right)
			for _, next := range m.imagePlacements {
				if next.Y == img.Y && next.X > img.X {
					end = min(end, next.X)
				}
			}
			target.rect = image.Rect(img.X, img.Y-1, end, img.Y)
		}
		return target
	}
	return hoverTarget{}
}

// paintHover underlines the hovered target on the screen just painted, from
// its first to its last non-blank cell, and clears the cells it underlined
// last time. This also restores underlines left by tick paths between frames.
// The caller presents the completed frame after these screen-only decorations.
func (r *managedREPL) paintHover(screen tcell.Screen) {
	if screen == nil {
		return
	}
	rect := r.hover.rect
	if r.model.modal == nil && r.focusedOwnWindow() == nil {
		// A detached view's selection is marked on its own screen.
		if action, ok := r.selectedInspectorAction(); ok && !r.viewDetached {
			rect = action.mark
		} else if action, ok := r.selectedInlineAction(); ok {
			rect = action.mark
		}
	}
	r.hoverCells = underlineRect(screen, r.hoverCells, rect)
}

// underlineRect moves a screen's underline from the cells it marked last
// time to rect, and returns the cells now marked.
func underlineRect(screen tcell.Screen, cells []image.Point, rect image.Rectangle) []image.Point {
	for _, pt := range cells {
		setScreenUnderline(screen, pt, false)
	}
	cells = cells[:0]
	width, height := screen.Size()
	for y := max(0, rect.Min.Y); y < min(height, rect.Max.Y); y++ {
		first, last := -1, -1
		for x := max(0, rect.Min.X); x < min(width, rect.Max.X); x++ {
			if str, _, _ := screen.Get(x, y); str != "" && str != " " {
				if first < 0 {
					first = x
				}
				last = x
			}
		}
		for x := first; first >= 0 && x <= last; x++ {
			pt := image.Pt(x, y)
			setScreenUnderline(screen, pt, true)
			cells = append(cells, pt)
		}
	}
	return cells
}

func (r *managedREPL) hovered(pt image.Point) bool {
	return slices.Contains(r.hoverCells, pt)
}

func setScreenUnderline(screen tcell.Screen, pt image.Point, on bool) {
	str, style, _ := screen.Get(pt.X, pt.Y)
	if str == "" {
		return
	}
	if on {
		style = style.Underline(true, hoverUnderlineColor())
	} else {
		style = style.Underline(false)
	}
	screen.Put(pt.X, pt.Y, str, style)
}
