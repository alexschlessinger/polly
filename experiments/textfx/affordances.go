package main

import (
	"math"
	"strings"

	ui "github.com/metaspartan/gotui/v5"
)

// Page four borrows the production affordances' wording and placement.
// Only simulated state changes (or focus) trigger an accent. The persistent
// breathing cue belongs to the idle caret, not transcript content.
func (p *playground) affordanceEffect(buf *ui.Buffer, x, y, kind int) {
	s := scene{p: p, buf: buf, x: x, y: y, t: math.Mod(p.t, 6)}
	switch kind {
	case 0:
		s.disclosureHint()
	case 1:
		s.countSettle()
	case 2:
		s.callerBeacon()
	case 3:
		s.contextEdge()
	case 4:
		s.queueReceipt()
	case 5:
		s.readyCursor()
	}
}

// A single smooth emphasis, with a quick onset and a slower release.
// Six-second repetition is only for comparing the demo treatments.
func cue(t, start, duration float64) float64 {
	phase := (t - start) / duration
	if phase <= 0 || phase >= 1 {
		return 0
	}
	if phase < .2 {
		return ease(phase / .2)
	}
	return 1 - ease((phase-.2)/.8)
}

func (s scene) tinted(x, y int, text string, base, highlight int, strength float64) {
	st := s.style(base, .9)
	if s.p.theme {
		// Indexed palette colors cannot express a continuous tint. One bounded
		// emphasis replaces interpolation; text still never shifts or blinks.
		if strength > .4 {
			st = s.style(highlight, .9)
			st.Modifier = ui.ModifierBold
		}
	} else {
		st.Fg = mix(st.Fg, s.style(highlight, 1).Fg, strength*.65)
		st.Bg = mix(st.Bg, s.style(highlight, 1).Fg, strength*.06)
	}
	put(s.buf, s.x+x, s.y+y, text, st)
}

func (s scene) disclosureHint() {
	open := s.t >= 2.3
	glyph := "▸"
	if open {
		glyph = "▾"
	}
	s.text(0, 0, "▸ thought 1.4s", inkBlue, .9)
	s.text(14, 0, " · ", inkQuiet, .8)
	s.tinted(17, 0, glyph, inkBlue, inkBody, cue(s.t, .7, 1.5))
	s.text(19, 0, "3 tools", inkBlue, .9)
	s.text(26, 0, " · ", inkQuiet, .8)
	s.text(29, 0, "▸ 1 image viewed", inkBlue, .9)
	if open {
		s.text(19, 1, "✓ read_file · repl.go", inkQuiet, .8*ease((s.t-2.3)/.25))
	}
}

func (s scene) countSettle() {
	s.text(0, 0, "▸", inkBlue, .9)
	switch {
	case s.t < 1.8:
		s.text(2, 0, "2 agents running, 1 completed", inkBlue, .9)
	case s.t < 4:
		s.text(2, 0, "1 agent running, ", inkBlue, .9)
		s.tinted(19, 0, "2", inkBlue, inkMint, cue(s.t, 1.8, 1.3))
		s.text(20, 0, " completed", inkBlue, .9)
	default:
		s.tinted(2, 0, "3", inkBlue, inkMint, cue(s.t, 4, 1.3))
		s.text(3, 0, " agents", inkBlue, .9)
	}
}

func (s scene) callerBeacon() {
	strength := cue(s.t, 1.1, 1.6)
	s.tinted(0, 0, "←", inkBlue, inkBody, strength)
	s.text(1, 0, " Back to caller", inkBlue, .9)
	front := 27 - (s.t-1.1)*10
	for x := 17; x < 51; x++ {
		light := .36
		if s.t > 1.1 && s.t < 2.7 && x < 28 {
			light += .25 * math.Exp(-math.Pow((float64(x)-front)/2, 2)) * strength
		}
		s.text(x, 0, "─", inkQuiet, light)
	}
	s.text(0, 1, ">", inkBlue, .9)
}

func (s scene) contextEdge() {
	filled, usage := 1, "~18.4k/128k"
	if s.t >= 2.2 {
		filled, usage = 2, "~29.0k/128k"
	}
	s.text(0, 0, "polly · ", inkQuiet, .9)
	s.text(8, 0, usage, inkQuiet, .9)
	for i := 0; i < 10; i++ {
		if i < filled {
			s.tinted(21+i, 0, "█", inkMint, inkBody, 0)
			if i == 1 {
				s.tinted(21+i, 0, "█", inkMint, inkBody, cue(s.t, 2.2, 1.4))
			}
		} else {
			s.text(21+i, 0, "░", inkMint, .6)
		}
	}
}

func (s scene) queueReceipt() {
	s.text(0, 0, ">", inkBlue, .9)
	s.text(2, 0, "also check the Windows path", inkBody, 1)
	if s.t < 3.5 {
		s.tinted(2, 1, "(queued)", inkQuiet, inkAmber, cue(s.t, .25, 1.5))
		return
	}
	// Retain the row geometry while only the ephemeral marker fades away.
	light := .9 * (1 - ease((s.t-3.5)/.4))
	if light > .02 {
		s.text(2, 1, "(queued)", inkQuiet, light)
	}
}

func (s scene) readyCursor() {
	s.text(0, -1, strings.Repeat("─", 51), inkQuiet, .36)
	s.text(0, 0, ">", inkBlue, .9)
	// Keep some cursor visible throughout the cycle, with no all-or-nothing
	// blink. In the real composer, typing would suspend the idle breathing.
	breath := (1 - math.Cos(s.t*math.Pi/3)) / 2
	st := s.style(inkBlue, .9)
	if s.p.theme {
		st.Modifier = ui.ModifierReverse
		if breath < .35 {
			st.Modifier |= ui.ModifierDim
		}
	} else {
		st.Bg = mix(st.Bg, st.Fg, .3+.18*breath)
	}
	put(s.buf, s.x+2, s.y, " ", st)
}
