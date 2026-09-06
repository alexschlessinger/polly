// textfx is an isolated gotui/tcell playground; it does not start Polly or a provider.
package main

import (
	"flag"
	"fmt"
	"image"
	"image/draw"
	"image/gif"
	"math"
	"os"
	"strings"
	"time"

	ui "github.com/metaspartan/gotui/v5"
)

type playground struct {
	ui.Block
	t             float64
	speed         float64
	page          int
	paused, theme bool
}

const effectsPerPage = 6

var names = []string{
	"LIGHT SWEEP", "BREATH", "AURORA", "DECODE", "WAVE", "SCANNER",
	"TYPEWRITER", "COMET", "TWINKLE", "RIPPLE", "COLOR WIPE", "SPLIT FLAP",
	"PROMPT GRAVITY", "AGENT CIRCUIT", "PATCH STITCH", "TOOL SONAR", "CONTEXT FOLD", "SOFT LANDING",
	"DISCLOSURE", "COUNT SETTLE", "CALLER BEACON", "CONTEXT EDGE", "QUEUE RECEIPT", "READY CURSOR",
}
var notes = []string{
	"A soft highlight travels across otherwise steady text.",
	"Slow, synchronized brightness. Quiet enough for waiting.",
	"A flowing blue / violet / mint gradient.",
	"Scrambled glyphs resolve into a message, then hold.",
	"Whole-cell vertical motion. For a splash, probably not prose.",
	"A moving background band; the words stay readable.",
	"A caret types, rests, then erases the line.",
	"A bright head leaves a long, fading cyan tail.",
	"Scattered warm glints, like stars catching the light.",
	"A pulse spreads outward, lifting letters as it passes.",
	"Amber hands off to mint, one letter at a time.",
	"An amber departure board: each tile flips and settles.",
	"A scattered thought gathers at the prompt, ready to send.",
	"Three branches carry work out and reports back to caller.",
	"A luminous needle stitches a deletion into an addition.",
	"Search sends out a pulse; matching lines light up in its wake.",
	"Old turns fold into a small memory for the next request.",
	"Busy work settles into a receipt, and the prompt wakes up.",
	"Focus gently lights the chevron; opening reveals the row.",
	"A new completed count gets a brief tint, then settles.",
	"A report arrives; a short glint travels toward the caller.",
	"New context lights only the meter's newly filled edge.",
	"Queued warms once, then quietly clears when picked up.",
	"An idle caret breathes slowly; the prompt stays steady.",
}

func rgb(r, g, b int32) ui.Color { return ui.NewColorRGB(r, g, b) }
func mix(a, b ui.Color, f float64) ui.Color {
	f = math.Max(0, math.Min(1, f))
	ar, ag, ab := a.RGB()
	br, bg, bb := b.RGB()
	return rgb(int32(float64(ar)+(float64(br-ar)*f)), int32(float64(ag)+(float64(bg-ag)*f)), int32(float64(ab)+(float64(bb-ab)*f)))
}

func (p *playground) colors() (bg, fg, muted ui.Color) {
	if p.theme {
		return ui.ColorClear, ui.ColorClear, ui.ColorGrey
	}
	return rgb(25, 27, 38), rgb(204, 211, 234), rgb(118, 129, 156)
}

func put(buf *ui.Buffer, x, y int, text string, style ui.Style) {
	for _, r := range text {
		if image.Pt(x, y).In(buf.Rectangle) {
			buf.SetCell(ui.Cell{Rune: r, Style: style}, image.Pt(x, y))
		}
		x++ // All sampler text is single-column; no arbitrary user input.
	}
}

