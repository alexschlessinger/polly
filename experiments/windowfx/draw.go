package main

import (
	"fmt"
	"image"
	"math"
	"strings"

	"github.com/gdamore/tcell/v3"
	rw "github.com/mattn/go-runewidth"
	ui "github.com/metaspartan/gotui/v5"
)

var (
	desk   = rgb(13, 17, 25)
	ink    = rgb(207, 217, 232)
	subtle = rgb(124, 141, 164)
	sea    = rgb(111, 223, 206)
)

func rgb(r, g, b int32) ui.Color { return ui.NewColorRGB(r, g, b) }
func clamp(n, lo, hi int) int    { return max(lo, min(hi, n)) }

func mix(a, b ui.Color, f float64) ui.Color {
	f = math.Max(0, math.Min(1, f))
	ar, ag, ab := a.RGB()
	br, bg, bb := b.RGB()
	return rgb(int32(float64(ar)+float64(br-ar)*f), int32(float64(ag)+float64(bg-ag)*f), int32(float64(ab)+float64(bb-ab)*f))
}

type canvas struct {
	buf     *ui.Buffer
	clip    image.Rectangle
	palette bool
}

// Clip each pane before drawing so narrow resizes cannot overwrite its
// scrollbar or neighbors. Exports use exactly the same terminal cells.
func (c canvas) cell(x, y int, r rune, fg, bg ui.Color, modifier ui.Modifier) {
	if !image.Pt(x, y).In(c.clip) {
		return
	}
	if c.palette {
		fg, bg = ansiColor(fg), ansiColor(bg)
	}
	c.buf.SetCell(ui.Cell{Rune: r, Style: ui.NewStyle(fg, bg, modifier)}, image.Pt(x, y))
}

func (c canvas) text(x, y, width int, text string, fg, bg ui.Color, bold bool) {
	mod := ui.Modifier(0)
	if bold {
		mod = ui.ModifierBold
	}
	for _, r := range rw.Truncate(text, max(0, width), "…") {
		c.cell(x, y, r, fg, bg, mod)
		x += rw.RuneWidth(r)
	}
}

func (c canvas) fill(r image.Rectangle, ch rune, fg, bg ui.Color) {
	r = r.Intersect(c.clip)
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			c.cell(x, y, ch, fg, bg, 0)
		}
	}
}

func (c canvas) hline(x, y, n int, ch rune, fg, bg ui.Color) {
	c.fill(image.Rect(x, y, x+max(0, n), y+1), ch, fg, bg)
}

var ansiSwatches = []struct {
	color ui.Color
	rgb   ui.Color
}{
	{ui.ColorBlack, rgb(0, 0, 0)}, {ui.ColorMaroon, rgb(128, 0, 0)},
	{ui.ColorGreen, rgb(0, 128, 0)}, {ui.ColorOlive, rgb(128, 128, 0)},
	{ui.ColorNavy, rgb(0, 0, 128)}, {ui.ColorPurple, rgb(128, 0, 128)},
	{ui.ColorTeal, rgb(0, 128, 128)}, {ui.ColorSilver, rgb(192, 192, 192)},
	{ui.ColorGrey, rgb(128, 128, 128)}, {ui.ColorRed, rgb(255, 0, 0)},
	{ui.ColorLime, rgb(0, 255, 0)}, {ui.ColorYellow, rgb(255, 255, 0)},
	{ui.ColorBlue, rgb(0, 0, 255)}, {tcell.ColorFuchsia, rgb(255, 0, 255)},
	{tcell.ColorAqua, rgb(0, 255, 255)}, {ui.ColorWhite, rgb(255, 255, 255)},
}

func ansiColor(c ui.Color) ui.Color {
	r, g, b := c.RGB()
	best, distance := ui.ColorBlack, int64(math.MaxInt64)
	for _, s := range ansiSwatches {
		sr, sg, sb := s.rgb.RGB()
		d := int64(r-sr)*int64(r-sr) + int64(g-sg)*int64(g-sg) + int64(b-sb)*int64(b-sb)
		if d < distance {
			best, distance = s.color, d
		}
	}
	return best
}

