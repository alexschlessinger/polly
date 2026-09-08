package main

import (
	"image"
	"math"
	"time"

	"github.com/gdamore/tcell/v3"
	ui "github.com/metaspartan/gotui/v5"
)

// chromeColor resolves a semantic color role from the parser's map, so the
// chrome uses the same terminal palette slots as the transcript.
func chromeColor(name string) ui.Color {
	return ui.StyleParserColorMap[name]
}

func screenCell(screen tcell.Screen, pt image.Point, cell ui.Cell) {
	s := cell.Style
	style := tcell.StyleDefault.Foreground(s.Fg).Background(s.Bg).
		Bold(s.Modifier&ui.ModifierBold != 0).Dim(s.Modifier&ui.ModifierDim != 0).
		Reverse(s.Modifier&ui.ModifierReverse != 0).Italic(s.Modifier&ui.ModifierItalic != 0).
		StrikeThrough(s.Modifier&tcell.AttrStrikeThrough != 0).Blink(s.Modifier&ui.ModifierBlink != 0)
	screen.SetContent(pt.X, pt.Y, cell.Rune, nil, style)
}

type edgeCell struct {
	point      image.Point
	base, last ui.Cell
}

// frameOrbit is the inspector frame's perimeter and the glint that travels
// around it while the inspected work runs. One cache, independent of
// transcript size: the epoch survives full redraws and only a geometry change
// rebuilds the clockwise path.
type frameOrbit struct {
	rect   image.Rectangle
	cells  []edgeCell
	epoch  time.Time
	active bool
	// masks are screen cells another element owns (the scrollbar thumb): the
	// orbit neither paints nor ticks them.
	masks []image.Rectangle
}

func (o *frameOrbit) geometry(rect image.Rectangle) {
	if rect == o.rect {
		return
	}
	o.rect, o.cells = rect, o.cells[:0]
	if rect.Dx() < 2 || rect.Dy() < 2 {
		return
	}
	add := func(x, y int, ch rune) {
		o.cells = append(o.cells, edgeCell{point: image.Pt(x, y), base: ui.Cell{Rune: ch, Style: ui.NewStyle(chromeColor("muted"))}})
	}
	for x := rect.Min.X; x < rect.Max.X; x++ {
		ch := '─'
		if x == rect.Min.X {
			ch = '╭'
		}
		if x == rect.Max.X-1 {
			ch = '╮'
		}
		add(x, rect.Min.Y, ch)
	}
	for y := rect.Min.Y + 1; y < rect.Max.Y; y++ {
		ch := '│'
		if y == rect.Max.Y-1 {
			ch = '╯'
		}
		add(rect.Max.X-1, y, ch)
	}
	for x := rect.Max.X - 2; x >= rect.Min.X; x-- {
		ch := '─'
		if x == rect.Min.X {
			ch = '╰'
		}
		add(x, rect.Max.Y-1, ch)
	}
	for y := rect.Max.Y - 2; y > rect.Min.Y; y-- {
		add(rect.Min.X, y, '│')
	}
}

func (o *frameOrbit) masked(pt image.Point) bool {
	for _, rect := range o.masks {
		if pt.In(rect) {
			return true
		}
	}
	return false
}

// frame is cell n as it looks now: the base border, or the glint's bold head
// and accent tail when the frame is active. The ramp uses palette slots, so
// it reads on every terminal theme.
func (o *frameOrbit) frame(n int, now time.Time) ui.Cell {
	cell := o.cells[n].base
	if o.active && len(o.cells) > 0 {
		head := math.Mod(now.Sub(o.epoch).Seconds()*22, float64(len(o.cells)))
		behind := math.Mod(head-float64(n)+float64(len(o.cells)), float64(len(o.cells)))
		switch {
		case behind < 3:
			cell.Style = ui.NewStyle(chromeColor("accent"), cell.Style.Bg, ui.ModifierBold)
		case behind < 16:
			cell.Style.Fg = chromeColor("accent")
		}
	}
	return cell
}

func (o *frameOrbit) draw(buf *ui.Buffer, now time.Time) {
	if o.epoch.IsZero() {
		o.epoch = now
	}
	for n := range o.cells {
		if o.masked(o.cells[n].point) {
			continue
		}
		buf.SetCell(o.frame(n, now), o.cells[n].point)
	}
}

// tick repaints only the perimeter cells whose glint changed since the last
// paint, straight to the screen, leaving content and hitboxes alone.
func (o *frameOrbit) tick(screen tcell.Screen, now time.Time) {
	if screen == nil {
		return
	}
	changed := false
	for n := range o.cells {
		if o.masked(o.cells[n].point) {
			continue
		}
		cell := o.frame(n, now)
		if cell != o.cells[n].last {
			screenCell(screen, o.cells[n].point, cell)
			o.cells[n].last = cell
			changed = true
		}
	}
	if changed {
		screen.Show()
	}
}
