package main

import (
	"image"

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
	point image.Point
	base  ui.Cell
}

// inspectorBorder caches the inspector's static perimeter independently of
// transcript size. Only a geometry change rebuilds the cells.
type inspectorBorder struct {
	rect  image.Rectangle
	cells []edgeCell
	// masks are screen cells another element owns, which the border skips.
	masks []image.Rectangle
}

func (b *inspectorBorder) geometry(rect image.Rectangle) {
	if rect == b.rect {
		return
	}
	b.rect, b.cells = rect, b.cells[:0]
	if rect.Dx() < 2 || rect.Dy() < 2 {
		return
	}
	add := func(x, y int, ch rune) {
		b.cells = append(b.cells, edgeCell{point: image.Pt(x, y), base: ui.Cell{Rune: ch, Style: ui.NewStyle(chromeColor("muted"))}})
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

func (b *inspectorBorder) masked(pt image.Point) bool {
	for _, rect := range b.masks {
		if pt.In(rect) {
			return true
		}
	}
	return false
}

func (b *inspectorBorder) draw(buf *ui.Buffer) {
	for _, cell := range b.cells {
		if !b.masked(cell.point) {
			buf.SetCell(cell.base, cell.point)
		}
	}
}
