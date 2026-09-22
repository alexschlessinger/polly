package main

import (
	ui "github.com/metaspartan/gotui/v5"
	"strings"
	"testing"
)

func TestHelpModalLeavesRoomAboveAndBelow(t *testing.T) {
	withDisplayTTY(t)
	r, screen := affordanceTestREPL(t)
	t.Cleanup(func() { _ = r.work.close() })
	for _, size := range []struct{ w, h, lines int }{{80, 24, 1}, {140, 40, 1}, {80, 24, 8}} {
		screen.SetSize(size.w, size.h)
		draft := strings.Repeat("draft\n", size.lines-1) + "draft"
		r.model.ed.setText(draft)
		before := r.model.fullTranscript()
		r.runCommand("/help")
		r.render()
		modal := r.model.modal
		layout := r.frameLayoutFor(size.w, size.h)
		if modal == nil || modal.bounds.Min.Y < 2 || modal.bounds.Max.Y > layout.transcriptHeight-2 || modal.bounds.Dy() > 24 {
			t.Fatalf("help crowds frame at %+v: bounds=%v transcript height=%d", size, modal.bounds, layout.transcriptHeight)
		}
		if r.model.ed.text() != draft || r.model.fullTranscript() != before {
			t.Fatal("help changed draft or transcript")
		}
		r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Down>"})
		r.render()
		if modal.top != 1 {
			t.Fatal("help Down did not scroll immediately")
		}
		r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<End>"})
		r.render()
		if modal.top == 0 {
			t.Fatal("help did not scroll")
		}
		r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Escape>"})
		if r.model.modal != nil || r.model.ed.text() != draft {
			t.Fatal("closing help lost draft")
		}
	}
}

func TestHelpModalFiltersAndShowsCommandDetails(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	r.model.busy = true
	r.runCommand("/help")
	m := r.model.modal
	if m == nil {
		t.Fatal("help did not open")
	}
	if text := plainStyledText(m.text(10, 76)); !strings.Contains(text, "PgUp/PgDn") || strings.Contains(text, "/type to filter") {
		t.Fatalf("incorrect help footer: %s", text)
	}
	for _, ch := range "sandbox" {
		r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: string(ch)})
	}
	text := plainStyledText(m.text(10, 76))
	if !strings.Contains(text, "/sandbox") || strings.Contains(text, "/add-dir") {
		t.Fatalf("filtered help: %s", text)
	}
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Escape>"})
	if r.model.cancelKey != "" || r.model.canceling {
		t.Fatal("dismissing help armed cancellation")
	}
	r.runCommand("/help /inspect")
	if m = r.model.modal; m == nil || !strings.Contains(strings.Join(m.helpLines, "\n"), "usage: /inspect") {
		t.Fatal("command help did not open in modal")
	}
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-c>"})
	if r.model.modal != nil || r.model.cancelKey != "" || r.model.canceling {
		t.Fatal("Ctrl-C did not just dismiss help")
	}
}
