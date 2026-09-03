package main

import "testing"

// A roomy terminal seats every region: transcript, dock, divider, composer,
// status bar, top to bottom.
func TestFrameLayoutSeatsEveryRegion(t *testing.T) {
	r := &managedREPL{model: newReplModel()}
	r.model.turnDock.visible = true

	l := r.frameLayoutFor(80, 24)
	want := frameLayout{width: 80, height: 24, transcriptHeight: 20, dockRows: 1, dividerRows: 1, inputRows: 1, statusRows: 1}
	if l != want {
		t.Fatalf("layout = %+v, want %+v", l, want)
	}
	if got := l.composerRow(0); got != 22 {
		t.Fatalf("composerRow(0) = %d, want 22", got)
	}
}

// A six-row terminal with an eight-line composer: the status bar keeps its
// row, the dock has no room, the composer is capped so one transcript row
// survives, and the divider drops out. The scroll handlers and render share
// this split, so the cursor lands where the composer actually starts.
func TestFrameLayoutShortTerminalCapsComposer(t *testing.T) {
	r := &managedREPL{model: newReplModel()}
	r.model.ed.setText("1\n2\n3\n4\n5\n6\n7\n8")
	r.model.turnDock.visible = true

	l := r.frameLayoutFor(80, 6)
	want := frameLayout{width: 80, height: 6, transcriptHeight: 1, inputRows: 4, statusRows: 1}
	if l != want {
		t.Fatalf("layout = %+v, want %+v", l, want)
	}
	if got := l.composerRow(2); got != 3 {
		t.Fatalf("composerRow(2) = %d, want 3", got)
	}
}

func TestTranscriptViewportWindow(t *testing.T) {
	l := frameLayout{width: 80, logoRows: 2, transcriptHeight: 10}

	// Pinned to the bottom with a one-row overlay ticker: the last ten rows
	// are in view, but the ticker covers the final one.
	v := l.transcriptViewport(25, 0, true, 1)
	if v.start != 15 || v.end != 24 || v.topPadding != 0 || v.width != 80 {
		t.Fatalf("pinned viewport = %+v", v)
	}
	if !v.contains(23) || v.contains(24) || v.screenY(15) != 2 {
		t.Fatalf("pinned viewport projection = %+v", v)
	}

	// A transcript shorter than the pane sits at the bottom, under blank rows.
	v = l.transcriptViewport(4, 0, true, 0)
	if v.start != 0 || v.end != 10 || v.topPadding != 6 || v.screenY(0) != 8 {
		t.Fatalf("short transcript viewport = %+v", v)
	}

	// A held anchor shows rows from there.
	v = l.transcriptViewport(25, 7, false, 0)
	if v.start != 7 || v.end != 17 || v.screenY(7) != 2 {
		t.Fatalf("held viewport = %+v", v)
	}

	// No pane, no window.
	v = frameLayout{width: 80}.transcriptViewport(25, 7, false, 0)
	if v.contains(7) || v.contains(0) {
		t.Fatalf("empty pane still contains rows: %+v", v)
	}
}
