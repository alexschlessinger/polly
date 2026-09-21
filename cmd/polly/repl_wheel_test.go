package main

import (
	"fmt"
	"image"
	"strings"
	"testing"
	"time"

	ui "github.com/metaspartan/gotui/v5"
)

func TestWheelBatchKeepsDeltasAndPaintsBeforeNextInput(t *testing.T) {
	r, _ := affordanceTestREPL(t)
	t.Cleanup(r.cancelWheelPaint)
	r.model.appendLine(strings.Repeat("line\n", 200))
	r.model.followBottom, r.model.scrollAnchor = false, 1
	r.render()
	var deadline <-chan time.Time
	for _, id := range []string{"<MouseWheelUp>", "<MouseWheelUp>", "<MouseWheelDown>"} {
		ev := mouseEvent(id, image.Pt(10, 10))
		r.handlePaintEvent(ev)
		r.paintAfterEvent(ev)
		if deadline == nil {
			deadline = r.wheelPaintC
		} else if r.wheelPaintC != deadline {
			t.Fatal("wheel burst replaced its paint deadline")
		}
	}
	// Applying deltas individually clamps at zero before the down event.
	if r.model.scrollAnchor != 3 || r.transcriptW.TopRow != 1 {
		t.Fatalf("anchor=%d painted=%d, want 3 and 1", r.model.scrollAnchor, r.transcriptW.TopRow)
	}
	// The event-loop dispatch must paint pending scrolling before a key or
	// click can act on the new viewport. This also cancels its pending timer.
	r.handlePaintEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Left>"})
	if r.transcriptW.TopRow != 3 || r.wheelPaintC != nil {
		t.Fatalf("input overtook paint: top=%d pending=%v", r.transcriptW.TopRow, r.wheelPaintC != nil)
	}
	r.scheduleWheelPaint()
	select {
	case <-r.wheelPaintC:
		r.render()
	case <-time.After(time.Second):
		t.Fatal("wheel batch never reached its paint deadline")
	}
	if r.wheelPaintC != nil {
		t.Fatal("paint retained a pending wheel deadline")
	}
}

func TestWheelBatchRoutesAcrossInspectorAndScrollbar(t *testing.T) {
	withDisplayTTY(t)
	r, screen := chromeTestREPL(t)
	screen.SetSize(160, 32)
	t.Cleanup(r.cancelWheelPaint)
	r.model.appendNoticeLine(strings.Repeat("line\n", 200))
	r.inspect(tabViewTarget(r.visibleTab()))
	waitInspector(t, r, 160)
	r.render()
	r.model.followBottom, r.model.scrollAnchor = false, 60
	s := r.workspace().viewState(r.workspace().inspector.target)
	s.follow, s.top = false, 60
	r.render()
	for _, pt := range []image.Point{r.chrome.main.Min.Add(image.Pt(1, 1)), r.chrome.inner.Min.Add(image.Pt(1, r.inspectorHeaderRows+1)), r.inspectorScrollbar.track.Min, r.inspectorScrollbar.track.Min} {
		ev := mouseEvent("<MouseWheelUp>", pt)
		r.handlePaintEvent(ev)
		r.paintAfterEvent(ev)
	}
	if r.model.scrollAnchor != 57 || s.top != 51 {
		t.Fatalf("pane routing lost deltas: main=%d inspector=%d, want 57/51", r.model.scrollAnchor, s.top)
	}
}

func TestWheelBatchModalMatchesPerEventPainting(t *testing.T) {
	r, _ := affordanceTestREPL(t)
	t.Cleanup(r.cancelWheelPaint)
	items := make([]replModalItem, 100)
	for i := range items {
		items[i] = replModalItem{label: fmt.Sprintf("item %03d", i)}
	}
	var wantTop, wantSelected int
	for _, batch := range []bool{false, true} {
		r.openModal(&replModal{title: "Choose", items: items, selected: 55})
		r.render()
		m := r.model.modal
		body, track := m.listBounds.Min, r.modalScrollbar.track.Min
		for _, ev := range []ui.Event{
			mouseEvent("<MouseWheelDown>", body),
			mouseEvent("<MouseWheelDown>", track),
			mouseEvent("<MouseWheelDown>", track),
			mouseEvent("<MouseWheelUp>", body),
			mouseEvent("<MouseWheelUp>", track),
		} {
			r.handlePaintEvent(ev)
			if batch {
				r.paintAfterEvent(ev)
			} else {
				r.render()
			}
		}
		r.render()
		if !batch {
			wantTop, wantSelected = m.top, m.selected
		} else if m.top != wantTop || m.selected != wantSelected {
			t.Fatalf("batch top/selection=%d/%d, per-event=%d/%d", m.top, m.selected, wantTop, wantSelected)
		}
	}
}
