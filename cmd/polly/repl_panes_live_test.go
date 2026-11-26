package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/rex"
)

// A program gets a pane only if it takes over its terminal. This runs against
// the Rex server the test is started in, which no ordinary run has.
func TestLiveRexPlacesOnlyTerminalPrograms(t *testing.T) {
	anchor := os.Getenv("REX_BLOCK")
	if os.Getenv("POLLYTOOL_REX_LIVE_TESTS") != "1" || anchor == "" {
		t.Skip("set POLLYTOOL_REX_LIVE_TESTS=1 inside a Rex terminal to run live Rex tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	host, c := newRexPanes(anchor), rex.NewClient()
	state := func(label string) (id, state string) {
		blocks, err := c.ListBlocks(ctx, anchor)
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range blocks {
			if b.Label == label {
				return b.BlockID, b.Placement
			}
		}
		return "", ""
	}
	file := filepath.Join(t.TempDir(), "note.md")
	if err := os.WriteFile(file, []byte("hello\n"), 0600); err != nil {
		t.Fatal(err)
	}

	// A full-screen editor is placed beside the anchor.
	id, err := host.open(ctx, []string{"/usr/bin/vim", "-u", "NONE", file}, paneIfTerminal, "polly-live-vim")
	if err != nil || id == "" {
		t.Fatalf("vim was not given a pane: id=%q err=%v", id, err)
	}
	if _, placed := state("polly-live-vim"); placed != "placed" {
		t.Errorf("vim's block is %q, want placed", placed)
	}
	_ = host.close(ctx, id)
	_, _ = c.FocusBlock(ctx, anchor, anchor)

	// A program that never touches its terminal, as a windowed editor's
	// launcher does while it waits, keeps running out of sight.
	id, err = host.open(ctx, []string{"/bin/sleep", "30"}, paneIfTerminal, "polly-live-gui")
	hidden, placed := state("polly-live-gui")
	if err != nil || id != "" || hidden == "" || placed == "placed" {
		t.Errorf("a silent program: id=%q err=%v block=%q state=%q, want it running unplaced", id, err, hidden, placed)
	}
	if hidden != "" {
		_ = host.close(ctx, hidden)
	}

	// One that exits at once, well or badly, leaves nothing behind.
	for label, program := range map[string]string{"polly-live-true": "/usr/bin/true", "polly-live-false": "/usr/bin/false"} {
		if id, err := host.open(ctx, []string{program}, paneIfTerminal, label); err != nil || id != "" {
			t.Errorf("%s: id=%q err=%v", program, id, err)
		}
		time.Sleep(500 * time.Millisecond)
		if left, _ := state(label); left != "" {
			t.Errorf("%s left a block behind", program)
			_ = host.close(ctx, left)
		}
	}
}

// The wall, the tab's name and bringing a terminal forward, against a Rex
// session made for the test so the one it runs in is left as it is.
func TestLiveRexTilesNamesAndFocuses(t *testing.T) {
	if os.Getenv("POLLYTOOL_REX_LIVE_TESTS") != "1" || os.Getenv("REX_BLOCK") == "" {
		t.Skip("set POLLYTOOL_REX_LIVE_TESTS=1 inside a Rex terminal to run live Rex tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := rex.NewClient()
	idle := []string{"/bin/sleep", "120"}
	created, err := c.CreateSession(ctx, rex.CreateSessionRequest{Label: "polly-live-test", InitialWindows: []rex.InitialWindowSpec{{
		Layout: rex.ShellLayout("home", &rex.TerminalOptions{Shell: "none", Command: idle}),
	}}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.DestroySession(context.Background(), created.SessionID) }()
	anchor := created.InitialWindows[0].BlockIDs[0]
	host := newRexPanes(anchor)
	window := func(block string) rex.WindowView {
		view, err := c.InspectSession(ctx, anchor)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range view.Windows {
			if windowHolds(w, block) {
				return w
			}
		}
		t.Fatalf("no window holds %s", block)
		return rex.WindowView{}
	}
	rect := func(w rex.WindowView, block string) rex.Rect {
		var find func(n *rex.LayoutNodeView) (rex.Rect, bool)
		find = func(n *rex.LayoutNodeView) (rex.Rect, bool) {
			if n == nil {
				return rex.Rect{}, false
			}
			if n.Split == nil {
				return n.Rect, n.BlockID == block
			}
			if r, ok := find(n.Split.Before); ok {
				return r, true
			}
			return find(n.Split.After)
		}
		for _, layer := range w.Layers {
			if r, ok := find(layer.Layout); ok {
				return r
			}
		}
		t.Fatalf("%s is in no layout", block)
		return rex.Rect{}
	}
	near := func(a, b float64) bool { return a-b < 0.01 && b-a < 0.01 }

	// Five panes make three columns of equal width: two, two and one.
	ids, err := host.tile(ctx, "wall", [][]string{idle, idle, idle, idle, idle})
	if err != nil || len(ids) != 5 {
		t.Fatalf("tile: %v, %v", ids, err)
	}
	wall := window(ids[0])
	if wall.Label != "wall" {
		t.Fatalf("the wall's tab is labelled %q", wall.Label)
	}
	for n, want := range []rex.Rect{{0, 0, 1.0 / 3, 0.5}, {0, 0.5, 1.0 / 3, 0.5}, {1.0 / 3, 0, 1.0 / 3, 0.5}, {1.0 / 3, 0.5, 1.0 / 3, 0.5}, {2.0 / 3, 0, 1.0 / 3, 1}} {
		got := rect(wall, ids[n])
		for k := range got {
			if !near(got[k], want[k]) {
				t.Fatalf("pane %d sits at %v, want %v", n, got, want)
			}
		}
	}
	// One more goes under the pane it is split from.
	grown, err := host.grow(ctx, ids[4], true, idle)
	if err != nil {
		t.Fatal(err)
	}
	if got := rect(window(grown), grown); !near(got[0], 2.0/3) || !near(got[1], 0.5) {
		t.Fatalf("the grown pane sits at %v, want under the last column", got)
	}

	// Any terminal can be brought forward by its ID.
	if err := host.focus(ctx, ids[2]); err != nil {
		t.Fatal(err)
	}
	if w := window(ids[2]); w.FocusedBlockID != ids[2] {
		t.Fatalf("focus landed on %s, want %s", w.FocusedBlockID, ids[2])
	}

	// A window floats over the tab polly is in, not the wall in front of it,
	// where it was asked to, and takes no keys from the terminal that had
	// them.
	floated, err := host.float(ctx, idle, "float", [4]float64{0.57, 0.03, 0.4, 0.3})
	if err != nil {
		t.Fatal(err)
	}
	home := window(floated)
	if !windowHolds(home, anchor) || home.FocusedBlockID != anchor {
		t.Fatalf("the window floats over a tab focused on %s, want polly's own with polly focused", home.FocusedBlockID)
	}
	for _, layer := range home.Layers {
		if len(layer.Blocks) != 1 || layer.Blocks[0].BlockID != floated {
			continue
		}
		want := rex.Rect{0.57, 0.03, 0.4, 0.3}
		for k := range want {
			if layer.Kind != "floating" || !near(layer.Bounds[k], want[k]) {
				t.Fatalf("the window is a %s layer at %v, want floating at %v", layer.Kind, layer.Bounds, want)
			}
		}
	}
	// It moves to make room for another, from where polly put it only: one
	// the user has since moved, or docked into the tab, is left alone.
	at := func() rex.Rect {
		for _, layer := range window(floated).Layers {
			if layer.Kind == "floating" && len(layer.Blocks) == 1 && layer.Blocks[0].BlockID == floated {
				return layer.Bounds
			}
		}
		return rex.Rect{}
	}
	first, second, third := [4]float64{0.57, 0.03, 0.4, 0.3}, [4]float64{0.57, 0.4, 0.4, 0.2}, [4]float64{0.1, 0.1, 0.3, 0.3}
	if !nearSpot(at(), first) {
		t.Fatalf("the window was put at %v", at())
	}
	moved, err := host.arrange(ctx, []paneMove{{id: floated, to: second}})
	if err != nil || !moved[0] || !nearSpot(at(), second) {
		t.Fatalf("arrange moved %v (%v) and left the window at %v", moved, err, at())
	}
	if _, err := c.ResizeLayer(ctx, anchor, home.Layers[len(home.Layers)-1].LayerID, rex.Rect{0.05, 0.05, 0.4, 0.2}); err == nil {
		if moved, err := host.arrange(ctx, []paneMove{{id: floated, to: third}}); err != nil || moved[0] {
			t.Fatalf("a window that is not where polly put it: moved %v (%v), now at %v", moved, err, at())
		}
	}
	if docked, err := host.docked(ctx, []string{floated, anchor}); err != nil || docked[0] != dockFloating || docked[1] != dockDocked {
		t.Fatalf("docked says %v (%v) of a floating window and polly's own pane", docked, err)
	}
	if _, err := c.MoveBlock(ctx, rex.MoveBlockRequest{SessionID: anchor, BlockID: floated, AnchorBlockID: anchor, Direction: rex.DirectionHorizontal, Side: rex.SideAfter}); err != nil {
		t.Fatal(err)
	}
	if moved, err := host.arrange(ctx, []paneMove{{id: floated, to: third}}); err != nil || moved[0] || at() != (rex.Rect{}) {
		t.Fatalf("a docked window: moved %v (%v), floating at %v", moved, err, at())
	}
	if docked, err := host.docked(ctx, []string{floated}); err != nil || docked[0] != dockDocked {
		t.Fatalf("docked says %v (%v) of a window docked into the tab", docked, err)
	}

	// With polly's pane now the left half of the tab, a window floats over
	// that half, not over the pane beside it.
	half, err := host.float(ctx, idle, "float", [4]float64{0.5, 0, 0.5, 1})
	if err != nil {
		t.Fatalf("a window over polly's right half failed: %v", err)
	}
	if got, ok := host.getPlaced(half); !ok || !nearSpot(got.at, [4]float64{0.25, 0, 0.25, 1}) {
		t.Fatalf("a window over polly's right half was put at %+v", got)
	}
	_ = host.close(ctx, half)
	if err := host.close(ctx, floated); err != nil {
		t.Fatal(err)
	}

	// A panel opens docked beside polly with the share asked for, and the
	// next one under it.
	beside, err := host.dock(ctx, idle, "docked", "", 0.3)
	if err != nil {
		t.Fatal(err)
	}
	if got := rect(window(beside), beside); !near(got[0], 0.7) || !near(got[2], 0.3) || !near(got[3], 1) {
		t.Fatalf("the docked pane sits at %v, want the right 0.3 of the tab", got)
	}
	below, err := host.dock(ctx, idle, "docked", beside, 0.3)
	if err != nil {
		t.Fatal(err)
	}
	if got := rect(window(below), below); !near(got[0], 0.7) || !near(got[1], 0.5) || window(anchor).FocusedBlockID != anchor {
		t.Fatalf("the second docked pane sits at %v, want under the first, with polly still focused", got)
	}
	_, _ = host.close(ctx, beside), host.close(ctx, below)

	// The tab polly is in takes the session's title, changes with it, and gets
	// its own name back; a name someone else gave it is left alone.
	own := window(anchor).Label
	for _, step := range []struct{ was, now, want string }{
		{"calm-otter", "Fix the login", "Fix the login"},
		{"Fix the login", "Fix the logout", "Fix the logout"},
		{"something else", "Not mine to change", "Fix the logout"},
		{"Fix the logout", "", own},
	} {
		if err := host.title(ctx, step.was, step.now); err != nil {
			t.Fatal(err)
		}
		if got := window(anchor).Label; got != step.want {
			t.Fatalf("title(%q, %q) left the tab named %q, want %q", step.was, step.now, got, step.want)
		}
	}
}
