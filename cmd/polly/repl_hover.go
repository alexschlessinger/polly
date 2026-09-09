package main

import (
	"image"

	tcell "github.com/gdamore/tcell/v3"
	ui "github.com/metaspartan/gotui/v5"
)

// Hover: the one target under the pointer is underlined, and a target with
// no words names its action in the status row's idle slot. The pointer only
// predicts what a click would do; it never expands, selects, or focuses
// anything, and keys keep routing by focus.
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
	if modal := m.modal; modal != nil {
		if !r.modalScrollbar.thumb.Empty() && p.In(r.modalScrollbar.track) {
			return hoverTarget{hint: hoverHintScroll}
		}
		if p.In(modal.listBounds) {
			if index := modal.top + p.Y - modal.listBounds.Min.Y; index >= 0 && index < len(modal.filteredItems()) {
				return hoverTarget{rect: image.Rect(modal.listBounds.Min.X, p.Y, modal.listBounds.Max.X, p.Y+1)}
			}
		}
		return hoverTarget{}
	}
	if r.inspectorDragging || p.In(r.chrome.divider) {
		return hoverTarget{hint: hoverHintResize}
	}
	if r.scrollDrag.pane != "" || (!r.inspectorScrollbar.thumb.Empty() && p.In(r.inspectorScrollbar.track)) {
		return hoverTarget{hint: hoverHintScroll}
	}
	i := &r.workspace().inspector
	if i.open && p.In(r.chrome.frame) {
		for _, b := range r.inspectorButtons {
			if p.In(b.rect) {
				return hoverTarget{rect: b.rect}
			}
		}
		if i.current != nil && i.current.model != nil {
			// Disclosure hitboxes in the inspector are pane-local in X, as
			// the click path expects; links and images are already absolute.
			return modelHoverTarget(i.current.model, p, r.chrome.inner.Min.X, r.chrome.inner.Max.X)
		}
		return hoverTarget{}
	}
	if p.In(m.parentLink) {
		return hoverTarget{rect: m.parentLink}
	}
	for _, f := range []statusSessionPlacement{m.status.sessionField, m.status.agentsField} {
		if f.hit(p.X, p.Y, height) {
			return hoverTarget{rect: image.Rect(f.X, height-1, f.X+f.Cols, height)}
		}
	}
	right := r.chrome.main.Max.X
	if right <= 0 {
		right, _ = ui.TerminalDimensions()
	}
	return modelHoverTarget(m, p, 0, right)
}

// modelHoverTarget resolves the pointer against one model's transcript
// hitboxes: agent links, activity labels, expanded tool and thought rows, and
// image thumbnails (whose caption row carries the underline).
func modelHoverTarget(m *replModel, p image.Point, disclosureX, right int) hoverTarget {
	for _, link := range m.agentLinkPlacements {
		if p.Y == link.Y && p.X >= link.X && p.X < link.X+link.Cols {
			return hoverTarget{rect: image.Rect(link.X, link.Y, link.X+link.Cols, link.Y+1)}
		}
	}
	for _, placements := range [][]disclosurePlacement{m.reasoningPlacements, m.toolDisclosurePlacements, m.agentDisclosurePlacements, m.imageDisclosurePlacements} {
		for _, pl := range placements {
			if x := pl.X + disclosureX; p.Y == pl.Y && p.X >= x && p.X < x+pl.Cols {
				return hoverTarget{rect: image.Rect(x, pl.Y, x+pl.Cols, pl.Y+1)}
			}
		}
	}
	for _, link := range m.inspectionLinks {
		if p.In(link.rect) {
			return hoverTarget{rect: link.rect}
		}
	}
	for _, img := range m.imagePlacements {
		if img.Path == "" || p.X < img.X || p.X >= img.X+img.Cols || p.Y < img.Y || p.Y >= img.Y+img.Rows {
			continue
		}
		target := hoverTarget{hint: hoverHintImage}
		if img.Y > 0 {
			target.rect = image.Rect(img.X, img.Y-1, max(img.X+img.Cols, right), img.Y)
		}
		return target
	}
	return hoverTarget{}
}

// paintHover underlines the hovered target on the screen just painted, from
// its first to its last non-blank cell, and clears the cells it underlined
// last time. gotui rewrites every cell each frame, so the clear only matters
// for the tick paths that repaint between frames.
func (r *managedREPL) paintHover(screen tcell.Screen) {
	if screen == nil {
		return
	}
	changed := len(r.hoverCells) > 0
	for _, pt := range r.hoverCells {
		setScreenUnderline(screen, pt, false)
	}
	r.hoverCells = r.hoverCells[:0]
	width, height := screen.Size()
	rect := r.hover.rect
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
			r.hoverCells = append(r.hoverCells, pt)
		}
	}
	if changed || len(r.hoverCells) > 0 {
		screen.Show()
	}
}

func (r *managedREPL) hovered(pt image.Point) bool {
	for _, cell := range r.hoverCells {
		if cell == pt {
			return true
		}
	}
	return false
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
