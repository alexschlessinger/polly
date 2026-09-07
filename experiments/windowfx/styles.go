package main

import (
	"fmt"
	"image"
	"math"
	"strings"

	ui "github.com/metaspartan/gotui/v5"
)

type skin struct {
	bg, fg, muted, edge, accent, second, title ui.Color
}

type windowStyle struct {
	slug, name, note string
	colors           skin
}

var styles = []windowStyle{
	{"split", "Split / origin", "Thin divider · breadcrumb title · plain scroll rail",
		skin{desk, ink, subtle, rgb(68, 83, 101), sea, sea, desk}},
	{"halo", "Halo", "An etched frame catches a single orbiting glint",
		skin{rgb(19, 25, 31), ink, subtle, rgb(57, 78, 87), sea, rgb(229, 255, 250), rgb(24, 35, 41)}},
	{"aurora", "Aurora", "A ribbon of violet, ice and mint folds around the edge",
		skin{rgb(22, 22, 39), rgb(222, 222, 246), rgb(139, 140, 177), rgb(79, 65, 126), rgb(184, 145, 250), sea, rgb(43, 35, 67)}},
	{"phosphor", "Phosphor", "Double-line CRT · scanning title · phosphor scrollbar",
		skin{rgb(10, 23, 17), rgb(174, 219, 163), rgb(91, 145, 101), rgb(53, 115, 66), rgb(149, 246, 112), rgb(222, 255, 158), rgb(26, 49, 26)}},
	{"workbench", "Workbench / 1993", "Raised chrome · blue caption · chunky elevator thumb",
		skin{rgb(33, 38, 48), rgb(218, 220, 230), rgb(147, 158, 177), rgb(136, 150, 171), rgb(148, 195, 255), rgb(218, 234, 255), rgb(42, 68, 133)}},
	{"paper", "Paper tab", "Warm stock · index tab · red bookmark in the margin",
		skin{rgb(238, 228, 204), rgb(67, 65, 57), rgb(132, 119, 96), rgb(171, 156, 126), rgb(170, 65, 48), rgb(204, 147, 75), rgb(223, 208, 172)}},
	{"circuit", "Signal path", "Packets turn corners and travel down a dotted rail",
		skin{rgb(14, 27, 31), rgb(195, 221, 223), rgb(98, 143, 150), rgb(46, 97, 105), rgb(92, 224, 211), rgb(232, 193, 107), rgb(21, 47, 52)}},
	{"tide", "Tidal", "Open horizon · rippling sill · floating scroll buoy",
		skin{rgb(14, 28, 40), rgb(184, 215, 233), rgb(103, 147, 170), rgb(47, 105, 132), rgb(107, 205, 239), rgb(124, 230, 211), rgb(22, 46, 60)}},
	{"blueprint", "Blueprint", "Drafting ticks · measured edges · caliper thumb",
		skin{rgb(21, 44, 77), rgb(199, 222, 243), rgb(122, 163, 199), rgb(75, 123, 164), rgb(181, 220, 252), rgb(109, 181, 224), rgb(31, 62, 103)}},
	{"marquee", "Marquee", "Amber chase lights · ticket title · perforated rail",
		skin{rgb(34, 23, 23), rgb(237, 220, 190), rgb(163, 130, 105), rgb(124, 80, 43), rgb(250, 192, 88), rgb(255, 239, 181), rgb(74, 45, 30)}},
	{"deco", "Velvet / deco", "Gold inlay · centered nameplate · a slow pendulum",
		skin{rgb(32, 25, 41), rgb(226, 215, 231), rgb(153, 130, 160), rgb(115, 89, 66), rgb(222, 184, 112), rgb(248, 221, 163), rgb(46, 32, 51)}},
	{"radar", "Radar", "Corner brackets · sweeping telemetry · range-finder rail",
		skin{rgb(15, 27, 26), rgb(189, 216, 201), rgb(101, 146, 128), rgb(48, 95, 76), rgb(136, 229, 153), rgb(229, 244, 151), rgb(24, 46, 34)}},
}