func (p *playground) effect(buf *ui.Buffer, x, y, kind int, text string) {
	bg, fg, _ := p.colors()
	blue, violet, mint := rgb(113, 159, 255), rgb(193, 141, 255), rgb(106, 229, 201)
	amber := rgb(246, 191, 105)
	if kind == 11 {
		text = strings.ToUpper(text)
	}
	runes := []rune(text)
	for i, r := range runes {
		pos := float64(i)
		t := p.t
		st := ui.NewStyle(blue, bg)
		dy := 0
		brightness := .5
		cursor, flipping := false, false
		switch kind {
		case 0:
			center := math.Mod(t*12, float64(len(runes))+20) - 10
			brightness = math.Exp(-math.Pow((pos-center)/3, 2))
			st.Fg = mix(rgb(92, 117, 171), rgb(237, 248, 255), brightness)
		case 1:
			brightness = (1 - math.Cos(t*math.Pi/2)) / 2
			st.Fg = mix(rgb(86, 108, 155), blue, brightness)
		case 2:
			phase := (math.Sin(pos*.12-t*1.1) + 1) / 2
			brightness = phase
			if phase < .5 {
				st.Fg = mix(blue, violet, phase*2)
			} else {
				st.Fg = mix(violet, mint, (phase-.5)*2)
			}
		case 3:
			progress := math.Mod(t, 6) * float64(len(runes)) / 2.4
			if pos > progress && r != ' ' {
				chars := []rune("01/:+*?<>#")
				r = chars[(i*7+int(t*13))%len(chars)]
				st.Fg = rgb(88, 102, 139)
			} else {
				st.Fg = mint
				brightness = 1
			}
		case 4:
			phase := math.Sin(pos*.3 - t*2)
			dy = int(math.Round(phase))
			brightness = (phase + 1) / 2
			st.Fg = mix(blue, mint, brightness)
		case 5:
			center := (math.Sin(t*1.1) + 1) / 2 * float64(len(runes)-1)
			brightness = math.Exp(-math.Pow((pos-center)/2.5, 2))
			st.Fg = fg
			st.Bg = mix(bg, rgb(57, 83, 132), brightness)
		case 6:
			phase := math.Mod(t, 6)
			count := len(runes)
			if phase < 2.8 {
				count = int(phase / 2.8 * float64(len(runes)))
			} else if phase > 4.5 {
				count = max(0, int((5.8-phase)/1.3*float64(len(runes))))
			}
			st.Fg = mint
			if i >= count {
				r = ' '
				cursor = i == count && int(t*3)%2 == 0
				if cursor {
					st.Bg = mint
				}
			}
		case 7:
			head := math.Mod(t*14, float64(len(runes))+24) - 4
			behind := head - pos
			brightness = math.Exp(-math.Abs(behind) * 3)
			if behind >= 0 {
				brightness = math.Exp(-behind / 7)
			}
			st.Fg = mix(rgb(65, 80, 115), mint, brightness)
			if math.Abs(behind) < .7 {
				st.Fg = rgb(236, 255, 252)
			}
		case 8:
			phase := math.Mod(t/3.6+pos*.61803398875, 1)
			brightness = math.Pow((1+math.Cos(phase*2*math.Pi))/2, 24)
			st.Fg = mix(rgb(106, 119, 151), amber, brightness)
		case 9:
			distance := math.Abs(pos - float64(len(runes)-1)/2)
			radius := math.Mod(t*8, float64(len(runes))/2+12) - 3
			brightness = math.Exp(-math.Pow((distance-radius)/1.8, 2))
			st.Fg = mix(rgb(111, 113, 169), rgb(226, 206, 255), brightness)
			if brightness > .6 {
				dy = -1
			}
		case 10:
			cycle := int(t / 3)
			front := math.Mod(t, 3)/2*float64(len(runes)+6) - 3
			brightness = math.Max(0, math.Min(1, (front-pos+1)/2))
			from, to := amber, mint
			if cycle%2 != 0 {
				from, to = to, from
			}
			st.Fg = mix(from, to, brightness)
		case 11:
			age := math.Mod(t, 6) - pos*.045
			st.Fg = amber
			st.Bg = rgb(42, 38, 36)
			if r == ' ' {
				st.Bg = bg
			} else if age < 0 {
				r = '-'
				brightness = .2
			} else if age < .9 {
				tick := int(age * 18)
				r = rune('A' + (i*7+tick)%26)
				flipping = tick%3 == 0
				if flipping {
					r = '-'
					st.Fg = rgb(255, 226, 162)
					st.Bg = rgb(83, 60, 39)
				}
			} else {
				brightness = 1
			}
		}
		if p.theme {
			st = ui.NewStyle(ui.ColorBlue, bg)
			if brightness < .35 {
				st.Modifier = ui.ModifierDim
			}
			if brightness > .8 {
				st.Modifier = ui.ModifierBold
			}
			if kind == 5 && brightness > .5 {
				st.Modifier = ui.ModifierReverse
			}
			if kind == 8 || kind == 11 {
				st.Fg = ui.ColorYellow
			}
			if kind == 10 {
				st.Fg = ui.ColorYellow
				if (brightness >= .5) != (int(t/3)%2 != 0) {
					st.Fg = ui.ColorGreen
				}
			}
			if cursor || flipping {
				st.Modifier = ui.ModifierReverse
			}
		}
		put(buf, x+i, y+dy, string(r), st)
	}
	if kind == 6 && math.Mod(p.t, 6) >= 2.8 && math.Mod(p.t, 6) <= 4.5 && int(p.t*3)%2 == 0 {
		st := ui.NewStyle(bg, mint)
		if p.theme {
			st = ui.NewStyle(ui.ColorBlue, bg, ui.ModifierReverse)
		}
		put(buf, x+len(runes), y, " ", st)
	}
}

