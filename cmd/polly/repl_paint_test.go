package main

import (
	"image"
	"testing"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/gdamore/tcell/v3"
	ui "github.com/metaspartan/gotui/v5"
)

type paintFixture struct {
	ui.Block
	cells map[image.Point]ui.Cell
}

func (d *paintFixture) Draw(buf *ui.Buffer) {
	for pt, cell := range d.cells {
		buf.SetCell(cell, pt)
	}
}

func TestFramePainterMatchesFullRepaint(t *testing.T) {
	t.Run("tracked", func(t *testing.T) { testFramePainterMatchesFullRepaint(t, true) })
	t.Run("untracked", func(t *testing.T) { testFramePainterMatchesFullRepaint(t, false) })
}

func testFramePainterMatchesFullRepaint(t *testing.T, tracked bool) {
	t.Helper()
	legacy := tcell.NewSimulationScreen("UTF-8")
	next := tcell.NewSimulationScreen("UTF-8")
	for _, screen := range []tcell.SimulationScreen{legacy, next} {
		if err := screen.Init(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(screen.Fini)
		screen.SetSize(96, 5)
	}
	t.Cleanup(func() { style.Apply(style.DefaultTheme()) })
	style.Apply(style.DefaultTheme())
	backend := &ui.Backend{Screen: themedScreen{legacy}}
	var painter framePainter
	var output tcell.Screen = next
	if tracked {
		output = painter.track(output)
	}
	d := &paintFixture{Block: *ui.NewBlock(), cells: map[image.Point]ui.Cell{}}
	d.SetRect(0, 0, 96, 5)
	paint := func() {
		t.Helper()
		backend.Screen.Clear()
		backend.Render(d)
		painter.draw(themedScreen{output}, d)
		w, h := legacy.Size()
		for y := range h {
			for x := range w {
				want, wantStyle, wantWidth := legacy.Get(x, y)
				got, gotStyle, gotWidth := next.Get(x, y)
				if got != want || gotStyle != wantStyle || gotWidth != wantWidth {
					t.Fatalf("cell %d,%d: %q/%v/%d, want %q/%v/%d", x, y, got, gotStyle, gotWidth, want, wantStyle, wantWidth)
				}
			}
		}
	}
	d.cells[image.Pt(1, 1)] = ui.NewCell('界', ui.NewStyle(ui.ColorRed))
	d.cells[image.Pt(4, 1)] = ui.NewCell('é', ui.NewStyle(ui.ColorGreen, ui.ColorClear, ui.ModifierBold))
	d.cells[image.Pt(11, 4)] = ui.NewCell('x')
	d.cells[image.Pt(31, 3)] = ui.NewCell('界') // Straddles a comparison block.
	paint()
	paint() // Unchanged frame.
	// A direct animation/hover write must not survive the next ordinary frame.
	output.Put(7, 2, "!", tcell.StyleDefault.Reverse(true).Underline(true))
	paint()
	delete(d.cells, image.Pt(1, 1)) // Wide glyph and removed overlay erase cleanly.
	delete(d.cells, image.Pt(11, 4))
	d.cells[image.Pt(2, 1)] = ui.NewCell('a')
	d.cells[image.Pt(31, 3)] = ui.NewCell('a')
	paint()
	delete(d.cells, image.Pt(31, 3))
	d.cells[image.Pt(32, 3)] = ui.NewCell('界')
	paint()
	theme, err := style.ParseTheme("paint", []byte(`{"colors":{"background":"#123456","foreground":"#abcdef"}}`))
	if err != nil {
		t.Fatal(err)
	}
	style.Apply(theme)
	paint() // Default-colored blank cells must also adopt the new surface.
	output.Clear()
	paint() // An unchanged widget frame still restores externally cleared cells.
	for _, size := range []image.Point{{8, 3}, {16, 7}} {
		legacy.SetSize(size.X, size.Y)
		output.SetSize(size.X, size.Y)
		d.SetRect(0, 0, size.X, size.Y)
		paint()
	}
}

// Deliberately exposes only the public Screen API, exercising the fallback
// for custom screens as well as counting unnecessary terminal mutations.
type paintCountingScreen struct {
	tcell.Screen
	reads, writes, clears int
}

func (s *paintCountingScreen) Get(x, y int) (string, tcell.Style, int) {
	s.reads++
	return s.Screen.Get(x, y)
}

func (s *paintCountingScreen) Put(x, y int, text string, st tcell.Style) (string, int) {
	s.writes++
	return s.Screen.Put(x, y, text, st)
}

func TestFramePainterSkipsUntouchedColumns(t *testing.T) {
	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatal(err)
	}
	defer sim.Fini()
	sim.SetSize(2003, 80) // Also exercise the partial block at the right edge.
	counter := &paintCountingScreen{Screen: sim}
	var painter framePainter
	output := painter.track(counter)
	d := &paintFixture{Block: *ui.NewBlock(), cells: map[image.Point]ui.Cell{
		image.Pt(8, 3):    ui.NewCell('a'),
		image.Pt(1700, 3): ui.NewCell('b'),
		image.Pt(2002, 5): ui.NewCell('c'),
	}}
	d.SetRect(0, 0, 2003, 80)
	painter.draw(output, d)
	counter.reads, counter.writes = 0, 0
	painter.draw(output, d)
	if counter.reads != 0 || counter.writes != 0 {
		t.Fatalf("unchanged frame consulted screen: reads=%d writes=%d", counter.reads, counter.writes)
	}
	d.cells[image.Pt(8, 3)] = ui.NewCell('x')
	d.cells[image.Pt(1700, 3)] = ui.NewCell('y')
	delete(d.cells, image.Pt(2002, 5))
	painter.draw(output, d)
	if counter.reads > 3*34 || counter.writes != 3 {
		t.Fatalf("sparse changes scanned blank gaps: reads=%d writes=%d", counter.reads, counter.writes)
	}
	// Damage in an otherwise unchanged blank area must still be erased.
	output.Put(1000, 70, "!", tcell.StyleDefault.Reverse(true))
	counter.reads, counter.writes = 0, 0
	painter.draw(output, d)
	if counter.reads > 2*34 || counter.writes != 1 {
		t.Fatalf("overlay restoration: reads=%d writes=%d", counter.reads, counter.writes)
	}
	if text, _, _ := sim.Get(1000, 70); text != " " {
		t.Fatalf("overlay survived: %q", text)
	}
}

