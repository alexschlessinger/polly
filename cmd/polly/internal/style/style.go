package style

import (
	"image"
	"strings"

	tcell "github.com/gdamore/tcell/v3"
	ui "github.com/metaspartan/gotui/v5"
	"github.com/metaspartan/gotui/v5/widgets"
)

// Package style is the transcript text vocabulary shared by every polly
// surface: gotui inline markup and its bracket escaping, cell wrapping, and
// the sidecar image slots that Markdown rendering leaves in styled text.

// The masthead bird's palette, registered by name below.
var (
	pollyGreen = tcell.NewRGBColor(0x01, 0xab, 0x46)
	pollyLight = tcell.NewRGBColor(0x57, 0xcd, 0x75)
	pollyWing  = tcell.NewRGBColor(0xb7, 0xd0, 0x19)
	pollyCrown = tcell.NewRGBColor(0xff, 0x2b, 0x23)
	pollyBeak  = tcell.NewRGBColor(0xff, 0xd5, 0x1d)
	pollyMouth = tcell.NewRGBColor(0xff, 0x60, 0x0d)
	pollyFace  = tcell.NewRGBColor(0xec, 0x80, 0xaf)
	pollyEye   = tcell.NewRGBColor(0x31, 0x2f, 0x2a)
	pollyFoot  = tcell.NewRGBColor(0xfe, 0xba, 0x02)
)

// init registers polly's semantic accent colors. Each name maps to an ANSI
// palette slot (XTerm 0–15) that the terminal (e.g. Ghostty) remaps to the
// active theme — unlike gotui's dark*/cyan names, which resolve to fixed RGB
// (e.g. darkred = 0x8B0000) and ignore the theme. Quiet variants are produced
// with the "dim" modifier at the call site, not a darker fixed color.
func init() {
	ui.StyleParserColorMap["ok"] = ui.ColorGreen      // success ✓
	ui.StyleParserColorMap["err"] = ui.ColorRed       // failure ✗ / errors
	ui.StyleParserColorMap["run"] = ui.ColorTeal      // running-tool arrow (ANSI cyan, XTerm6)
	ui.StyleParserColorMap["accent"] = ui.ColorBlue   // prompts & interactive markers
	ui.StyleParserColorMap["active"] = ui.ColorYellow // status-bar active turn
	ui.StyleParserColorMap["muted"] = ui.ColorGrey    // metadata (ANSI bright-black, XTerm8)
	ui.StyleParserColorMap["code"] = ui.ColorWhite    // fenced code block contents
	// The masthead bird is the one place the TUI paints true color: its
	// palette registers by name so the bird is ordinary styled text.
	ui.StyleParserColorMap["polly-green"] = pollyGreen
	ui.StyleParserColorMap["polly-light"] = pollyLight
	ui.StyleParserColorMap["polly-wing"] = pollyWing
	ui.StyleParserColorMap["polly-crown"] = pollyCrown
	ui.StyleParserColorMap["polly-beak"] = pollyBeak
	ui.StyleParserColorMap["polly-mouth"] = pollyMouth
	ui.StyleParserColorMap["polly-face"] = pollyFace
	ui.StyleParserColorMap["polly-eye"] = pollyEye
	ui.StyleParserColorMap["polly-foot"] = pollyFoot
}

// gotui's ParseStyles has no escape: it enters styled-text mode on any '[',
// and leaves only once brackets balance and a '(' follows. A balanced pair
// renders literally, but one stray '[' swallows every later rune of the
// entry and drops the last one. So text bound for the parser never carries
// real brackets: styleEscape swaps them for two Unicode noncharacters, which
// never occur in text and are width 1 in every locale (the private-use
// range is double width under East Asian locales), and parseStyledCells or a
// literalParagraph restores them once cell styles are assigned.
const (
	styledLiteralOpenBracket  rune = '\ufdd0'
	styledLiteralCloseBracket rune = '\ufdd1'
)

var styledLiteralBracketReplacer = strings.NewReplacer(
	"[", string(styledLiteralOpenBracket),
	"]", string(styledLiteralCloseBracket),
)

// Escape makes s inert to gotui's style parser. Callers building markup
// by hand apply it to every run of literal text; styled applies it itself.
func Escape(s string) string {
	return styledLiteralBracketReplacer.Replace(s)
}

// StyledBg is styled with a background color as well; both names resolve
// through StyleParserColorMap.
func StyledBg(text, fg, bg string) string {
	if text == "" {
		return ""
	}
	return "[" + Escape(text) + "](fg:" + fg + ",bg:" + bg + ")"
}

// Styled wraps text in gotui's inline style markup. Color names come from
// gotui's StyleParserColorMap; empty fg/modifier means no styling. The text is
// run through styleEscape — callers don't need to pre-sanitize.
func Styled(text, fg, modifier string) string {
	if text == "" {
		return ""
	}
	text = Escape(text)
	parts := []string{}
	if fg != "" {
		parts = append(parts, "fg:"+fg)
	}
	if modifier != "" {
		parts = append(parts, "mod:"+modifier)
	}
	if len(parts) == 0 {
		return text
	}
	return "[" + text + "](" + strings.Join(parts, ",") + ")"
}

// Link marks text the user can click. A Link is the accent color and nothing
// else: the terminal has no underline through gotui, and brackets would read
// as literal text.
func Link(label string) string {
	return Styled(label, "accent", "")
}

// KeyHints renders key and verb pairs the way dialog footers do: the key in
// the text color, its verb muted, pairs joined by a muted middle dot. A pair
// with an empty verb shows the key alone.
func KeyHints(pairs ...[2]string) string {
	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		part := Escape(p[0])
		if p[1] != "" {
			part += " " + Styled(p[1], "muted", "")
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, Styled(" · ", "muted", ""))
}

// KeyHintsText is the unstyled text of keyHints, for width math.
func KeyHintsText(pairs ...[2]string) string {
	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		part := p[0]
		if p[1] != "" {
			part += " " + p[1]
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, " · ")
}

// styledLiteralRune maps a substitute rune back to the bracket it stands for.
func styledLiteralRune(r rune) (rune, bool) {
	switch r {
	case styledLiteralOpenBracket:
		return '[', true
	case styledLiteralCloseBracket:
		return ']', true
	}
	return r, false
}

func ParseCells(text string, defaultStyle ui.Style) []ui.Cell {
	cells := ui.ParseStyles(text, defaultStyle)
	for i := range cells {
		cells[i].Rune, _ = styledLiteralRune(cells[i].Rune)
	}
	return cells
}

// LiteralParagraph is a gotui Paragraph for text built with styled and
// styleEscape. The stock widget parses its Text itself, so the substitute
// runes would reach the screen; Draw restores them in the buffer afterwards.
type LiteralParagraph struct{ *widgets.Paragraph }

func NewLiteralParagraph() *LiteralParagraph {
	return &LiteralParagraph{Paragraph: widgets.NewParagraph()}
}

func (p *LiteralParagraph) Draw(buf *ui.Buffer) {
	p.Paragraph.Draw(buf)
	RestoreLiterals(buf, p.Inner)
}

// RestoreLiterals rewrites the substitute runes within rect back to
// brackets.
func RestoreLiterals(buf *ui.Buffer, rect image.Rectangle) {
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		for x := rect.Min.X; x < rect.Max.X; x++ {
			pt := image.Pt(x, y)
			cell := buf.GetCell(pt)
			if r, ok := styledLiteralRune(cell.Rune); ok {
				cell.Rune = r
				buf.SetCell(cell, pt)
			}
		}
	}
}
