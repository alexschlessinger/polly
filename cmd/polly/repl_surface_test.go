package main

import (
	"errors"
	"image"
	"testing"

	ui "github.com/metaspartan/gotui/v5"
)

// A view shown on a screen of its own is open with no frame: the main screen
// is laid out and takes its input as if it had no inspector, and the view's
// content still answers at its surface.
func TestDetachedViewLeavesTheMainScreenAlone(t *testing.T) {
	r, fake, _ := paneTestREPL(t)
	r.model.appendThinking("a thought")
	r.panes.floatless = true // Start in the frame, as outside Rex.
	r.openAttention()
	waitInspector(t, r, 140)
	r.render()
	framed := r.chrome
	if !r.framed() || framed.frame.Empty() || r.surface.inner != framed.inner || r.surface.track != framed.edge {
		t.Fatalf("fixture: framed=%v chrome=%+v surface=%v", r.framed(), framed, r.surface.inner)
	}
	r.openAttention()
	r.render()

	// A pane takes the view: its surface is that screen's to place.
	r.panes.floatless = false
	openTestPane(t, r, fake)
	own := image.Rect(0, 0, 59, 20)
	i := &r.workspace().inspector
	if r.framed() || !i.open {
		t.Fatalf("a detached view must stay open without a frame: framed=%v open=%v", r.framed(), i.open)
	}
	if g := r.chrome; !g.frame.Empty() || !g.inner.Empty() || g.main.Dx() != 140 {
		t.Fatalf("the main screen still made room for the view: %+v", g)
	}
	if r.surface.inner != own {
		t.Fatalf("the main frame moved a surface it does not own: %v", r.surface.inner)
	}
	if got := r.projectionGeometry(140).width; got != own.Dx() {
		t.Fatalf("the view is wrapped for width %d, want its surface's %d", got, own.Dx())
	}

	// Nothing on the main screen addresses the view: not the pointer over
	// where its frame was, not Tab, not Escape, not a click or a key while it
	// searches.
	inside := image.Pt(framed.inner.Min.X+2, framed.inner.Min.Y+1)
	hoverAt(t, r, inside)
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Tab>"})
	if r.inspectorFocused() {
		t.Fatal("the main screen focused a view it does not show")
	}
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Escape>"})
	if !i.open {
		t.Fatal("Escape on the main screen closed a view shown elsewhere")
	}
	top := r.workspace().viewState(i.target).top
	r.handleEvent(mouseEvent("<MouseWheelUp>", image.Pt(5, 5)))
	if got := r.workspace().viewState(i.target).top; got != top {
		t.Fatalf("a wheel turn on the main screen scrolled the view: top %d -> %d", top, got)
	}
	i.searching = true
	r.model.ed.setText("") // Tab, with nothing to focus, went to the composer.
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "x"})
	if r.model.ed.text() != "x" || i.searchInput.text() != "" {
		t.Fatalf("the view's search took the composer's key: composer=%q search=%q", r.model.ed.text(), i.searchInput.text())
	}

	// The view's own handlers work at its surface, whoever delivers the event.
	if !r.viewSearchKey(ui.Event{Type: ui.KeyboardEvent, ID: "y"}) || i.searchInput.text() != "y" {
		t.Fatalf("the view's search ignored its own key: %q", i.searchInput.text())
	}
	i.searching = false
	if !r.viewContentMouse(mouseEvent("<MouseWheelUp>", image.Pt(5, 5)), image.Pt(5, 5)) {
		t.Fatal("a wheel turn inside the surface was not the view's")
	}
	if r.viewContentMouse(mouseEvent("<MouseWheelUp>", image.Pt(80, 5)), image.Pt(80, 5)) {
		t.Fatal("a wheel turn outside the surface was taken by the view")
	}

	// Back in the frame, the main screen makes room again.
	r.failPane(r.panes.pane, errors.New("gone"))
	r.render()
	if !r.framed() || r.chrome.frame != framed.frame || r.surface.inner != framed.inner {
		t.Fatalf("the frame did not return: chrome=%+v surface=%v", r.chrome, r.surface.inner)
	}
}
