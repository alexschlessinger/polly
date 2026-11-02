package style

import (
	"strings"
	"unicode/utf8"

	tcell "github.com/gdamore/tcell/v3"
	ui "github.com/metaspartan/gotui/v5"
)

// parseFlatCells handles the complete, nonnested markup emitted by Styled and
// the Markdown renderer. Count first so even a large transcript needs only its
// final cell buffer, without intermediate rune or styled-cell buffers. Leave
// nested and incomplete markup to gotui's parser, including its rollback rules.
func parseFlatCells(text string, defaultStyle ui.Style) ([]ui.Cell, bool) {
	count := 0
	for remaining := text; remaining != ""; {
		plain, body, _, rest, ok := flatSpan(remaining)
		if !ok {
			return nil, false
		}
		count += utf8.RuneCountInString(plain) + utf8.RuneCountInString(body)
		remaining = rest
	}
	cells := make([]ui.Cell, count)
	index := 0
	write := func(text string, style ui.Style) {
		for _, r := range text {
			r, _ = styledLiteralRune(r)
			cells[index] = ui.Cell{Rune: r, Style: style}
			index++
		}
	}
	for remaining := text; remaining != ""; {
		plain, body, items, rest, _ := flatSpan(remaining)
		write(plain, defaultStyle)
		if body != "" {
			write(body, flatStyle(items, defaultStyle))
		}
		remaining = rest
	}
	return cells, true
}

// flatSpan returns the literal prefix and next complete, unnested style span.
// A remaining literal suffix is returned as plain with an empty rest.
func flatSpan(text string) (plain, body, items, rest string, ok bool) {
	open := strings.IndexByte(text, '[')
	if open < 0 {
		return text, "", "", "", true
	}
	end := strings.IndexByte(text[open+1:], ']')
	if end < 0 {
		return "", "", "", "", false
	}
	end += open + 1
	if strings.Contains(text[open+1:end], "[") || end+1 >= len(text) || text[end+1] != '(' {
		return "", "", "", "", false
	}
	close := strings.IndexByte(text[end+2:], ')')
	if close < 0 {
		return "", "", "", "", false
	}
	close += end + 2
	items = text[end+2 : close]
	// gotui converts the entire input to runes before resolving style names.
	// Retain that behavior for unusual invalid UTF-8 style specifications.
	if !utf8.ValidString(items) {
		return "", "", "", "", false
	}
	return text[:open], text[open+1 : end], items, text[close+1:], true
}

// flatStyle mirrors gotui's readStyle: items are untrimmed, duplicate keys
// replace earlier values, unknown modifiers clear attributes, and color names
// consult the live theme map before falling back to tcell's color parser.
func flatStyle(items string, style ui.Style) ui.Style {
	for item := range strings.SplitSeq(items, ",") {
		key, value, ok := strings.Cut(item, ":")
		if !ok || strings.Contains(value, ":") {
			continue
		}
		switch key {
		case "fg", "bg":
			color, ok := ui.StyleParserColorMap[value]
			if !ok {
				color = tcell.GetColor(value)
			}
			if key == "fg" {
				style.Fg = color
			} else {
				style.Bg = color
			}
		case "mod":
			switch value {
			case "bold":
				style.Modifier = ui.ModifierBold
			case "reverse":
				style.Modifier = ui.ModifierReverse
			case "dim":
				style.Modifier = ui.ModifierDim
			case "blink":
				style.Modifier = ui.ModifierBlink
			case "italic":
				style.Modifier = ui.ModifierItalic
			case "strike":
				style.Modifier = ui.ModifierStrike
			default:
				style.Modifier = 0
			}
		}
	}
	return style
}
