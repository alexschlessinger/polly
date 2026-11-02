package main

import (
	"fmt"
	"image"
	"testing"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/headlessscreen"
	ui "github.com/metaspartan/gotui/v5"
)

// Capture every presented frame, not just the final state: a correct final
// underline can hide a preceding flush that briefly erased it.
type hoverFrameScreen struct {
	*headlessscreen.Screen
	t      *testing.T
	frames []*headlessscreen.Frame
}

func (s *hoverFrameScreen) Show() {
	s.Screen.Show()
	s.frames = append(s.frames, screenSnapshot(s.t, s.Screen))
}

func TestAgentUnderlineIsPresentedWithTheFrame(t *testing.T) {
	withDisplayTTY(t)
	for _, watch := range []bool{false, true} {
		for _, keyboard := range []bool{false, true} {
			t.Run(fmt.Sprintf("watch=%v/keyboard=%v", watch, keyboard), func(t *testing.T) {
				r, screen := chromeTestREPL(t)
				screen.SetSize(160, 40)
				presented := &hoverFrameScreen{Screen: screen, t: t}
				ui.DefaultBackend.Screen = presented
				child := newReplModel()
				child.appendLine("agent output")
				watchTestAgent(r, child)
				r.workspace().viewState(r.workspace().inspector.target).watch = watch
				r.render() // First opening, before the background projection lands.
				var title image.Rectangle
				for _, button := range r.inspectorButtons {
					if button.action == "parent" {
						title = button.rect
					}
				}
				if title.Empty() {
					t.Fatal("agent title target missing")
				}
				if keyboard {
					r.workspace().inspector.focused = true
					r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<S-Tab>"})
				} else {
					r.handleEvent(mouseEvent("<MouseRelease>", title.Min))
				}
				paint := func(want string) {
					t.Helper()
					presented.frames = nil
					r.render()
					for n, frame := range presented.frames {
						if got := frameUnderlinedRun(frame, title.Min.Y); got != want {
							t.Fatalf("presented frame %d underline = %q, want %q", n, got, want)
						}
					}
					if len(presented.frames) != 1 {
						t.Fatalf("render presented %d frames, want one complete frame", len(presented.frames))
					}
				}
				paint("fixture-agent")
				waitInspector(t, r, 160)
				for range 3 {
					paint("fixture-agent")
					if !r.inspectorFocused() {
						t.Fatal("repaint changed inspector focus")
					}
				}
				if keyboard {
					r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Tab>"})
				} else {
					r.handleEvent(mouseEvent("<MouseRelease>", image.Pt(1, 1)))
				}
				paint("")
				if r.inspectorFocused() {
					t.Fatal("leaving the pane did not return focus")
				}
			})
		}
	}
}
