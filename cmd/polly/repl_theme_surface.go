package main

import (
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	tcell "github.com/gdamore/tcell/v3"
	ui "github.com/metaspartan/gotui/v5"
)

// themedScreen paints the theme's surface: the "background" and "foreground"
// roles stand in wherever a cell leaves its background or foreground at the
// terminal default. Nearly every cell polly builds does (ui.ColorClear), so the
// substitution happens once, at the screen, instead of at each call site — the
// buffers, the link hit test, and the wrap gutter detector keep comparing the
// unsubstituted styles. With both roles at inherit the wrapper changes nothing.
//
// Every writer reaches the screen through ui.DefaultBackend.Screen (gotui's
// render, screenCell, the hover underline), all on the event loop, which is
// also where style.Apply rewrites the roles Surface reads.
type themedScreen struct {
	tcell.Screen
}

func (s themedScreen) surface(st tcell.Style) tcell.Style {
	fg, bg := style.Surface()
	if fg != ui.ColorClear && st.GetForeground() == tcell.ColorDefault {
		st = st.Foreground(fg)
	}
	if bg != ui.ColorClear && st.GetBackground() == tcell.ColorDefault {
		st = st.Background(bg)
	}
	return st
}

func (s themedScreen) SetContent(x, y int, primary rune, combining []rune, st tcell.Style) {
	s.Screen.SetContent(x, y, primary, combining, s.surface(st))
}

func (s themedScreen) Put(x, y int, str string, st tcell.Style) (string, int) {
	return s.Screen.Put(x, y, str, s.surface(st))
}

func (s themedScreen) PutStrStyled(x, y int, str string, st tcell.Style) {
	s.Screen.PutStrStyled(x, y, str, s.surface(st))
}

func (s themedScreen) Fill(r rune, st tcell.Style) {
	s.Screen.Fill(r, s.surface(st))
}

// syncSurface sets the screen's default style, which Clear fills with and
// which therefore shows through every cell a frame does not draw. render calls
// it before each ui.Clear, so a theme change lands on the next paint.
func (s themedScreen) syncSurface() {
	s.Screen.SetStyle(s.surface(tcell.StyleDefault))
}

// syncThemeSurface is syncSurface on the live screen; a no-op before Run
// installs the wrapper (tests render without one).
func syncThemeSurface() {
	if s, ok := ui.DefaultBackend.Screen.(themedScreen); ok {
		s.syncSurface()
	}
}
