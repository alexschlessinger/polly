package main

import (
	"image"
	"math"
	"sync"
	"time"

	"github.com/gdamore/tcell/v3"
	ui "github.com/metaspartan/gotui/v5"
)

// Theme conversion happens only at the display boundary. The parser's ANSI
// roles remain canonical: wrapping recognizes them, and view caches share them.
type tuiPalette struct {
	pane, text, muted, edge, accent, glint, ok, attention, failure ui.Color
}

var haloPalette = tuiPalette{
	pane: tcell.NewHexColor(0x13191f),
	text: tcell.NewHexColor(0xcfd9e8), muted: tcell.NewHexColor(0x7c8da4),
	edge: tcell.NewHexColor(0x394e57), accent: tcell.NewHexColor(0x6fdfce),
	glint: tcell.NewHexColor(0xe5fffa),
	ok:    tcell.NewHexColor(0x73d59a), attention: tcell.NewHexColor(0xf6b460),
	failure: tcell.NewHexColor(0xf67b95),
}

func (r *managedREPL) haloEnabled() bool { return r.config != nil && r.config.Theme == "halo" }

func (r *managedREPL) haloChrome(width, height int) bool {
	return r.haloEnabled() && r.workspace().inspector.open && width >= 24 && height >= 12
}

type themeLayer struct {
	ui.Drawable
	halo    bool
	colors  int
	palette map[ui.Color]ui.Color
	panes   []image.Rectangle
	modal   ui.Drawable
}

// terminalPalette is the terminal's indexed palette, built once per depth:
// FindColor's nearest-color scan is per cell, so the candidate list is not.
var terminalPalettes sync.Map

func terminalPalette(n int) []tcell.Color {
	if v, ok := terminalPalettes.Load(n); ok {
		return v.([]tcell.Color)
	}
	colors := make([]tcell.Color, n)
	for i := range colors {
		colors[i] = tcell.PaletteColor(i)
	}
	v, _ := terminalPalettes.LoadOrStore(n, colors)
	return v.([]tcell.Color)
}

func (t *themeLayer) color(c ui.Color) ui.Color {
	if t.colors >= 1<<24 || c == ui.ColorClear {
		return c
	}
	if t.colors <= 0 {
		return ui.ColorClear
	}
	if v, ok := t.palette[c]; ok {
		return v
	}
	v := tcell.FindColor(c, terminalPalette(min(t.colors, 256)))
	if t.palette == nil {
		t.palette = make(map[ui.Color]ui.Color)
	}
	if len(t.palette) >= 1024 {
		clear(t.palette)
	}
	t.palette[c] = v
	return v
}

func (t *themeLayer) contains(pt image.Point) bool {
	if !t.halo {
		return false
	}
	if t.modal != nil && pt.In(t.modal.GetRect()) {
		return true
	}
	for _, rect := range t.panes {
		if pt.In(rect) {
			return true
		}
	}
	return false
}

func (t *themeLayer) convert(cell ui.Cell, pt image.Point) ui.Cell {
	if !t.contains(pt) {
		return cell
	}
	p := haloPalette
	s := &cell.Style
	switch s.Fg {
	case ui.ColorClear, ui.ColorWhite:
		s.Fg = p.text
	case ui.ColorGrey:
		s.Fg = p.muted
	case ui.ColorBlue, ui.ColorTeal:
		s.Fg = p.accent
	case ui.ColorGreen:
		s.Fg = p.ok
	case ui.ColorYellow:
		s.Fg = p.attention
	case ui.ColorRed:
		s.Fg = p.failure
	}
	// Inspector chrome shares the main canvas background. Dialogs receive
	// their opaque background after drawing in Draw.
	if t.colors <= 0 {
		// Retain semantic emphasis even without a color-capable terminal.
		if s.Fg == p.accent || s.Fg == p.attention || s.Fg == p.failure {
			s.Modifier |= ui.ModifierBold
		}
		s.Fg, s.Bg = ui.ColorClear, ui.ColorClear
	} else {
		// Explicit RGB media cells are left alone on true-color terminals.
		s.Fg, s.Bg = t.color(s.Fg), t.color(s.Bg)
	}
	return cell
}

