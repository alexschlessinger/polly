package main

import (
	"image"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v3"
	ui "github.com/metaspartan/gotui/v5"
)

func TestPointerNavigationUsesVisiblePaneGeometry(t *testing.T) {
	withDisplayTTY(t)
	r, screen := affordanceTestREPL(t)
	t.Cleanup(func() { _ = r.work.close() })
	r.model.appendLine(strings.Repeat("main transcript line\n", 100))
	r.model.appendThinking(strings.Repeat("inspected thought\n", 100))
	r.model.ed.setText("draft")
	r.inspectCommand("thoughts")
	move := func(point image.Point) {
		r.handleEvent(convertTcellMouse(tcell.NewEventMouse(point.X, point.Y, tcell.ButtonNone, tcell.ModNone)))
	}
	key := func(id string) { r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: id}); r.render() }
	for _, mode := range []struct {
		width     int
		maximized bool
	}{{140, false}, {100, false}, {140, true}} {
		screen.SetSize(mode.width, 40)
		r.workspace().inspector.maximized = mode.maximized
		waitInspector(t, r, mode.width)
		r.render()
		s := r.workspace().viewState(r.workspace().inspector.target)
		move(r.chrome.inner.Min.Add(image.Pt(2, 4)))
		key("<Home>")
		key("<Down>")
		if s.top != 1 || s.follow {
			t.Fatalf("inspector line scrolling at width %d: %+v", mode.width, s)
		}
		key("<PageDown>")
		top := s.top
		if top <= 1 {
			t.Fatal("Page Down did not page inspector")
		}
		key("<Up>")
		if s.top != top-1 {
			t.Fatal("Up did not scroll inspector one row")
		}
		key("<PageUp>")
		if s.top != 0 {
			t.Fatal("Page Up did not restore inspector top")
		}
		key("<End>")
		if !s.follow {
			t.Fatal("End failed to follow inspector")
		}
		if r.model.ed.text() != "draft" || r.model.ed.cursor != 5 {
			t.Fatal("pane scrolling changed composer")
		}
		if mode.width < 120 || mode.maximized {
			if !r.chrome.main.Empty() {
				t.Fatal("hidden main transcript retained hitbox")
			}
		} else {
			move(r.chrome.main.Min.Add(image.Pt(2, 4)))
			key("<Home>")
			key("<Down>")
			if r.model.scrollAnchor != 1 || !s.follow {
				t.Fatal("main pane scrolling changed inspector")
			}
			key("<PageDown>")
			if r.model.scrollAnchor <= 1 {
				t.Fatal("main Page Down did not scroll")
			}
			key("<End>")
			if !r.model.followBottom {
				t.Fatal("main End failed to follow")
			}
		}
		move(r.inputW.Inner.Min)
		key("<Home>")
		if r.model.ed.cursor != 0 {
			t.Fatal("composer Home failed")
		}
		key("<End>")
		if r.model.ed.cursor != 5 {
			t.Fatal("composer End failed")
		}
		move(r.chrome.inner.Min.Add(image.Pt(2, 4)))
		key("<C-a>")
		if r.model.ed.cursor != 0 {
			t.Fatal("Ctrl-A stopped addressing editor")
		}
		key("<C-e>")
		if r.model.ed.cursor != 5 {
			t.Fatal("Ctrl-E stopped addressing editor")
		}
		header := plainStyledText(r.inspectorHeaderW.Text)
		if !strings.Contains(strings.Split(header, "\n")[0], "Thought · 1/1") || strings.Contains(header, "[Prev]") || strings.Contains(header, "[Next]") {
			t.Fatalf("thought chrome differs from tools: %s", header)
		}
	}
}
