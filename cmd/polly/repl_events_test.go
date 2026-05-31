package main

import (
	"testing"

	"github.com/gdamore/tcell/v3"
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
		tcell.KeyLeft:  "<Left>",
	} {
		if ev := convertTcellKey(tcell.NewEventKey(key, "", tcell.ModNone)); ev.ID != want {
			t.Fatalf("key %v ID = %q, want %q", key, ev.ID, want)
		}
	}
}