func (s *paintCountingScreen) Clear() {
	s.clears++
	s.Screen.Clear()
}

func TestFramePainterOnlyWritesChangedCells(t *testing.T) {
	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatal(err)
	}
	defer sim.Fini()
	sim.SetSize(20, 8)
	screen := &paintCountingScreen{Screen: sim}
	d := &paintFixture{Block: *ui.NewBlock(), cells: map[image.Point]ui.Cell{image.Pt(3, 2): ui.NewCell('a')}}
	d.SetRect(0, 0, 20, 8)
	var painter framePainter
	painter.draw(screen, d)
	screen.writes = 0
	painter.draw(screen, d)
	if screen.writes != 0 || screen.clears != 0 {
		t.Fatalf("unchanged frame: writes=%d clears=%d", screen.writes, screen.clears)
	}
	delete(d.cells, image.Pt(3, 2))
	painter.draw(screen, d)
	if screen.writes != 1 || screen.clears != 0 {
		t.Fatalf("removed glyph: writes=%d clears=%d", screen.writes, screen.clears)
	}
}

type paintResumeScreen struct{ tcell.Screen }

func (s paintResumeScreen) Resume() error {
	s.Screen.Clear() // Model a backend recreating its buffer on resume.
	return nil
}

func TestFramePainterRestoresExternalWritesAndScreenReplacement(t *testing.T) {
	newScreen := func() tcell.SimulationScreen {
		s := tcell.NewSimulationScreen("UTF-8")
		if err := s.Init(); err != nil {
			t.Fatal(err)
		}
		s.SetSize(96, 6)
		t.Cleanup(s.Fini)
		return s
	}
	first := newScreen()
	var painter framePainter
	output := painter.track(paintResumeScreen{first})
	d := &paintFixture{Block: *ui.NewBlock(), cells: map[image.Point]ui.Cell{image.Pt(2, 1): ui.NewCell('x')}}
	d.SetRect(0, 0, 96, 6)
	painter.draw(output, d)
	check := func(t *testing.T, s tcell.Screen) {
		t.Helper()
		for y := range 6 {
			for x := range 96 {
				want := " "
				if x == 2 && y == 1 {
					want = "x"
				}
				if got, st, _ := s.Get(x, y); got != want || st != tcell.StyleDefault {
					t.Fatalf("cell %d,%d = %q/%v, want %q/default", x, y, got, st, want)
				}
			}
		}
	}
	for _, change := range []struct {
		name string
		fn   func()
	}{
		{"animation", func() { output.SetContent(40, 2, '*', nil, tcell.StyleDefault.Bold(true)) }},
		{"styled-string", func() { output.PutStrStyled(30, 2, "a long highlighted string", tcell.StyleDefault.Reverse(true)) }},
		{"string", func() { output.PutStr(60, 3, "another overlay") }},
		{"fill", func() { output.Fill('!', tcell.StyleDefault.Dim(true)) }},
		{"same-size-resize", func() { output.SetSize(96, 6) }},
		{"resume", func() {
			if err := output.Resume(); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(change.name, func(t *testing.T) {
			change.fn()
			painter.draw(output, d)
			check(t, first)
		})
	}
	// A temporary painter using the same screen must damage the owner's
	// cache rather than bypassing its tracking wrapper.
	var temporary framePainter
	overlay := &paintFixture{Block: *ui.NewBlock(), cells: map[image.Point]ui.Cell{image.Pt(70, 5): ui.NewCell('!')}}
	overlay.SetRect(0, 0, 96, 6)
	temporary.draw(output, overlay)
	painter.draw(output, d)
	check(t, first)
	second := newScreen()
	output = painter.track(second)
	painter.draw(output, d)
	check(t, second) // Same dimensions and widget content, different backend.
}