func (s skin) focus(active bool, state activity) skin {
	if !active {
		s.edge = mix(s.bg, s.edge, .45)
		s.accent = mix(s.bg, s.accent, .45)
		s.second = mix(s.bg, s.second, .45)
		s.title = mix(s.bg, s.title, .45)
		return s
	}
	if state == attention {
		s.accent = rgb(246, 180, 96)
		s.second = rgb(255, 231, 169)
		r, g, b := s.bg.RGB()
		if r+g+b > 570 {
			s.accent, s.second = rgb(149, 67, 28), rgb(183, 97, 33)
		}
	} else if state == complete {
		s.accent = mix(s.accent, rgb(115, 213, 154), .45)
	}
	return s
}

func (p *playground) drawWindow(c canvas, r image.Rectangle, kind, pane int, compact, focused bool) windowGeometry {
	c.clip = c.clip.Intersect(r)
	s := styles[kind].colors.focus(focused, p.state)
	t := p.t
	if !focused || p.state > attention {
		t = 0
	}
	c.fill(r, ' ', s.fg, s.bg)
	p.drawBorder(c, r, kind, s, t)
	p.drawTitle(c, r, kind, pane, s, t, focused)
	g := windowGeometry{
		frame: r,
		body:  image.Rect(r.Min.X+2, r.Min.Y+3, r.Max.X-4, r.Max.Y-2),
		zoom:  image.Rect(r.Max.X-9, r.Min.Y+1, r.Max.X-6, r.Min.Y+2),
		close: image.Rect(r.Max.X-5, r.Min.Y+1, r.Max.X-2, r.Min.Y+2),
	}
	g.track = image.Rect(r.Max.X-2, g.body.Min.Y, r.Max.X-1, g.body.Max.Y)
	lines := sampleRows(pane, g.body.Dx())
	top := p.scroll[pane]
	if compact {
		lines = []sampleLine{{"Tracing the frame, one detail at a time.", "text"}, {"› read_file   repl_inspector_frame.go", "accent"}, {"  3 tools complete · 1 running", "muted"}}
		top = 0
	}
	g.lines = len(lines)
	top = clamp(top, 0, max(0, len(lines)-g.body.Dy()))
	if !compact {
		p.scroll[pane] = top
	}
	body := c
	body.clip = body.clip.Intersect(g.body)
	for row := 0; row < g.body.Dy() && top+row < len(lines); row++ {
		line := lines[top+row]
		fg, bg := s.fg, s.bg
		if line.tone == "muted" {
			fg = s.muted
		} else if line.tone == "accent" {
			fg = s.accent
		}
		if kind == 5 && row%2 == 0 {
			bg = mix(s.bg, s.title, .22)
			body.hline(g.body.Min.X, g.body.Min.Y+row, g.body.Dx(), ' ', fg, bg)
		}
		body.text(g.body.Min.X, g.body.Min.Y+row, g.body.Dx(), line.text, fg, bg, line.tone == "heading")
	}
	total, visible, scrollTop := g.lines, g.body.Dy(), top
	if compact {
		// Gallery thumbs are diagrams of the same proportional scrollbar.
		total, visible, scrollTop = 30, g.body.Dy(), 7
	}
	g.thumb = scrollbar(g.track, total, visible, scrollTop)
	p.drawScrollbar(c, g, kind, s)
	state := activityNames[p.state]
	if p.state == attention {
		state = "! needs review"
	}
	c.text(r.Min.X+2, r.Max.Y-2, r.Dx()-7, state, s.accent, s.bg, false)
	if !compact && r.Dx() >= 38 {
		position := fmt.Sprintf("%d–%d / %d", top+1, min(g.lines, top+g.body.Dy()), g.lines)
		c.text(r.Max.X-4-len([]rune(position)), r.Max.Y-2, len([]rune(position)), position, s.muted, s.bg, false)
	}
	// The complete bottom edge resizes height; the corner marks its affordance.
	grip := []rune{'┘', '╯', '◢', '╝', '▟', '◿', '╱', '≋', '┘', '◆', '╱', '┘'}[kind]
	c.cell(r.Max.X-1, r.Max.Y-1, grip, s.accent, s.bg, 0)
	return g
}