func (p *playground) galleryLayout() (cols, rows, size int) {
	cols = 1
	if p.GetRect().Dx() >= 110 {
		cols = 2
	}
	rows = clamp((p.GetRect().Dy()-13)/11, 1, 3)
	return cols, rows, cols * rows
}

func (p *playground) gallerySize() int {
	_, _, n := p.galleryLayout()
	return n
}

func (p *playground) Draw(buf *ui.Buffer) {
	c := canvas{buf: buf, clip: buf.Rectangle, palette: p.palette}
	c.fill(buf.Rectangle, ' ', ink, desk)
	p.windows, p.targets, p.divider = [2]windowGeometry{}, nil, image.Rectangle{}
	w, h := buf.Dx(), buf.Dy()
	if w < 72 || h < 28 {
		c.text(2, 2, w-4, "WINDOW LAB  /  resize to at least 72 × 28", ink, desk, true)
		c.text(2, 4, w-4, "q quit  ·  controls resume after resizing", subtle, desk, false)
		return
	}
	c.text(3, 1, w-6, "P O L L Y   /   W I N D O W   L A B", ink, desk, true)
	mode := "SPLIT PREVIEW"
	if p.gallery {
		mode = "STYLE GALLERY"
	}
	c.text(w-23, 1, 20, mode, sea, desk, false)
	c.hline(3, 4, w-6, '─', rgb(41, 53, 70), desk)
	if p.gallery {
		p.drawGallery(c)
	} else {
		p.drawSplit(c)
	}
	play, colorMode := "motion on", "RGB"
	if p.paused {
		play = "paused"
	} else if p.state > attention {
		play = "settled"
	}
	if p.palette {
		colorMode = "ANSI"
	}
	c.hline(3, h-6, w-6, '─', rgb(41, 53, 70), desk)
	status := fmt.Sprintf("%s  ·  %s  ·  %.2gx  ·  %s", strings.ToUpper(activityNames[p.state]), play, p.speed, colorMode)
	if !p.gallery {
		status += "  ·  focus: " + []string{"conversation", "scout"}[p.focus]
	}
	c.text(3, h-5, w-6, status, sea, desk, false)
	if p.gallery {
		c.text(3, h-3, w-6, "←/→ style   [/] page   Enter or click explore   s state   Space pause   +/- speed   c colors", ink, desk, false)
		c.text(3, h-2, w-6, "q quit  ·  scrollbars, title controls, divider & bottom-edge grips are live in split preview", subtle, desk, false)
	} else if w < 110 {
		c.text(3, h-4, w-6, "Drag divider / edge / thumb · wheel scrolls the pointed pane", subtle, desk, false)
		c.text(3, h-3, w-6, "q quit  ←/→ style  g gallery  Tab focus  z zoom  x close  i restore", ink, desk, false)
		c.text(3, h-2, w-6, "↑/↓ scroll  [/] width  {/} height  s state  Space pause  r reset", subtle, desk, false)
	} else {
		c.text(3, h-4, w-6, "Drag divider / bottom edge / scrollbar  ·  wheel over either pane  ·  click title to focus", subtle, desk, false)
		c.text(3, h-3, w-6, "←/→ style  g gallery  Tab focus  z zoom  x close  i restore  b original  [/] width  {/} height", ink, desk, false)
		c.text(3, h-2, w-6, "↑/↓ PgUp/PgDn Home/End scroll   s state   Space pause   +/- speed   c colors   r reset   q quit", subtle, desk, false)
	}
}

