package main

import (
	"strings"

	ui "github.com/metaspartan/gotui/v5"
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

// styleEscape neutralizes the only sequence gotui's ParseStyles treats as
// style markup: a "[...](...)" run. gotui has no backslash escape — balanced
// brackets already render literally via its nesting counter — so we just break
// a "](" adjacency (e.g. a markdown link in model output) with a zero-width
// space, leaving every other character untouched. Adding backslashes would be
// wrong: gotui renders them verbatim.
func styleEscape(s string) string {
	return strings.ReplaceAll(s, "](", "]\u200b(")
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

// Chroma can split source punctuation into individual tokens. A token that is
// literally "[" or "]" cannot be nested safely inside gotui's own
// [text](style) syntax, so code rendering substitutes private runes until after
// ParseStyles has assigned cell styles. The transcript parser restores the
// original source characters before wrapping or drawing.
const (
	styledLiteralOpenBracket  rune = '\ue100'
	styledLiteralCloseBracket rune = '\ue101'
)

var styledLiteralBracketReplacer = strings.NewReplacer(
	"[", string(styledLiteralOpenBracket),
	"]", string(styledLiteralCloseBracket),
)

func styledCodeLiteral(text, fg, modifier string) string {
	text = styledLiteralBracketReplacer.Replace(text)
	return styled(text, fg, modifier)
}

func parseStyledCells(text string, defaultStyle ui.Style) []ui.Cell {
	cells := ui.ParseStyles(text, defaultStyle)
	for i := range cells {
		switch cells[i].Rune {
		case styledLiteralOpenBracket:
			cells[i].Rune = '['
		case styledLiteralCloseBracket:
			cells[i].Rune = ']'
		}
	}
	return cells
}