func (t *themeLayer) Draw(buf *ui.Buffer) {
	t.Drawable.Draw(buf)
	if t.halo {
		for n, cell := range buf.Cells {
			pt := image.Pt(buf.Min.X+n%buf.Dx(), buf.Min.Y+n/buf.Dx())
			if cell.Rune == 0 && t.contains(pt) {
				cell.Rune = ' '
				cell.Style = ui.StyleClear
			}
			cell = t.convert(cell, pt)
			if t.modal != nil {
				cell.Style.Modifier |= ui.ModifierDim
			}
			buf.Cells[n] = cell
		}
	}
	if t.modal != nil {
		t.modal.Draw(buf)
		rect := t.modal.GetRect().Intersect(buf.Rectangle)
		for y := rect.Min.Y; y < rect.Max.Y; y++ {
			for x := rect.Min.X; x < rect.Max.X; x++ {
				pt := image.Pt(x, y)
				cell := buf.GetCell(pt)
				cell = t.convert(cell, pt)
				if t.halo {
					cell.Style.Bg = t.color(haloPalette.pane)
				}
				buf.SetCell(cell, pt)
			}
		}
	}
}

func screenCell(screen tcell.Screen, pt image.Point, cell ui.Cell) {
	s := cell.Style
	style := tcell.StyleDefault.Foreground(s.Fg).Background(s.Bg).
		Bold(s.Modifier&ui.ModifierBold != 0).Dim(s.Modifier&ui.ModifierDim != 0).
		Reverse(s.Modifier&ui.ModifierReverse != 0).Italic(s.Modifier&ui.ModifierItalic != 0).
		StrikeThrough(s.Modifier&tcell.AttrStrikeThrough != 0).Blink(s.Modifier&ui.ModifierBlink != 0)
	screen.SetContent(pt.X, pt.Y, cell.Rune, nil, style)
}

type haloEdgeCell struct {
	point      image.Point
	base, last ui.Cell
}

// One perimeter cache, independent of transcript size. The epoch survives full
// redraws; only a geometry change rebuilds the clockwise path.
type haloOrbit struct {
	rect   image.Rectangle
	cells  []haloEdgeCell
	epoch  time.Time
	active bool
}

func (o *haloOrbit) geometry(rect image.Rectangle) {
	if rect == o.rect {
		return
	}
	o.rect, o.cells = rect, o.cells[:0]
	if rect.Dx() < 2 || rect.Dy() < 2 {
		return
	}
	add := func(x, y int, ch rune) {
		o.cells = append(o.cells, haloEdgeCell{point: image.Pt(x, y), base: ui.Cell{Rune: ch, Style: ui.NewStyle(haloPalette.edge)}})
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

func haloMix(a, b ui.Color, amount float64) ui.Color {
	ar, ag, ab := a.RGB()
	br, bg, bb := b.RGB()
	mix := func(x, y int32) int32 { return x + int32(float64(y-x)*amount) }
	return tcell.NewRGBColor(mix(ar, br), mix(ag, bg), mix(ab, bb))
}

func (o *haloOrbit) frame(n int, now time.Time) ui.Cell {
	cell := o.cells[n].base
	if o.active && len(o.cells) > 0 {
		head := math.Mod(now.Sub(o.epoch).Seconds()*22, float64(len(o.cells)))
		behind := math.Mod(head-float64(n)+float64(len(o.cells)), float64(len(o.cells)))
		if behind < 35 {
			cell.Style.Fg = haloMix(haloPalette.edge, haloPalette.glint, math.Exp(-behind/7))
		}
	}
	return cell
}

func (o *haloOrbit) draw(buf *ui.Buffer, now time.Time) {
	if o.epoch.IsZero() {
		o.epoch = now
	}
	for n := range o.cells {
		buf.SetCell(o.frame(n, now), o.cells[n].point)
	}
}

func (o *haloOrbit) tick(screen tcell.Screen, theme *themeLayer, now time.Time) {
	if screen == nil || theme == nil {
		return
	}
	changed := false
	for n := range o.cells {
		cell := theme.convert(o.frame(n, now), o.cells[n].point)
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
