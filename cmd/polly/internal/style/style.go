// Package style is the transcript text vocabulary shared by every polly
// surface: gotui inline markup and its bracket escaping, cell wrapping, the
// theme role table that resolves those markup color names, and the sidecar
// image slots that Markdown rendering leaves in styled text.
package style

import (
	"image"
	"strings"

	tcell "github.com/gdamore/tcell/v3"
	ui "github.com/metaspartan/gotui/v5"
	"github.com/metaspartan/gotui/v5/widgets"
)

// The masthead bird's palette. Besides being plain ui.Color values, these are
// part of the built-in theme's mapping (see builtinColors in theme.go) and
// register by name in the parser map like every other role.
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

// init applies polly's built-in theme. That registers every role name into
// ui.StyleParserColorMap — the table style.Styled emits into gotui's markup and
// chromeColor paints cells from — so a run with no theme flag resolves exactly
// the colors polly registered by hand before themes existed, plus the seven
// syn-* token roles, which are unset and follow their fallback role. The rules
// live in theme.go.
func init() {
	Apply(DefaultTheme())
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
	switch {
	case fg != "" && modifier != "":
		return "[" + text + "](fg:" + fg + ",mod:" + modifier + ")"
	case fg != "":
		return "[" + text + "](fg:" + fg + ")"
	case modifier != "":
		return "[" + text + "](mod:" + modifier + ")"
	default:
		return text
	}
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