// Perimeter index proceeds clockwise, so a pulse travels around corners rather
// than restarting independently on each edge.
func perimeter(r image.Rectangle, x, y int) int {
	w, h := r.Dx(), r.Dy()
	switch {
	case y == r.Min.Y:
		return x - r.Min.X
	case x == r.Max.X-1:
		return w - 1 + y - r.Min.Y
	case y == r.Max.Y-1:
		return w + h - 2 + r.Max.X - 1 - x
	default:
		return 2*w + h - 3 + r.Max.Y - 1 - y
	}
}

func orbit(index, length int, t, speed, tail float64) float64 {
	behind := math.Mod(t*speed-float64(index)+float64(length)*100, float64(max(1, length)))
	return math.Exp(-behind / tail)
}

func (p *playground) drawBorder(c canvas, r image.Rectangle, kind int, s skin, t float64) {
	w, h := r.Dx(), r.Dy()
	corners, horizontal, vertical := []rune("┌┐└┘"), '─', '│'
	if kind == 1 || kind == 2 || kind == 7 {
		corners = []rune("╭╮╰╯")
	}
	if kind == 3 || kind == 10 {
		corners, horizontal, vertical = []rune("╔╗╚╝"), '═', '║'
	}
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			if x != r.Min.X && x != r.Max.X-1 && y != r.Min.Y && y != r.Max.Y-1 {
				continue
			}
			i, length := perimeter(r, x, y), 2*w+2*h-4
			ch, fg, bg := horizontal, s.edge, s.bg
			if x == r.Min.X || x == r.Max.X-1 {
				ch = vertical
			}
			switch {
			case x == r.Min.X && y == r.Min.Y:
				ch = corners[0]
			case x == r.Max.X-1 && y == r.Min.Y:
				ch = corners[1]
			case x == r.Min.X && y == r.Max.Y-1:
				ch = corners[2]
			case x == r.Max.X-1 && y == r.Max.Y-1:
				ch = corners[3]
			}
			switch kind {
			case 0:
				if x != r.Min.X {
					ch = ' '
				}
			case 1:
				fg = mix(s.edge, s.second, orbit(i, length, t, 22, 7))
			case 2:
				wave := (1 + math.Sin(float64(i)*.08-t*1.4)) / 2
				fg = mix(s.accent, s.second, wave)
				bg = mix(s.bg, fg, .09)
			case 3:
				scan := math.Exp(-math.Pow((float64(y-r.Min.Y)-math.Mod(t*3, float64(h)+5))/1.7, 2))
				fg = mix(s.edge, s.accent, scan)
			case 4:
				ch = '▐'
				if y == r.Min.Y || y == r.Max.Y-1 {
					ch = '▀'
				}
				fg = s.edge
				if y == r.Min.Y {
					fg = mix(s.edge, s.second, orbit(i, length, t, 20, 6))
				}
				if x == r.Max.X-1 || y == r.Max.Y-1 {
					fg = mix(s.bg, s.edge, .27)
				}
			case 5:
				if y == r.Min.Y && x > r.Min.X+19 {
					ch = ' '
				}
				if x == r.Min.X && y > r.Min.Y+2 {
					fg = mix(s.accent, s.bg, .55)
				}
			case 6:
				pulse := orbit(i, length, t, 18, 4)
				fg = mix(s.edge, s.accent, pulse)
				if i%7 == 0 {
					ch = '·'
				}
				if pulse > .83 {
					ch, fg = '▪', s.second
				}
			case 7:
				if y == r.Max.Y-1 {
					wave := (1 + math.Sin(float64(x)*.38-t*2)) / 2
					ch = []rune("▁▂▃▄▅▆")[int(wave*5)]
					fg = mix(s.edge, s.second, wave)
				} else if x > r.Min.X+4 && x < r.Max.X-5 && y == r.Min.Y {
					ch = ' '
				}
			case 8:
				if y == r.Min.Y || y == r.Max.Y-1 {
					ch = '┄'
					if (x-r.Min.X)%10 == 0 {
						ch = '┼'
					}
				} else if (y-r.Min.Y)%5 == 0 {
					ch = '┼'
				}
				fg = mix(s.edge, s.accent, orbit(i, length, t, 14, 2))
			case 9:
				ch = '·'
				if i%2 == 0 {
					ch = '•'
					fg = mix(s.edge, s.accent, .32)
					if (i/2-int(t*7))%5 == 0 {
						ch, fg = '●', s.second
					}
				}
			case 10:
				fg = mix(s.edge, s.accent, .55)
				if y == r.Min.Y || y == r.Max.Y-1 {
					center := float64(r.Min.X) + (1+math.Sin(t*.65))/2*float64(w-1)
					fg = mix(fg, s.second, math.Exp(-math.Pow((float64(x)-center)/3, 2)))
				}
			case 11:
				if x > r.Min.X+4 && x < r.Max.X-5 && (y == r.Min.Y || y == r.Max.Y-1) {
					ch = ' '
				} else if y > r.Min.Y+2 && y < r.Max.Y-3 {
					ch = '·'
				}
				fg = mix(s.edge, s.second, orbit(i, length, t, 25, 16))
			}
			c.cell(x, y, ch, fg, bg, 0)
		}
	}
}

