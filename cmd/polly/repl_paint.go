package main

import (
	"image"
	"sync"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/gdamore/tcell/v3"
	ui "github.com/metaspartan/gotui/v5"
)

// framePainter retains two widget buffers to find changed spans before
// consulting tcell's live cells. trackedPaintScreen also records writes made
// between frames, so animations and hover cannot leave stale cells behind.
type framePainter struct {
	buffer, previous *ui.Buffer
	damage           []paintSpan
	runs             []paintRun
	invalid          bool
	epoch            uint64
}

type paintSpan struct{ start, end int }
type paintRun struct {
	paintSpan
	y int
}

// Native tcell screens expose bulk cell access on their concrete backend.
// Other Screen implementations still work through the public Get/Put API.
type paintCellAccess interface {
	sync.Locker
	GetCells() *tcell.CellBuffer
}

func (p *framePainter) draw(screen tcell.Screen, items ...ui.Drawable) {
	w, h := screen.Size()
	rect := image.Rect(0, 0, w, h)
	if p.buffer == nil || p.buffer.Rectangle != rect {
		p.buffer = ui.NewBuffer(rect)
		p.previous = ui.NewBuffer(rect)
		p.damage = make([]paintSpan, h)
		p.invalid = true
	} else {
		p.buffer, p.previous = p.previous, p.buffer
		p.buffer.Fill(ui.CellClear, rect)
	}
	for _, item := range items {
		item.Lock()
		item.Draw(p.buffer)
		item.Unlock()
	}
	target := screen
	if themed, ok := screen.(themedScreen); ok {
		target = themed.Screen
	}
	tracked, ok := target.(*trackedPaintScreen)
	// Untracked custom screens may be changed through another reference;
	// retain the full live-screen comparison for those callers.
	full := !ok || tracked.painter != p || p.invalid || p.epoch != style.Epoch()
	p.changedRuns(full)
	p.flush(screen)
	clear(p.damage)
	p.invalid = false
	p.epoch = style.Epoch()
	screen.Show()
}

// Compare fixed-size blocks before consulting the screen. Untouched blank
// columns never reach tcell's Get/Put path.
// Separate runs preserve blank gaps between content in different panes.
func (p *framePainter) changedRuns(full bool) {
	const block = 32
	p.runs = p.runs[:0]
	w := p.buffer.Dx()
	for y := 0; y < p.buffer.Dy(); y++ {
		if full {
			p.addRun(y, 0, w)
			continue
		}
		after := p.buffer.Cells[y*w:][:w]
		before := p.previous.Cells[y*w:][:w]
		damage := p.damage[y]
		for x := 0; x < w; x += block {
			end := min(x+block, w)
			dirty := damage.end > x && damage.start < end
			if !dirty {
				if end-x == block {
					dirty = *(*[block]ui.Cell)(after[x:]) != *(*[block]ui.Cell)(before[x:])
				} else {
					for i := x; i < end; i++ {
						if after[i] != before[i] {
							dirty = true
							break
						}
					}
				}
			}
			if dirty {
				// Include neighbors when a wide glyph crosses a block edge.
				p.addRun(y, max(0, x-1), min(w, end+1))
			}
		}
	}
}

func (p *framePainter) addRun(y, start, end int) {
	if n := len(p.runs); n > 0 && p.runs[n-1].y == y && p.runs[n-1].end >= start {
		p.runs[n-1].end = max(p.runs[n-1].end, end)
		return
	}
	p.runs = append(p.runs, paintRun{paintSpan{start, end}, y})
}

// ASCII cells reuse substrings instead of allocating a string for each cell.
const paintASCII = "\x00\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0a\x0b\x0c\x0d\x0e\x0f" +
	"\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f" +
	" !\"#$%&'()*+,-./0123456789:;<=>?@ABCDEFGHIJKLMNOPQRSTUVWXYZ[\\]^_`abcdefghijklmnopqrstuvwxyz{|}~\x7f"

func (p *framePainter) flush(screen tcell.Screen) {
	// Native screens expose their cell buffer and lock for bulk access. Lock
	// once rather than taking it for each cell; never call screen methods
	// that acquire it while here. Show handles dirty output and image locks.
	target := screen
	if themed, ok := screen.(themedScreen); ok {
		target = themed.Screen
	}
	if tracked, ok := target.(*trackedPaintScreen); ok && tracked.painter == p {
		target = tracked.Screen
	}
	w, h := screen.Size()
	// The painter already resolves the theme and owns the frame. Its writes
	// bypass tracking; only writes made outside this flush create damage.
	get, put := target.Get, target.Put
	if backend, ok := target.(paintCellAccess); ok {
		backend.Lock()
		defer backend.Unlock()
		cells := backend.GetCells()
		w, h = cells.Size()
		get, put = cells.Get, cells.Put
	}
	w, h = min(w, p.buffer.Dx()), min(h, p.buffer.Dy())
	var last ui.Style
	var resolved tcell.Style
	validStyle := false
	for _, run := range p.runs {
		y := run.y
		if y >= h {
			continue
		}
		row := p.buffer.Cells[y*p.buffer.Dx():][:w]
		for x := run.start; x < min(run.end, w); x++ {
			cell := row[x]
			if cell.Rune == 0 {
				cell = ui.CellClear
			}
			if !validStyle || cell.Style != last {
				last = cell.Style
				resolved = tcell.StyleDefault.Foreground(last.Fg).Background(last.Bg).Attributes(last.Modifier)
				if themed, ok := screen.(themedScreen); ok {
					resolved = themed.surface(resolved)
				}
				validStyle = true
			}
			text := ""
			if cell.Rune >= 0 && cell.Rune < 128 {
				text = paintASCII[cell.Rune : cell.Rune+1]
			} else {
				text = string(cell.Rune)
			}
			oldText, oldStyle, _ := get(x, y)
			if text != oldText || resolved != oldStyle {
				put(x, y, text, resolved)
			}
		}
	}
}
