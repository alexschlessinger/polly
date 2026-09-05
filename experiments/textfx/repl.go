package main

import (
	"fmt"
	"image"
	"math"
	"strings"

	ui "github.com/metaspartan/gotui/v5"
)

// These are scripted REPL moments, not live agent/tool events. Each scene
// owns three rows; its label, caption, and neighboring scenes stay still.
type scene struct {
	p    *playground
	buf  *ui.Buffer
	x, y int
	t    float64
}

const (
	inkBody = iota
	inkBlue
	inkMint
	inkViolet
	inkAmber
	inkRed
	inkQuiet
)

func clamp(v float64) float64 { return math.Max(0, math.Min(1, v)) }
func ease(v float64) float64  { v = clamp(v); return v * v * (3 - 2*v) }

func (s scene) style(ink int, light float64) ui.Style {
	bg, _, _ := s.p.colors()
	colors := []ui.Color{
		rgb(204, 211, 234), rgb(113, 159, 255), rgb(106, 229, 201),
		rgb(193, 141, 255), rgb(246, 191, 105), rgb(246, 123, 149), rgb(118, 129, 156),
	}
	if s.p.theme {
		colors = []ui.Color{ui.ColorClear, ui.ColorBlue, ui.ColorGreen, ui.ColorMagenta, ui.ColorYellow, ui.ColorRed, ui.ColorGrey}
		st := ui.NewStyle(colors[ink], bg)
		if light < .45 {
			st.Modifier = ui.ModifierDim
		} else if light > .95 {
			st.Modifier = ui.ModifierBold
		}
		return st
	}
	return ui.NewStyle(mix(bg, colors[ink], light), bg)
}

func (s scene) text(x, y int, text string, ink int, light float64) {
	put(s.buf, s.x+x, s.y+y, text, s.style(ink, light))
}

func (p *playground) replEffect(buf *ui.Buffer, x, y, kind int) {
	s := scene{p: p, buf: buf, x: x, y: y, t: math.Mod(p.t, 6)}
	switch kind {
	case 0:
		s.promptGravity()
	case 1:
		s.agentCircuit()
	case 2:
		s.patchStitch()
	case 3:
		s.toolSonar()
	case 4:
		s.contextFold()
	case 5:
		s.softLanding()
	}
}

func (s scene) promptGravity() {
	s.text(0, 0, ">", inkBlue, .7+.3*math.Sin(s.t*2)*math.Sin(s.t*2))
	text := "explain this diff"
	for i, r := range text {
		if r == ' ' {
			continue
		}
		arrive := ease((s.t - float64(i)*.04) / 1.9)
		direction := 1 - (i%2)*2
		x := 2 + i + int(math.Round((1-arrive)*float64(8+i%5)))
		y := int(math.Round((1 - arrive) * math.Sin(float64(i)*1.7+s.t) * float64(direction)))
		if arrive < .96 {
			s.text(x+2, -y, "·", inkViolet, (1-arrive)*.4)
		}
		s.text(x, y, string(r), inkBlue, .3+.7*arrive)
	}
	if s.t > 2.7 {
		st := s.style(inkBlue, 1)
		st.Modifier = ui.ModifierReverse
		if int(s.t*2)%2 == 0 {
			put(s.buf, s.x+2+len(text), s.y, " ", st)
		}
		s.text(27, 0, "↵ send", inkQuiet, ease(s.t-2.7)*.8)
	}
}

// pulse paints a head and fading tail on a precomputed cell route. Keeping
// motion on the wires lets the agent names and counts remain readable.
func (s scene) pulse(path []image.Point, progress float64, ink int) {
	if progress < 0 || progress > 1 {
		return
	}
	head := int(progress * float64(len(path)-1))
	for tail := 3; tail >= 0; tail-- {
		i := head - tail
		if i < 0 {
			continue
		}
		glyph := "·"
		if tail == 0 {
			glyph = "●"
		}
		s.text(path[i].X, path[i].Y, glyph, ink, 1-float64(tail)*.23)
	}
}

func (s scene) agentCircuit() {
	s.text(0, 0, "caller", inkBlue, 1)
	s.text(6, 0, "───┼────", inkQuiet, .45)
	s.text(9, -1, "╭────", inkQuiet, .45)
	s.text(9, 1, "╰────", inkQuiet, .45)
	s.text(22, -1, "───────────╮", inkQuiet, .45)
	s.text(22, 0, "───────────┼──", inkQuiet, .45)
	s.text(22, 1, "───────────╯", inkQuiet, .45)
	finished := 0
	for lane, name := range []string{"search", "build", "test"} {
		dy := lane - 1
		start := .15 + float64(lane)*.3
		returnAt := 2.2 + float64(lane)*.65
		ink := []int{inkBlue, inkViolet, inkMint}[lane]
		light := .5
		if s.t > start+.9 {
			light = .7 + .3*math.Pow(math.Sin(s.t*3+float64(lane)), 2)
		}
		if s.t > returnAt+1 {
			finished++
			light = 1
			s.text(21, dy, "✓", inkMint, 1)
		}
		s.text(14, dy, name, ink, light)
		out := []image.Point{}
		for x := 6; x <= 9; x++ {
			out = append(out, image.Pt(x, 0))
		}
		for x := 9; x <= 13; x++ {
			out = append(out, image.Pt(x, dy))
		}
		s.pulse(out, (s.t-start)/.9, ink)
		back := []image.Point{}
		for x := 22; x <= 33; x++ {
			back = append(back, image.Pt(x, dy))
		}
		for x := 33; x <= 35; x++ {
			back = append(back, image.Pt(x, 0))
		}
		s.pulse(back, s.t-returnAt, ink)
	}
	s.text(37, 0, fmt.Sprintf("%d/3 reports", finished), inkMint, .5+float64(finished)/6)
}