func (p *playground) drawTitle(c canvas, r image.Rectangle, kind, pane int, s skin, t float64, focused bool) {
	x, y, width := r.Min.X+2, r.Min.Y+1, r.Dx()-13
	name := []string{"polly › conversation", "polly › scout"}[pane]
	titleBG, titleFG := s.bg, s.accent
	if kind == 2 || kind == 3 || kind == 4 || kind == 5 || kind == 9 {
		titleBG = s.title
		c.hline(r.Min.X+1, y, r.Dx()-2, ' ', titleFG, titleBG)
	}
	sep, sepColor := '─', s.edge
	switch kind {
	case 0:
		// The reference keeps the production breadcrumb + rule vocabulary.
	case 1:
		name = "●  " + name
	case 2:
		name = "◈  " + name
	case 3:
		name = "SYS:// " + strings.ToUpper([]string{"polly", "scout"}[pane])
		c.hline(r.Min.X+1, y, r.Dx()-2, '░', s.edge, titleBG)
		head := math.Mod(t*10, float64(max(1, width))+12) - 6
		for n := 0; n < width; n++ {
			bg := mix(titleBG, s.accent, .17*math.Exp(-math.Pow((float64(n)-head)/3, 2)))
			c.cell(x+n, y, ' ', s.accent, bg, 0)
		}
	case 4:
		name = "■  " + []string{"Polly / Conversation", "Scout / Inspector"}[pane]
		titleFG = s.second
		sep = '▄'
	case 5:
		name = " / " + strings.ToUpper([]string{"CONVERSATION", "SCOUT / NOTES"}[pane])
		c.text(r.Min.X+1, r.Min.Y, 22, "┌─ FIELD NOTES ───┐", s.edge, s.bg, false)
		sep = '┄'
		// A short underline travels across the index tab.
		pos := int(math.Mod(t*4, 16))
		c.cell(r.Min.X+2+pos, r.Min.Y, '━', s.accent, s.bg, 0)
	case 6:
		name = "● ROOT ── " + strings.ToUpper([]string{"CHAT", "SCOUT"}[pane])
		sep = '┄'
	case 7:
		name = "≈  " + []string{"conversation", "scout"}[pane] + " / drift"
		sep = ' '
	case 8:
		name = "+ " + strings.ToUpper([]string{"CONVERSATION", "SCOUT"}[pane])
		c.text(r.Min.X+3, r.Min.Y, r.Dx()-7, fmt.Sprintf(" %d COL × %d ROW ", r.Dx(), r.Dy()), s.accent, s.bg, false)
		sep = '┄'
	case 9:
		name = "ADMIT ONE / " + strings.ToUpper([]string{"POLLY", "SCOUT"}[pane])
		titleFG, titleBG = s.bg, s.accent
		c.hline(r.Min.X+1, y, r.Dx()-2, ' ', titleFG, titleBG)
		sep = '·'
	case 10:
		name = "◇  " + strings.ToUpper([]string{"CONVERSATION", "SCOUT"}[pane]) + "  ◇"
		x += max(0, (width-len([]rune(name)))/2)
		width = r.Max.X - 11 - x
		sep, sepColor = '═', mix(s.edge, s.bg, .45)
	case 11:
		name = "⌖ " + strings.ToUpper([]string{"BASE", "SCOUT"}[pane]) + " / TRACK 01"
		sep = '·'
	}
	c.hline(r.Min.X+1, y+1, r.Dx()-2, sep, sepColor, s.bg)
	if kind == 6 {
		c.cell(r.Min.X+1+int(math.Mod(t*8, float64(max(1, r.Dx()-2)))), y+1, '▪', s.second, s.bg, 0)
	}
	if kind == 7 || kind == 11 {
		for n := 0; n < r.Dx()-2; n++ {
			phase := orbit(n, r.Dx()-2, t, 12, 7)
			ch := '─'
			if kind == 11 {
				ch = '·'
			}
			c.cell(r.Min.X+1+n, y+1, ch, mix(s.bg, s.accent, .15+.65*phase), s.bg, 0)
		}
	}
	c.text(x, y, width, name, titleFG, titleBG, focused)
	c.text(r.Max.X-9, y, 3, "[+]", titleFG, titleBG, false)
	c.text(r.Max.X-5, y, 3, "[x]", titleFG, titleBG, false)
}

