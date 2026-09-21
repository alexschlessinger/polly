package main

import (
	"image"

	"github.com/gdamore/tcell/v3"
)

// All content writers use this screen on the UI loop, including the image
// manager and style-only ticks. The wrapped screen remains responsible for
// synchronization and terminal output. Damage belongs to the same UI loop as
// the painter; reads and terminal event delivery do not mutate it.
type trackedPaintScreen struct {
	tcell.Screen
	painter *framePainter
	style   tcell.Style
}

func (p *framePainter) track(screen tcell.Screen) tcell.Screen {
	if themed, ok := screen.(themedScreen); ok {
		return themedScreen{p.track(themed.Screen)}
	}
	if tracked, ok := screen.(*trackedPaintScreen); ok {
		if tracked.painter == p {
			return screen
		}
		screen = tracked.Screen
	}
	p.invalid = true
	return &trackedPaintScreen{Screen: screen, painter: p, style: tcell.StyleDefault}
}

func (p *framePainter) damageRect(rect image.Rectangle) {
	if p.buffer == nil {
		p.invalid = true
		return
	}
	rect = rect.Intersect(p.buffer.Rectangle)
	if rect.Empty() {
		return
	}
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		d := &p.damage[y]
		if d.end == 0 {
			d.start, d.end = rect.Min.X, rect.Max.X
		} else {
			d.start, d.end = min(d.start, rect.Min.X), max(d.end, rect.Max.X)
		}
	}
}

func (s *trackedPaintScreen) SetContent(x, y int, primary rune, combining []rune, st tcell.Style) {
	s.painter.damageRect(image.Rect(x-1, y, x+2, y+1))
	s.Screen.SetContent(x, y, primary, combining, st)
}

func (s *trackedPaintScreen) Put(x, y int, str string, st tcell.Style) (string, int) {
	s.painter.damageRect(image.Rect(x-1, y, x+2, y+1))
	return s.Screen.Put(x, y, str, st)
}

func (s *trackedPaintScreen) PutStrStyled(x, y int, str string, st tcell.Style) {
	w, _ := s.Screen.Size()
	s.painter.damageRect(image.Rect(x-1, y, w, y+1))
	s.Screen.PutStrStyled(x, y, str, st)
}

func (s *trackedPaintScreen) PutStr(x, y int, str string) {
	w, _ := s.Screen.Size()
	s.painter.damageRect(image.Rect(x-1, y, w, y+1))
	s.Screen.PutStr(x, y, str)
}

func (s *trackedPaintScreen) Clear() {
	s.painter.invalid = true
	s.Screen.Clear()
}

func (s *trackedPaintScreen) Fill(r rune, st tcell.Style) {
	s.painter.invalid = true
	s.Screen.Fill(r, st)
}

func (s *trackedPaintScreen) SetStyle(st tcell.Style) {
	if st != s.style {
		s.painter.invalid = true
		s.style = st
	}
	s.Screen.SetStyle(st)
}

func (s *trackedPaintScreen) SetSize(w, h int) {
	s.painter.invalid = true
	s.Screen.SetSize(w, h)
}

func (s *trackedPaintScreen) Resize(w, h, pixelW, pixelH int) {
	s.painter.invalid = true
	s.Screen.Resize(w, h, pixelW, pixelH)
}

func (s *trackedPaintScreen) Resume() error {
	s.painter.invalid = true
	return s.Screen.Resume()
}

func (s *trackedPaintScreen) Sync() {
	s.painter.invalid = true
	s.Screen.Sync()
}

func (s *trackedPaintScreen) LockRegion(x, y, w, h int, lock bool) {
	if !lock {
		s.painter.damageRect(image.Rect(x, y, x+w, y+h))
	}
	s.Screen.LockRegion(x, y, w, h, lock)
}
