// windowfx is a local window-chrome playground. It never starts a provider.
package main

import (
	"flag"
	"fmt"
	"image"
	"math"
	"os"
	"strings"
	"time"

	"github.com/gdamore/tcell/v3"
	ui "github.com/metaspartan/gotui/v5"
)

type activity int

const (
	working activity = iota
	attention
	complete
	idle
)

var activityNames = []string{"working", "attention", "complete", "idle"}

type windowGeometry struct {
	frame, body, track, thumb, zoom, close image.Rectangle
	lines                                  int
}

type galleryTarget struct {
	rect image.Rectangle
	kind int
}

type dragState struct {
	kind   string
	pane   int
	offset int
}

type playground struct {
	ui.Block
	t, speed                 float64
	selected, focus          int
	state                    activity
	gallery, paused, palette bool
	maximized, reference     bool
	hidden                   int
	ratio, heightRatio       float64
	scroll                   [2]int
	windows                  [2]windowGeometry
	targets                  []galleryTarget
	stage, divider           image.Rectangle
	drag                     dragState
	mouseDown                bool
}

func newPlayground() *playground {
	return &playground{Block: *ui.NewBlock(), speed: 1, gallery: true,
		focus: 1, hidden: -1, ratio: .5, heightRatio: 1}
}

func (p *playground) moving() bool { return !p.paused && p.state <= attention }

func (p *playground) choose(n int) {
	p.selected = (n%len(styles) + len(styles)) % len(styles)
	p.reference = false
	p.drag = dragState{}
}

func (p *playground) scrollBy(pane, delta int) {
	g := p.windows[pane]
	if g.frame.Empty() {
		return
	}
	p.scroll[pane] = clamp(p.scroll[pane]+delta, 0, max(0, g.lines-g.body.Dy()))
}

func (p *playground) handle(e ui.Event) bool {
	if e.Type == ui.MouseEvent {
		if m, ok := e.Payload.(ui.Mouse); ok {
			p.mouse(e.ID, image.Pt(m.X, m.Y))
		}
		return false
	}
	if e.Type == ui.ResizeEvent {
		p.drag = dragState{}
	}
	switch e.ID {
	case "q", "<C-c>", "<Escape>":
		return true
	case "<Right>", "l":
		p.choose(p.selected + 1)
	case "<Left>", "h":
		p.choose(p.selected - 1)
	case "g", "<Enter>":
		p.gallery = !p.gallery
		p.drag = dragState{}
	case "<Tab>":
		p.focus = 1 - p.focus
		p.hidden = -1
	case " ", "<Space>":
		p.paused = !p.paused
	case "+", "=":
		p.speed = math.Min(4, p.speed*2)
	case "-":
		p.speed = math.Max(.125, p.speed/2)
	case "c":
		p.palette = !p.palette
	case "s":
		p.state = (p.state + 1) % 4
		p.t = 0
	case "z":
		p.gallery = false
		p.maximized = !p.maximized
	case "x":
		p.gallery = false
		p.hidden = p.focus
		p.focus = 1 - p.focus
	case "i":
		p.hidden, p.maximized = -1, false
	case "b":
		p.gallery = false
		p.reference = !p.reference
	case "r":
		p.t, p.ratio, p.heightRatio = 0, .5, 1
		p.scroll, p.drag = [2]int{}, dragState{}
		p.hidden, p.maximized = -1, false
	case "[":
		if p.gallery {
			p.choose(p.selected - p.gallerySize())
		} else {
			p.ratio = math.Max(.25, p.ratio-.05)
		}
	case "]":
		if p.gallery {
			p.choose(p.selected + p.gallerySize())
		} else {
			p.ratio = math.Min(.75, p.ratio+.05)
		}
	case "{":
		p.heightRatio = math.Max(.45, p.heightRatio-.05)
	case "}":
		p.heightRatio = math.Min(1, p.heightRatio+.05)
	case "<Up>", "k":
		p.scrollBy(p.focus, -1)
	case "<Down>", "j":
		p.scrollBy(p.focus, 1)
	case "<PageUp>", "<C-u>":
		p.scrollBy(p.focus, -max(1, p.windows[p.focus].body.Dy()-1))
	case "<PageDown>", "<C-d>":
		p.scrollBy(p.focus, max(1, p.windows[p.focus].body.Dy()-1))
	case "<Home>":
		p.scroll[p.focus] = 0
	case "<End>":
		p.scrollBy(p.focus, p.windows[p.focus].lines)
	default:
		if len(e.ID) == 1 && e.ID[0] >= '1' && e.ID[0] <= '9' {
			p.choose(int(e.ID[0] - '1'))
		} else if e.ID == "0" {
			p.choose(9)
		}
	}
	return false
}

