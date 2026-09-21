package main

import (
	"time"

	ui "github.com/metaspartan/gotui/v5"
)

func isWheelEvent(ev ui.Event) bool {
	return ev.Type == ui.MouseEvent && (ev.ID == "<MouseWheelUp>" || ev.ID == "<MouseWheelDown>")
}

func (r *managedREPL) handlePaintEvent(ev ui.Event) bool {
	// A click must see the hitboxes of the scrolled frame, and keys must not
	// overtake pending visual navigation.
	if !isWheelEvent(ev) && r.wheelPaintC != nil {
		r.render()
	}
	return r.handleEvent(ev)
}

func (r *managedREPL) paintAfterEvent(ev ui.Event) {
	if !r.wantsRenderForEvent(ev) {
		return
	}
	if isWheelEvent(ev) {
		r.scheduleWheelPaint()
	} else {
		r.render()
	}
}

// Do not reset the deadline on each event: continuous scrolling must still
// paint. The handlers apply every delta in order, including at scroll bounds.
func (r *managedREPL) scheduleWheelPaint() {
	if r.wheelPaintC != nil {
		return
	}
	if r.wheelPaintTimer == nil {
		r.wheelPaintTimer = time.NewTimer(time.Second / 60)
	} else {
		r.wheelPaintTimer.Reset(time.Second / 60)
	}
	r.wheelPaintC = r.wheelPaintTimer.C
}

func (r *managedREPL) cancelWheelPaint() {
	if r.wheelPaintTimer != nil {
		r.wheelPaintTimer.Stop()
	}
	r.wheelPaintC = nil
}