func (p *playground) Draw(buf *ui.Buffer) {
	bg, fg, muted := p.colors()
	for y := buf.Min.Y; y < buf.Max.Y; y++ {
		for x := buf.Min.X; x < buf.Max.X; x++ {
			put(buf, x, y, " ", ui.NewStyle(fg, bg))
		}
	}
	if buf.Dx() < 60 || buf.Dy() < 34 {
		put(buf, 2, 2, "Text lab needs at least 60 columns x 34 rows.", ui.NewStyle(fg, bg))
		put(buf, 2, 4, "Resize to continue; q quits.", ui.NewStyle(muted, bg))
		return
	}
	put(buf, 3, 1, "P O L L Y   /   T E X T   L A B", ui.NewStyle(fg, bg, ui.ModifierBold))
	heading := fmt.Sprintf("%d ways to make text move.    Collection %d / %d", len(names), p.page+1, len(names)/effectsPerPage)
	if p.page == 2 {
		heading = fmt.Sprintf("REPL micro-scenes.    Collection 3 / %d", len(names)/effectsPerPage)
	} else if p.page == 3 {
		heading = fmt.Sprintf("Quiet affordances.    Collection 4 / %d", len(names)/effectsPerPage)
	}
	put(buf, 3, 3, heading, ui.NewStyle(muted, bg))
	for i := 0; i < effectsPerPage; i++ {
		kind := p.page*effectsPerPage + i
		y := 6 + i*4
		put(buf, 3, y, fmt.Sprintf("%02d  %s", kind+1, names[kind]), ui.NewStyle(muted, bg))
		if kind >= 18 {
			p.affordanceEffect(buf, 22, y, kind-18)
		} else if kind >= 12 {
			p.replEffect(buf, 22, y, kind-12)
		} else {
			p.effect(buf, 22, y, kind, "Thinking through the possibilities")
		}
		put(buf, 22, y+2, notes[kind], ui.NewStyle(muted, bg))
	}
	mode, state := "RGB", "playing"
	if p.theme {
		mode = "terminal palette"
	}
	if p.paused {
		state = "paused"
	}
	put(buf, 3, 30, "left/right or 1/2/3/4 collection", ui.NewStyle(fg, bg))
	put(buf, 3, 31, "space pause   +/- speed   c color   r restart   q quit", ui.NewStyle(fg, bg))
	put(buf, 3, 33, fmt.Sprintf("%s  /  %.2gx  /  %s", mode, p.speed, state), ui.NewStyle(muted, bg))
}

func run() error {
	snapshot := flag.String("snapshot", "", "save a PNG of the actual gotui cell buffer")
	animation := flag.String("gif", "", "save a six-second animated preview of the cell buffer")
	page := flag.Int("page", 1, "collection to display or export (1, 2, 3, or 4)")
	flag.Parse()
	if *page < 1 || *page > len(names)/effectsPerPage {
		return fmt.Errorf("page must be between 1 and %d", len(names)/effectsPerPage)
	}
	p := &playground{Block: *ui.NewBlock(), speed: 1, page: *page - 1}
	p.SetRect(0, 0, 88, 35)
	if *snapshot != "" {
		p.t = 1.4
		return savePreview(*snapshot, p)
	}
	if *animation != "" {
		renderer := newPreviewRenderer()
		defer renderer.close()
		out := &gif.GIF{LoopCount: 0}
		for i := 0; i < 72; i++ {
			p.t = float64(i) / 12
			img := renderer.capture(p)
			frame := image.NewPaletted(img.Bounds(), previewPalette())
			draw.Draw(frame, frame.Bounds(), img, image.Point{}, draw.Src)
			out.Image = append(out.Image, frame)
			out.Delay = append(out.Delay, 8+i%3/2) // 8, 8, 9 centiseconds = 12 fps.
		}
		f, err := os.Create(*animation)
		if err != nil {
			return err
		}
		defer f.Close()
		return gif.EncodeAll(f, out)
	}
	if err := ui.Init(); err != nil {
		return err
	}
	defer ui.Close()
	ui.DefaultBackend.Screen.HideCursor()
	events := ui.PollEvents()
	ticker := time.NewTicker(time.Second / 30)
	defer ticker.Stop()
	last := time.Now()
	for {
		w, h := ui.TerminalDimensions()
		p.SetRect(0, 0, w, h)
		ui.Render(p)
		select {
		case e, ok := <-events:
			if !ok {
				return nil
			}
			switch e.ID {
			case "q", "<Escape>", "<C-c>":
				return nil
			case " ", "<Space>":
				p.paused = !p.paused
			case "+", "=":
				p.speed = math.Min(4, p.speed*2)
			case "-":
				p.speed = math.Max(.125, p.speed/2)
			case "c":
				p.theme = !p.theme
			case "r":
				p.t = 0
			case "<Right>", "<Tab>":
				p.page = (p.page + 1) % (len(names) / effectsPerPage)
				p.t = 0
			case "<Left>":
				p.page = (p.page + len(names)/effectsPerPage - 1) % (len(names) / effectsPerPage)
				p.t = 0
			case "1", "2", "3", "4":
				p.page = int(e.ID[0] - '1')
				p.t = 0
			}
		case <-ticker.C:
		}
		now := time.Now()
		if !p.paused {
			p.t += now.Sub(last).Seconds() * p.speed
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
