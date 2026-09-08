package main

import (
	"image"
	"reflect"
	"strings"
	"testing"

	rw "github.com/mattn/go-runewidth"
	ui "github.com/metaspartan/gotui/v5"
)

func renderLab(p *playground, w, h int) *ui.Buffer {
	p.SetRect(0, 0, w, h)
	buf := ui.NewBuffer(p.GetRect())
	p.Draw(buf)
	return buf
}

func key(p *playground, id string) { p.handle(ui.Event{Type: ui.KeyboardEvent, ID: id}) }
func mouse(p *playground, id string, at image.Point) {
	p.handle(ui.Event{Type: ui.MouseEvent, ID: id, Payload: ui.Mouse{X: at.X, Y: at.Y}})
}

func TestScrollingAndDraggingUseRenderedGeometry(t *testing.T) {
	p := newPlayground()
	p.gallery = false
	renderLab(p, 132, 48)
	g := p.windows[0]
	mouse(p, "<MouseWheelDown>", g.body.Min)
	if p.scroll[0] != 3 || p.scroll[1] != 0 || p.focus != 1 {
		t.Fatalf("pointer scroll changed wrong pane or focus: scroll=%v focus=%d", p.scroll, p.focus)
	}
	renderLab(p, 132, 48)
	g = p.windows[0]
	mouse(p, "<MouseLeft>", g.thumb.Min)
	mouse(p, "<MouseLeft>", image.Pt(g.track.Min.X, g.track.Max.Y+50))
	mouse(p, "<MouseRelease>", g.track.Max)
	if want := g.lines - g.body.Dy(); p.scroll[0] != want {
		t.Fatalf("thumb drag got %d, want bottom %d", p.scroll[0], want)
	}
	renderLab(p, 132, 48)
	g = p.windows[0]
	before := p.scroll[0]
	mouse(p, "<MouseLeft>", g.track.Min)
	mouse(p, "<MouseRelease>", g.track.Min)
	if p.scroll[0] >= before {
		t.Fatal("clicking above the thumb did not page up")
	}
}

func TestResizeAndWindowControls(t *testing.T) {
	p := newPlayground()
	p.gallery = false
	renderLab(p, 132, 48)
	at := p.divider.Min.Add(image.Pt(1, 2))
	mouse(p, "<MouseLeft>", at)
	mouse(p, "<MouseLeft>", at.Add(image.Pt(18, 0)))
	mouse(p, "<MouseRelease>", at)
	renderLab(p, 132, 48)
	if p.windows[0].frame.Dx() <= p.windows[1].frame.Dx() {
		t.Fatal("divider drag did not enlarge left pane")
	}
	g := p.windows[1]
	at = g.frame.Max.Sub(image.Pt(1, 1))
	mouse(p, "<MouseLeft>", at)
	mouse(p, "<MouseLeft>", at.Sub(image.Pt(0, 12)))
	mouse(p, "<MouseRelease>", at)
	renderLab(p, 132, 48)
	if p.windows[1].frame.Dy() >= g.frame.Dy() || p.windows[1].frame.Dy() < 12 {
		t.Fatalf("bottom grip height = %d", p.windows[1].frame.Dy())
	}
	g = p.windows[1]
	mouse(p, "<MouseLeft>", g.zoom.Min)
	mouse(p, "<MouseLeft>", g.zoom.Min.Add(image.Pt(1, 0)))
	if !p.maximized {
		t.Fatal("held-button motion toggled the maximize control twice")
	}
	mouse(p, "<MouseRelease>", g.zoom.Min)
	renderLab(p, 132, 48)
	if !p.windows[0].frame.Empty() || p.windows[1].frame.Dx() != p.stage.Dx() {
		t.Fatal("maximize did not select the whole stage")
	}
	key(p, "x")
	renderLab(p, 132, 48)
	if p.windows[0].frame.Empty() || !p.windows[1].frame.Empty() {
		t.Fatal("closing the focused pane did not reveal its peer")
	}
	key(p, "i")
	renderLab(p, 132, 48)
	if p.windows[0].frame.Empty() || p.windows[1].frame.Empty() {
		t.Fatal("restore did not reopen both panes")
	}
}

func TestStyleAnimationProtectsContentAndSettles(t *testing.T) {
	for kind, style := range styles {
		t.Run(style.slug, func(t *testing.T) {
			p := newPlayground()
			p.gallery, p.selected = false, kind
			p.t = .2
			first := renderLab(p, 132, 48)
			g := p.windows[1]
			p.t = 2.7
			next := renderLab(p, 132, 48)
			for y := g.body.Min.Y; y < g.body.Max.Y; y++ {
				for x := g.body.Min.X; x < g.frame.Max.X-1; x++ {
					at := image.Pt(x, y)
					if first.GetCell(at) != next.GetCell(at) {
						t.Fatalf("animation changed transcript or scrollbar at %v", at)
					}
				}
			}
			if kind > 0 && reflect.DeepEqual(first.Cells, next.Cells) {
				t.Fatal("working style did not animate")
			}
			p.state = complete
			first = renderLab(p, 132, 48)
			p.t = 19
			next = renderLab(p, 132, 48)
			if !reflect.DeepEqual(first.Cells, next.Cells) {
				t.Fatal("completed style still animates")
			}
		})
	}
}

func TestNarrowLayoutsRetainControlsAndPanePosition(t *testing.T) {
	for kind := range styles {
		p := newPlayground()
		p.gallery, p.selected = false, kind
		for _, size := range []image.Point{{132, 48}, {92, 28}, {72, 28}} {
			buf := renderLab(p, size.X, size.Y)
			for _, g := range p.windows {
				if g.frame.Empty() {
					continue
				}
				if !g.body.In(g.frame) || !g.thumb.In(g.track) {
					t.Fatalf("%s at %v has invalid geometry: %+v", styles[kind].slug, size, g)
				}
				for _, button := range []image.Rectangle{g.close, g.zoom} {
					if buf.GetCell(button.Min).Rune != '[' || buf.GetCell(button.Max.Sub(image.Pt(1, 1))).Rune != ']' {
						t.Fatalf("%s at %v lost a title control", styles[kind].slug, size)
					}
				}
			}
		}
	}
	p := newPlayground()
	p.gallery = false
	renderLab(p, 72, 28)
	key(p, "<Down>")
	key(p, "<Tab>")
	renderLab(p, 72, 28)
	key(p, "<Down>")
	key(p, "<Down>")
	key(p, "<Tab>")
	renderLab(p, 72, 28)
	if p.scroll != [2]int{2, 1} {
		t.Fatalf("compact pane switching lost positions: %v", p.scroll)
	}
	renderLab(p, 35, 10)
	renderLab(p, 132, 48)
	if p.scroll != [2]int{2, 1} {
		t.Fatalf("terminal resize lost positions: %v", p.scroll)
	}
}

func TestWrappedSamplePreservesWordsAndFits(t *testing.T) {
	for width := 1; width < 80; width++ {
		for _, line := range sampleRows(1, width) {
			if rw.StringWidth(line.text) > width {
				t.Fatalf("line exceeds width %d: %q", width, line.text)
			}
		}
	}
	lines := wrapWords("The title stays readable while resizing.", 18)
	if got := strings.Join(lines, "|"); got != "The title stays|readable while|resizing." {
		t.Fatalf("word wrapping = %q", got)
	}
}