func (s scene) patchStitch() {
	old, next := "- max_agents = 4", "+ max_agents = 32"
	front := (s.t-.5)/2.5*float64(len(next)+2) - 1
	for i, r := range old {
		light := .8
		if float64(i) < front {
			light = .25
		}
		s.text(i, -1, string(r), inkRed, light)
	}
	for i, r := range next {
		if float64(i) <= front {
			s.text(i, 1, string(r), inkMint, 1)
		} else {
			s.text(i, 1, "·", inkQuiet, .25)
		}
	}
	if front >= 0 && front < float64(len(next)) {
		x := int(front)
		s.text(x, -1, "┬", inkAmber, 1)
		s.text(x, 0, "│", inkAmber, .9)
		s.text(x, 1, "✦", inkAmber, 1)
	}
	s.text(24, -1, "config.go", inkQuiet, .6)
	if s.t > 3.2 {
		s.text(24, 0, "✓ patch applied", inkMint, ease(s.t-3.2))
	} else {
		s.text(24, 0, "stitching…", inkAmber, .7)
	}
}

func (s scene) toolSonar() {
	s.text(0, -1, "› rg \"TODO\" ./...", inkBlue, 1)
	front := (s.t - .4) * 22
	for row, text := range []string{"src/repl.go:42   TODO: retry", "src/agent.go:18  TODO: cancel"} {
		for i, r := range text {
			distance := float64(i+4) + float64(row)*12
			brightness := .14
			if front > distance {
				brightness = .65 + .35*math.Exp(-(front-distance)/8)
			}
			s.text(2+i, row, string(r), inkMint, brightness)
		}
	}
	// Two expanding wave fronts travel through the empty margin, above and
	// below the result rows. The text itself never shifts with the wave.
	for echo := 0; echo < 2; echo++ {
		x := int(front) - echo*9
		if x > 0 && x < 53 {
			s.text(x, 0, "│", inkBlue, .65-float64(echo)*.25)
			s.text(x+1, 1, "╲", inkBlue, .45-float64(echo)*.15)
		}
	}
	if s.t > 2.4 {
		s.text(37, -1, "2 matches", inkMint, ease(s.t-2.4))
	}
}

func (s scene) contextFold() {
	fold := ease((s.t - 1.1) / 2)
	for row, text := range []string{"read files · plan · changes", "tool output · decisions"} {
		dy := row*2 - 1
		for i, r := range text {
			if r == ' ' {
				continue
			}
			x := int(math.Round(4 + float64(i)*(1-fold) + 12*fold))
			y := int(math.Round(float64(dy) * (1 - fold)))
			if fold < .95 {
				s.text(x, y, string(r), inkViolet, (1-fold)*.8)
			}
		}
	}
	if fold < .7 {
		s.text(8, 0, "⌜", inkViolet, .6)
		s.text(28, 0, "⌟", inkViolet, .6)
	} else {
		width := int(math.Round(8 * (1 - ease((fold-.7)/.3))))
		s.text(11-width, 0, "["+strings.Repeat(" ", width)+"memory"+strings.Repeat(" ", width)+"]", inkViolet, .7+.3*fold)
	}
	if s.t > 3.3 {
		s.text(23, 0, "→  84k to 12k", inkMint, ease(s.t-3.3))
	}
}

func (s scene) softLanding() {
	s.text(0, -1, "Updated the retry logic.", inkBody, 1)
	if s.t < 2.2 {
		spinner := []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")
		s.text(0, 1, string(spinner[int(s.t*10)%len(spinner)]), inkBlue, 1)
		s.text(2, 1, "thought · 2 tools", inkBlue, .75)
		return
	}
	land := s.t - 2.2
	s.text(0, 1, "✓", inkMint, 1)
	for i, text := range []string{"2.7s", " · ", "4.2k in / 214 out"} {
		x := []int{2, 6, 9}[i]
		s.text(x, 1, text, inkQuiet, ease((land-float64(i)*.18)/.7)*.9)
	}
	if land < .7 {
		distance := int(land * 12)
		s.text(1+distance, 0, "·", inkMint, 1-land/.7)
		s.text(1+distance*2, 1, "·", inkMint, (1-land/.7)*.6)
	}
	if land > 1.2 {
		s.text(37, 1, ">", inkBlue, 1)
		st := s.style(inkBlue, .7)
		if int(land*2)%2 == 0 {
			st.Modifier = ui.ModifierReverse
			put(s.buf, s.x+39, s.y+1, " ", st)
		}
	}
}