func (p *playground) drawScrollbar(c canvas, g windowGeometry, kind int, s skin) {
	track, thumb := '│', '┃'
	switch kind {
	case 1:
		track, thumb = '┊', '▐'
	case 2:
		track, thumb = '│', '▌'
	case 3:
		track, thumb = '░', '▓'
	case 4:
		track, thumb = '▒', '█'
	case 5:
		track, thumb = '┊', '▐'
	case 6:
		track, thumb = '·', '▪'
	case 7:
		track, thumb = '┊', '┃'
	case 8:
		track, thumb = '┤', '╟'
	case 9:
		track, thumb = '·', '◆'
	case 10:
		track, thumb = '│', '║'
	case 11:
		track, thumb = '┊', '┫'
	}
	c.fill(g.track, track, s.edge, s.bg)
	c.fill(g.thumb, thumb, s.accent, s.bg)
	if kind == 4 {
		c.fill(g.thumb, '≡', s.bg, s.edge)
	} else if kind == 5 && !g.thumb.Empty() {
		c.cell(g.thumb.Min.X, g.thumb.Max.Y-1, '▼', s.accent, s.bg, 0)
	} else if kind == 7 && !g.thumb.Empty() {
		c.cell(g.thumb.Min.X, g.thumb.Min.Y, '○', s.second, s.bg, 0)
	} else if kind == 8 && !g.thumb.Empty() {
		c.cell(g.thumb.Min.X, g.thumb.Min.Y, '┬', s.accent, s.bg, 0)
		c.cell(g.thumb.Min.X, g.thumb.Max.Y-1, '┴', s.accent, s.bg, 0)
	}
}
