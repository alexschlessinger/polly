package main

import (
	"fmt"

	"github.com/gdamore/tcell/v3"
	ui "github.com/metaspartan/gotui/v5"
)

// gotui v5.0.3's event poller forwards only key/mouse/resize events and drops
// tcell's bracketed-paste markers, so it can't tell a pasted newline from a
// typed Enter. We run our own poll loop against the same tcell screen: it
// enables bracketed paste and surfaces PasteStart/PasteEnd as events, while
// reproducing gotui's key/mouse ID mapping so the rest of the REPL is
// unchanged. gotui is still used for all rendering.
const (
	pasteStartID = "<PasteStart>"
	pasteEndID   = "<PasteEnd>"
)

// tcellKeyMap mirrors gotui's internal key map (events.go) so the IDs the REPL
// switches on ("<Enter>", "<C-w>", "<Left>", …) are identical to what
// ui.PollEvents would have produced.
var tcellKeyMap = map[tcell.Key]string{
	tcell.KeyF1: "<F1>", tcell.KeyF2: "<F2>", tcell.KeyF3: "<F3>",
	tcell.KeyF4: "<F4>", tcell.KeyF5: "<F5>", tcell.KeyF6: "<F6>",
	tcell.KeyF7: "<F7>", tcell.KeyF8: "<F8>", tcell.KeyF9: "<F9>",
	tcell.KeyF10: "<F10>", tcell.KeyF11: "<F11>", tcell.KeyF12: "<F12>",
	tcell.KeyInsert: "<Insert>", tcell.KeyDelete: "<Delete>",
	tcell.KeyHome: "<Home>", tcell.KeyEnd: "<End>",
	tcell.KeyPgUp: "<PageUp>", tcell.KeyPgDn: "<PageDown>",
	tcell.KeyUp: "<Up>", tcell.KeyDown: "<Down>",
	tcell.KeyLeft: "<Left>", tcell.KeyRight: "<Right>",
	tcell.KeyCtrlA: "<C-a>", tcell.KeyCtrlB: "<C-b>", tcell.KeyCtrlC: "<C-c>",
	tcell.KeyCtrlD: "<C-d>", tcell.KeyCtrlE: "<C-e>", tcell.KeyCtrlF: "<C-f>",
	tcell.KeyCtrlG: "<C-g>", tcell.KeyCtrlH: "<C-h>", tcell.KeyTab: "<Tab>",
	tcell.KeyCtrlJ: "<C-j>", tcell.KeyCtrlK: "<C-k>", tcell.KeyCtrlL: "<C-l>",
	tcell.KeyEnter: "<Enter>", tcell.KeyCtrlN: "<C-n>", tcell.KeyCtrlO: "<C-o>",
	tcell.KeyCtrlP: "<C-p>", tcell.KeyCtrlQ: "<C-q>", tcell.KeyCtrlR: "<C-r>",
	tcell.KeyCtrlS: "<C-s>", tcell.KeyCtrlT: "<C-t>", tcell.KeyCtrlU: "<C-u>",
	tcell.KeyCtrlV: "<C-v>", tcell.KeyCtrlW: "<C-w>", tcell.KeyCtrlX: "<C-x>",
	tcell.KeyCtrlY: "<C-y>", tcell.KeyCtrlZ: "<C-z>", tcell.KeyEsc: "<Escape>",
	tcell.KeyBackspace: "<Backspace>", tcell.KeyBackspace2: "<Backspace>",
}

// pollManagedEvents enables bracketed paste on the screen and returns a channel
// of converted ui.Events. The goroutine exits when the screen is finalized
// (PollEvent returns nil), which ui.Close does on shutdown.
func pollManagedEvents(screen tcell.Screen) <-chan ui.Event {
	if screen != nil {
		screen.EnablePaste()
	}
	ch := make(chan ui.Event)
	go func() {
		defer close(ch)
		if screen == nil {
			return
		}
		// EventQ stays open until the screen is finalized (ui.Close → Fini),
		// at which point the range ends and the goroutine exits.
		for ev := range screen.EventQ() {
			if out, ok := convertTcellEvent(ev); ok {
				ch <- out
			}
		}
	}()
	return ch
}

// convertTcellEvent maps a tcell event to the gotui ui.Event the REPL expects,
// adding PasteStart/PasteEnd IDs that gotui itself discards. ok is false for
// events with no REPL meaning.
func convertTcellEvent(ev tcell.Event) (ui.Event, bool) {
	switch e := ev.(type) {
	case *tcell.EventKey:
		return convertTcellKey(e), true
	case *tcell.EventMouse:
		return convertTcellMouse(e), true
	case *tcell.EventResize:
		w, h := e.Size()
		return ui.Event{Type: ui.ResizeEvent, ID: "<Resize>", Payload: ui.Resize{Width: w, Height: h}}, true
	case *tcell.EventPaste:
		id := pasteEndID
		if e.Start() {
			id = pasteStartID
		}
		return ui.Event{Type: ui.KeyboardEvent, ID: id}, true
	default:
		return ui.Event{}, false
	}
}

func convertTcellKey(e *tcell.EventKey) ui.Event {
	var id string
	if e.Key() == tcell.KeyRune {
		s := e.Str()
		if e.Modifiers()&tcell.ModAlt != 0 {
			id = fmt.Sprintf("<M-%s>", s)
		} else {
			id = s
		}
	} else if val, ok := tcellKeyMap[e.Key()]; ok {
		id = val
	} else {
		id = fmt.Sprintf("<Key:%v>", e.Key())
	}
	return ui.Event{Type: ui.KeyboardEvent, ID: id, Payload: e}
}

func convertTcellMouse(e *tcell.EventMouse) ui.Event {
	btns := e.Buttons()
	id := "Unknown_Mouse_Button"
	switch {
	case btns&tcell.Button1 != 0:
		id = "<MouseLeft>"
	case btns&tcell.Button2 != 0:
		id = "<MouseMiddle>"
	case btns&tcell.Button3 != 0:
		id = "<MouseRight>"
	case btns&tcell.WheelUp != 0:
		id = "<MouseWheelUp>"
	case btns&tcell.WheelDown != 0:
		id = "<MouseWheelDown>"
	case btns == tcell.ButtonNone:
		id = "<MouseRelease>"
	}
	x, y := e.Position()
	return ui.Event{Type: ui.MouseEvent, ID: id, Payload: ui.Mouse{X: x, Y: y}}
}
