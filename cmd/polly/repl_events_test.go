package main

import (
	"testing"

	"github.com/gdamore/tcell/v3"
	ui "github.com/metaspartan/gotui/v5"
)

func TestConvertTcellEvent(t *testing.T) {
	// Bracketed-paste markers become our paste IDs.
	if ev, ok := convertTcellEvent(tcell.NewEventPaste(true)); !ok || ev.ID != pasteStartID {
		t.Fatalf("paste start = %q ok=%v", ev.ID, ok)
	}
	if ev, ok := convertTcellEvent(tcell.NewEventPaste(false)); !ok || ev.ID != pasteEndID {
		t.Fatalf("paste end = %q ok=%v", ev.ID, ok)
	}

	// A rune key maps to its literal string ID; Alt-prefixed gets <M-…>.
	if ev := convertTcellKey(tcell.NewEventKey(tcell.KeyRune, "x", tcell.ModNone)); ev.ID != "x" {
		t.Fatalf("rune key ID = %q", ev.ID)
	}
	if ev := convertTcellKey(tcell.NewEventKey(tcell.KeyRune, "b", tcell.ModAlt)); ev.ID != "<M-b>" {
		t.Fatalf("alt key ID = %q", ev.ID)
	}

	// Named keys map through the same table gotui uses.
	for key, want := range map[tcell.Key]string{
		tcell.KeyEnter: "<Enter>",
		tcell.KeyCtrlW: "<C-w>",
		tcell.KeyCtrlR: "<C-r>",
		tcell.KeyCtrlZ: "<C-z>",
		tcell.KeyLeft:  "<Left>",
	} {
		if ev := convertTcellKey(tcell.NewEventKey(key, "", tcell.ModNone)); ev.ID != want {
			t.Fatalf("key %v ID = %q, want %q", key, ev.ID, want)
		}
	}
}

func TestMouseDragMarksHeldButtonMotion(t *testing.T) {
	var held tcell.ButtonMask
	sample := func(x int, buttons tcell.ButtonMask) ui.Mouse {
		e := tcell.NewEventMouse(x, 1, buttons, tcell.ModNone)
		return markMouseDrag(&held, e, convertTcellMouse(e)).Payload.(ui.Mouse)
	}
	if m := sample(1, tcell.Button1); m.Drag {
		t.Fatal("press marked as drag")
	}
	if m := sample(2, tcell.Button1); !m.Drag || m.X != 2 {
		t.Fatalf("held motion not marked as drag: %+v", m)
	}
	if m := sample(2, tcell.ButtonNone); m.Drag {
		t.Fatal("release marked as drag")
	}
	if m := sample(3, tcell.ButtonNone); m.Drag {
		t.Fatal("hover marked as drag")
	}
	if m := sample(3, tcell.WheelUp); m.Drag {
		t.Fatal("wheel marked as drag")
	}
	if m := sample(3, tcell.Button1); m.Drag {
		t.Fatal("fresh press after a hover marked as drag")
	}
}
