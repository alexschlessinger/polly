// Package headlessscreen runs tcell's native renderer against a terminal
// emulator. Keep the evolving vt API here rather than in the REPL and tests.
package headlessscreen

import (
	"errors"
	"fmt"
	"sync"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/screenimg"
	"github.com/gdamore/tcell/v3"
	"github.com/gdamore/tcell/v3/vt"
)

// nativeScreen promotes GetCells so the REPL painter's cell-access assertion
// matches a headless screen as it does the terminal's.
type nativeScreen interface {
	tcell.Screen
	sync.Locker
	GetCells() *tcell.CellBuffer
}

// Screen owns its native event queue: scripted input is delivered by the REPL,
// so terminal negotiation and resize events are consumed here, not by callers.
// Screen.Get reads logical cells; Snapshot reads only presented output.
type Screen struct {
	nativeScreen
	term      vt.MockTerm
	lifecycle sync.Mutex
	closed    bool
}

// New returns an initialized screen with deterministic truecolor capabilities.
func New(width, height int) (*Screen, error) {
	if width <= 0 || height <= 0 {
		return nil, fmt.Errorf("invalid headless size %dx%d", width, height)
	}
	term := vt.NewMockTerm(vt.MockOptSize{X: vt.Col(width), Y: vt.Row(height)}, vt.MockOptColors(1<<24), vt.MockOptDefaultColors{
		Foreground: screenimg.DefaultForeground, Background: screenimg.DefaultBackground,
	})
	native, err := tcell.NewTerminfoScreenFromTty(term, tcell.OptTerm("xterm-256color"), tcell.OptColors(1<<24), tcell.OptAdvancedKeys(false))
	if err != nil {
		_ = term.Close()
		return nil, fmt.Errorf("create headless screen: %w", err)
	}
	if err := native.Init(); err != nil {
		_ = term.Close()
		return nil, fmt.Errorf("initialize headless screen: %w", err)
	}
	// Decode complete grapheme clusters, matching tcell's cell model. This is
	// an emulator display mode, independent of advanced keyboard reporting.
	if err := term.Backend().SetPrivateMode(vt.PmGraphemeClusters, vt.ModeOn); err != nil {
		native.Fini()
		return nil, fmt.Errorf("enable headless graphemes: %w", err)
	}
	// Fini closes the queue, which ends the consumer.
	go func() {
		for range native.EventQ() {
		}
	}()
	return &Screen{nativeScreen: native.(nativeScreen), term: term}, nil
}

// SetSize resizes the terminal and synchronizes tcell's dimensions immediately.
// Like screen writes, the caller must serialize this with application layout.
func (s *Screen) SetSize(width, height int) {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	if s.closed || width <= 0 || height <= 0 {
		return
	}
	if size := s.term.Backend().GetSize(); int(size.X) == width && int(size.Y) == height {
		return
	}
	s.term.SetSize(vt.Coord{X: vt.Col(width), Y: vt.Row(height)})
	s.nativeScreen.Show() // checks the TTY size before drawing
}

// Fini releases the native reader and emulator; later Snapshots report closed.
func (s *Screen) Fini() {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	s.nativeScreen.Fini()
}

type cell struct {
	text  string
	style tcell.Style
	width int
}

// Frame is an immutable copy of terminal output, suitable for PNGs and asserts.
// The cursor is metadata; screenimg intentionally does not rasterize it.
type Frame struct {
	width, height    int
	cells            []cell
	cursorX, cursorY int
	cursorVisible    bool
}

func (f *Frame) Size() (int, int) { return f.width, f.height }
func (f *Frame) Get(x, y int) (string, tcell.Style, int) {
	if x < 0 || y < 0 || x >= f.width || y >= f.height {
		return "", tcell.StyleDefault, 0
	}
	c := f.cells[y*f.width+x]
	return c.text, c.style, c.width
}
func (f *Frame) GetCursor() (int, int, bool) { return f.cursorX, f.cursorY, f.cursorVisible }

// Snapshot waits for output already sent by Show; it never presents new writes.
func (s *Screen) Snapshot() (*Frame, error) {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	if s.closed {
		return nil, errors.New("headless screen is closed")
	}
	// Native resize/draw activity also holds this lock. Do not call lock-taking
	// screen methods while copying; query only the mock backend under this lock.
	s.nativeScreen.Lock()
	defer s.nativeScreen.Unlock()
	if err := s.term.Drain(); err != nil {
		return nil, fmt.Errorf("drain headless output: %w", err)
	}
	size := s.term.Backend().GetSize()
	pos := s.term.Pos()
	f := &Frame{width: int(size.X), height: int(size.Y), cursorX: int(pos.X), cursorY: int(pos.Y), cursorVisible: s.term.Backend().GetCursor().IsVisible()}
	f.cells = make([]cell, f.width*f.height)
	for y := range f.height {
		for x := range f.width {
			c := s.term.GetCell(vt.Coord{X: vt.Col(x), Y: vt.Row(y)})
			// Untouched blank cells have width zero, like wide continuations. Preserve
			// actual continuations and normalize only independent empty cells.
			width := c.W
			text := c.C
			if width == 0 && (x == 0 || f.cells[y*f.width+x-1].width != 2) {
				width = 1
				text = " "
			}
			f.cells[y*f.width+x] = cell{text: text, style: decodeStyle(c.S), width: width}
		}
	}
	return f, nil
}

var underlines = map[vt.Attr]tcell.UnderlineStyle{
	vt.PlainUnderline:  tcell.UnderlineStyleSolid,
	vt.DoubleUnderline: tcell.UnderlineStyleDouble,
	vt.CurlyUnderline:  tcell.UnderlineStyleCurly,
	vt.DottedUnderline: tcell.UnderlineStyleDotted,
	vt.DashedUnderline: tcell.UnderlineStyleDashed,
}

func decodeStyle(st vt.Style) tcell.Style {
	if st == nil {
		return tcell.StyleDefault
	}
	a := st.Attr()
	s := tcell.StyleDefault.Foreground(st.Fg()).Background(st.Bg()).
		Bold(a&vt.Bold != 0).Blink(a&vt.Blink != 0).Reverse(a&vt.Reverse != 0).
		Dim(a&vt.Dim != 0).Italic(a&vt.Italic != 0).StrikeThrough(a&vt.StrikeThrough != 0)
	if ul, ok := underlines[a&vt.UnderlineMask]; ok {
		s = s.Underline(ul, st.Uc())
	}
	url, id := st.Url()
	if url != "" {
		s = s.Url(url).UrlId(id)
	}
	return s
}