func (p *playground) mouse(id string, at image.Point) {
	if id == "<MouseRelease>" {
		p.drag = dragState{}
		p.mouseDown = false
		return
	}
	firstPress := !p.mouseDown
	if id == "<MouseLeft>" {
		p.mouseDown = true
	}
	if p.gallery {
		if id == "<MouseLeft>" && firstPress {
			for _, target := range p.targets {
				if at.In(target.rect) {
					p.choose(target.kind)
					p.gallery = false
					return
				}
			}
		}
		return
	}
	if id == "<MouseLeft>" && p.drag.kind != "" {
		switch p.drag.kind {
		case "divider":
			p.ratio = math.Max(.25, math.Min(.75, float64(at.X-p.stage.Min.X)/float64(max(1, p.stage.Dx()-3))))
		case "height":
			p.heightRatio = math.Max(.45, math.Min(1, float64(at.Y-p.stage.Min.Y+1)/float64(max(1, p.stage.Dy()))))
		case "scroll":
			g := p.windows[p.drag.pane]
			travel := max(1, g.track.Dy()-g.thumb.Dy())
			top := clamp(at.Y-g.track.Min.Y-p.drag.offset, 0, travel)
			p.scroll[p.drag.pane] = int(math.Round(float64(top) / float64(travel) * float64(max(0, g.lines-g.body.Dy()))))
		}
		return
	}
	// gotui reports held-button motion as MouseLeft too. Only a fresh press
	// activates a title control; ongoing motion belongs to an established drag.
	if id == "<MouseLeft>" && !firstPress {
		return
	}
	if id == "<MouseLeft>" && at.In(p.divider) {
		p.drag.kind = "divider"
		return
	}
	for pane, g := range p.windows {
		if g.frame.Empty() || !at.In(g.frame) {
			continue
		}
		switch id {
		case "<MouseWheelUp>":
			p.scrollBy(pane, -3)
		case "<MouseWheelDown>":
			p.scrollBy(pane, 3)
		case "<MouseLeft>":
			p.focus = pane
			switch {
			case at.In(g.close):
				p.hidden, p.focus = pane, 1-pane
			case at.In(g.zoom):
				p.maximized = !p.maximized
			case at.In(g.thumb) && g.lines > g.body.Dy():
				p.drag = dragState{kind: "scroll", pane: pane, offset: at.Y - g.thumb.Min.Y}
			case at.In(g.track):
				delta := max(1, g.body.Dy()-1)
				if at.Y < g.thumb.Min.Y {
					delta = -delta
				}
				p.scrollBy(pane, delta)
			case at.Y == g.frame.Max.Y-1:
				p.drag.kind = "height"
			}
		}
		return
	}
}

func run() error {
	snapshot := flag.String("snapshot", "", "export a PNG without opening a terminal")
	animation := flag.String("gif", "", "export an animated GIF without opening a terminal")
	style := flag.String("style", "split", "style slug (see --list)")
	view := flag.String("view", "gallery", "gallery or split")
	state := flag.String("state", "working", "working, attention, complete, or idle")
	width := flag.Int("width", 132, "export width in terminal columns")
	height := flag.Int("height", 48, "export height in terminal rows")
	at := flag.Float64("at", 1.6, "animation time for PNG exports")
	seconds := flag.Float64("seconds", 6, "GIF duration in seconds (1 to 20)")
	paused := flag.Bool("still", false, "start with motion paused")
	palette := flag.Bool("palette", false, "use the 16 ANSI colors")
	list := flag.Bool("list", false, "list style names")
	flag.Parse()
	if *list {
		for i, s := range styles {
			fmt.Printf("%02d  %-12s %s\n", i+1, s.slug, s.note)
		}
		return nil
	}
	p := newPlayground()
	found := false
	for i, s := range styles {
		if strings.EqualFold(*style, s.slug) {
			p.selected, found = i, true
		}
	}
	if !found {
		return fmt.Errorf("unknown style %q; use --list", *style)
	}
	if *view != "gallery" && *view != "split" {
		return fmt.Errorf("view must be gallery or split")
	}
	found = false
	for i, name := range activityNames {
		if *state == name {
			p.state, found = activity(i), true
		}
	}
	if !found {
		return fmt.Errorf("unknown state %q", *state)
	}
	if *width < 72 || *width > 240 || *height < 28 || *height > 100 {
		return fmt.Errorf("export dimensions must be 72..240 columns and 28..100 rows")
	}
	if math.IsNaN(*at) || math.IsInf(*at, 0) || *at < 0 || *at > 3600 || math.IsNaN(*seconds) || *seconds < 1 || *seconds > 20 {
		return fmt.Errorf("at must be 0..3600 and seconds must be 1..20")
	}
	if *snapshot != "" && *animation != "" {
		return fmt.Errorf("choose either --snapshot or --gif")
	}
	p.gallery, p.paused, p.palette = *view == "gallery", *paused, *palette
	p.SetRect(0, 0, *width, *height)
	if *snapshot != "" {
		p.t = *at
		return savePreview(*snapshot, p)
	}
	if *animation != "" {
		return saveGIF(*animation, p, *seconds)
	}
	if err := ui.Init(); err != nil {
		return err
	}
	defer ui.Close()
	ui.DefaultBackend.Screen.HideCursor()
	ui.DefaultBackend.Screen.EnableMouse(tcell.MouseButtonEvents | tcell.MouseDragEvents)
	events := ui.PollEvents()
	ticker := time.NewTicker(time.Second / 30)
	defer ticker.Stop()
	last := time.Now()
	for {
		w, h := ui.TerminalDimensions()
		p.SetRect(0, 0, w, h)
		ui.Render(p)
		var tick <-chan time.Time
		moving := p.moving()
		if moving {
			tick = ticker.C
		}
		select {
		case e, ok := <-events:
			if !ok || p.handle(e) {
				return nil
			}
		case <-tick:
		}
		now := time.Now()
		if moving && p.moving() {
			p.t += math.Min(.1, now.Sub(last).Seconds()) * p.speed
		}
		last = now
	}
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
