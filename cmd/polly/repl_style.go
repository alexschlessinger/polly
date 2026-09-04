package main

import (
	"image"
	"strings"

	ui "github.com/metaspartan/gotui/v5"
	"github.com/metaspartan/gotui/v5/widgets"
)

// Inline style markup: the gotui color roles and the [text](style) helpers.

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

// styleEscape makes s inert to gotui's style parser. Callers building markup
// by hand apply it to every run of literal text; styled applies it itself.
func styleEscape(s string) string {
	return styledLiteralBracketReplacer.Replace(s)
}

// styled wraps text in gotui's inline style markup. Color names come from
// gotui's StyleParserColorMap; empty fg/modifier means no styling. The text is
// run through styleEscape — callers don't need to pre-sanitize.
func styled(text, fg, modifier string) string {
	if text == "" {
		return ""
	}
	text = styleEscape(text)
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

func parseStyledCells(text string, defaultStyle ui.Style) []ui.Cell {
	cells := ui.ParseStyles(text, defaultStyle)
	for i := range cells {
		cells[i].Rune, _ = styledLiteralRune(cells[i].Rune)
	}
	return cells
}

// literalParagraph is a gotui Paragraph for text built with styled and
// styleEscape. The stock widget parses its Text itself, so the substitute
// runes would reach the screen; Draw restores them in the buffer afterwards.
type literalParagraph struct{ *widgets.Paragraph }

func newLiteralParagraph() *literalParagraph {
	return &literalParagraph{Paragraph: widgets.NewParagraph()}
}

func (p *literalParagraph) Draw(buf *ui.Buffer) {
	p.Paragraph.Draw(buf)
	restoreStyledLiterals(buf, p.Inner)
}

// restoreStyledLiterals rewrites the substitute runes within rect back to
// brackets.
func restoreStyledLiterals(buf *ui.Buffer, rect image.Rectangle) {
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
