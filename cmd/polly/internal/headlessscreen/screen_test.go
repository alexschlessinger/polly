package headlessscreen

import (
	"sync"
	"testing"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/screenimg"
	"github.com/gdamore/tcell/v3"
)

func newScreen(t *testing.T, w, h int) *Screen {
	t.Helper()
	s, err := New(w, h)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Fini)
	return s
}

func snapshot(t *testing.T, s *Screen) *Frame {
	t.Helper()
	f, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestSnapshotOnlyPresentedOutput(t *testing.T) {
	s := newScreen(t, 40, 3)
	s.Show()
	before := snapshot(t, s)
	s.Put(31, 1, "界", tcell.StyleDefault)
	if text, _, _ := snapshot(t, s).Get(31, 1); text != " " {
		t.Fatalf("unpresented text %q", text)
	}
	s.Show()
	after := snapshot(t, s)
	if text, _, w := after.Get(31, 1); text != "界" || w != 2 {
		t.Fatalf("wide cell %q width %d", text, w)
	}
	if _, _, w := after.Get(32, 1); w != 0 {
		t.Fatalf("continuation width %d", w)
	}
	if text, _, _ := before.Get(31, 1); text != " " {
		t.Fatal("snapshot mutated")
	}
	s.FillArea(31, 1, 2, 1, ' ', tcell.StyleDefault)
	s.Put(2, 0, "e\u0301", tcell.StyleDefault)
	s.Show()
	if text, _, w := snapshot(t, s).Get(31, 1); text != " " || w != 1 {
		t.Fatalf("wide clear %q width %d", text, w)
	}
	if text, _, w := snapshot(t, s).Get(2, 0); text != "e\u0301" || w != 1 {
		t.Fatalf("grapheme %q width %d", text, w)
	}
}

func TestSnapshotStylesAndDefaults(t *testing.T) {
	t.Setenv("NO_COLOR", "1") // Headless capabilities are explicit, independent of the host.
	t.Setenv("TERM", "dumb")
	t.Setenv("LC_ALL", "C")
	s := newScreen(t, 40, 3)
	s.Put(0, 0, "a", tcell.StyleDefault)
	s.Put(1, 0, "b", tcell.StyleDefault.Foreground(tcell.ColorSilver).Background(tcell.ColorBlack))
	want := tcell.StyleDefault.Foreground(tcell.NewHexColor(0x123456)).Background(tcell.NewHexColor(0xabcdef)).Bold(true).Italic(true).Blink(true).Reverse(true).StrikeThrough(true)
	s.Put(2, 0, "c", want)
	s.Put(3, 0, "d", tcell.StyleDefault.Dim(true))
	for i, ul := range []tcell.UnderlineStyle{tcell.UnderlineStyleSolid, tcell.UnderlineStyleDouble, tcell.UnderlineStyleCurly, tcell.UnderlineStyleDotted, tcell.UnderlineStyleDashed} {
		s.Put(i, 1, "u", want.Underline(ul, tcell.ColorRed).Url("https://example.com").UrlId("test"))
	}
	s.Show()
	f := snapshot(t, s)
	_, a, _ := f.Get(0, 0)
	_, b, _ := f.Get(1, 0)
	_, c, _ := f.Get(2, 0)
	if a.GetForeground() != screenimg.DefaultForeground || a.GetBackground() != screenimg.DefaultBackground {
		t.Fatalf("default colors %v", a)
	}
	if b.GetForeground().Hex() != tcell.ColorSilver.Hex() || b.GetBackground().Hex() != tcell.ColorBlack.Hex() {
		t.Fatalf("explicit colors %v", b)
	}
	if _, dim, _ := f.Get(3, 0); !dim.HasDim() || dim.HasBold() {
		t.Fatalf("dim style %v", dim)
	}
	if c != want {
		t.Fatalf("style %v, want %v", c, want)
	}
	for i, ul := range []tcell.UnderlineStyle{tcell.UnderlineStyleSolid, tcell.UnderlineStyleDouble, tcell.UnderlineStyleCurly, tcell.UnderlineStyleDotted, tcell.UnderlineStyleDashed} {
		_, st, _ := f.Get(i, 1)
		id, url := st.GetUrl()
		if st.GetUnderlineStyle() != ul || st.GetUnderlineColor().Hex() != tcell.ColorRed.Hex() || id != "test" || url != "https://example.com" {
			t.Fatalf("underline/link %d: %v, %q/%q", i, st, id, url)
		}
	}
}

func TestResizeCursorAndShutdown(t *testing.T) {
	for range 3 {
		s := newScreen(t, 20, 4)
		for i := range 300 {
			w, h := 20+i%3, 4+i%2
			s.SetSize(w, h)
			if x, y := s.Size(); x != w || y != h {
				t.Fatalf("resize %dx%d, want %dx%d", x, y, w, h)
			}
			s.Put(w-1, h-1, "x", tcell.StyleDefault)
			s.ShowCursor(3, 2)
			s.Show()
			f := snapshot(t, s)
			if x, y, visible := f.GetCursor(); x != 3 || y != 2 || !visible {
				t.Fatalf("cursor %d,%d,%v", x, y, visible)
			}
			if text, _, _ := f.Get(w-1, h-1); text != "x" {
				t.Fatalf("resized output %q", text)
			}
			s.HideCursor()
			s.Show()
			if _, _, visible := snapshot(t, s).GetCursor(); visible {
				t.Fatal("cursor visible after hide")
			}
		}
		var wg sync.WaitGroup
		for range 3 {
			wg.Go(s.Fini)
		}
		wg.Wait()
		select {
		case <-s.eventsDone:
		default:
			t.Fatal("event consumer survived Fini")
		}
		if _, err := s.Snapshot(); err == nil {
			t.Fatal("snapshot succeeded after Fini")
		}
	}
	if _, err := New(0, 1); err == nil {
		t.Fatal("invalid size accepted")
	}
}