func (p *playground) drawGallery(c canvas) {
	w, h := c.buf.Dx(), c.buf.Dy()
	cols, rows, count := p.galleryLayout()
	start := p.selected / count * count
	c.text(3, 3, w-6, fmt.Sprintf("%02d–%02d / 12   ·   Same inspector. Different personalities. Select a study to try its controls.", start+1, min(12, start+count)), subtle, desk, false)
	cellW, cellH := (w-6-(cols-1)*4)/cols, (h-13)/rows
	for n := 0; n < count && start+n < len(styles); n++ {
		kind := start + n
		x, y := 3+(n%cols)*(cellW+4), 6+(n/cols)*cellH
		fg, mark := subtle, " "
		if kind == p.selected {
			fg, mark = sea, "›"
		}
		c.text(x, y, cellW, fmt.Sprintf("%s %02d  %s", mark, kind+1, strings.ToUpper(styles[kind].name)), fg, desk, true)
		r := image.Rect(x, y+1, x+cellW, y+cellH-2)
		p.drawWindow(c, r, kind, 1, true, true)
		c.text(x+1, y+cellH-2, cellW-2, styles[kind].note, subtle, desk, false)
		p.targets = append(p.targets, galleryTarget{rect: image.Rect(x, y, x+cellW, y+cellH-1), kind: kind})
	}
}

func (p *playground) drawSplit(c canvas) {
	w, h := c.buf.Dx(), c.buf.Dy()
	kind := p.selected
	if p.reference {
		kind = 0
	}
	c.text(3, 3, w-6, fmt.Sprintf("%02d / 12  %s  ·  %s", kind+1, strings.ToUpper(styles[kind].name), styles[kind].note), subtle, desk, false)
	p.stage = image.Rect(3, 6, w-3, h-9)
	height := clamp(int(float64(p.stage.Dy())*p.heightRatio), 12, p.stage.Dy())
	r := p.stage
	r.Max.Y = r.Min.Y + height
	if p.maximized || p.hidden >= 0 || w < 92 {
		pane := p.focus
		if p.hidden >= 0 {
			pane = 1 - p.hidden
		}
		p.windows[pane] = p.drawWindow(c, r, kind, pane, false, true)
	} else {
		left := clamp(int(float64(r.Dx()-3)*p.ratio), 32, r.Dx()-35)
		x := r.Min.X + left
		p.windows[0] = p.drawWindow(c, image.Rect(r.Min.X, r.Min.Y, x, r.Max.Y), kind, 0, false, p.focus == 0)
		p.windows[1] = p.drawWindow(c, image.Rect(x+3, r.Min.Y, r.Max.X, r.Max.Y), kind, 1, false, p.focus == 1)
		p.divider = image.Rect(x, r.Min.Y, x+3, r.Max.Y)
		fg := rgb(56, 73, 91)
		if p.drag.kind == "divider" {
			fg = sea
		}
		c.fill(image.Rect(x+1, r.Min.Y, x+2, r.Max.Y), '│', fg, desk)
		c.text(x, r.Min.Y+r.Dy()/2, 3, "‹┃›", sea, desk, false)
	}
	c.text(4, h-8, w-8, ">  Sketching a calmer workspace…", sea, desk, false)
	if w < 92 && !p.maximized && p.hidden < 0 {
		c.text(4, h-7, w-8, "Compact layout · Tab switches panes · widen to 92 columns for the split", subtle, desk, false)
	} else {
		c.text(4, h-7, w-8, "Sample conversation  ·  local only  ·  no models or sessions", subtle, desk, false)
	}
}

func scrollbar(track image.Rectangle, total, visible, top int) image.Rectangle {
	if track.Empty() {
		return image.Rectangle{}
	}
	length := track.Dy()
	size := clamp(int(math.Round(float64(length)*float64(visible)/float64(max(1, total)))), 1, length)
	travel := length - size
	offset := 0
	if total > visible {
		offset = int(math.Round(float64(clamp(top, 0, total-visible)) / float64(total-visible) * float64(travel)))
	}
	return image.Rect(track.Min.X, track.Min.Y+offset, track.Max.X, track.Min.Y+offset+size)
}
