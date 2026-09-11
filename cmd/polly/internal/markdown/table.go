package markdown

import (
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	ui "github.com/metaspartan/gotui/v5"
	"github.com/rivo/uniseg"
	east "github.com/yuin/goldmark/extension/ast"
)

const minimumTableColumn = 12

// Nested block renderers add their gutters after rendering their children.
func insetTableWidth(state *renderState, inset int) func() {
	if state == nil || state.width <= 0 {
		return func() {}
	}
	old := state.width
	state.width = max(1, old-inset)
	return func() { state.width = old }
}

func responsiveTable(rows [][]string, natural []int, table *east.Table, first, cont string, width int) []string {
	width = max(1, width-max(style.TextWidth(first), style.TextWidth(cont)))
	gutter := style.Styled("│ ", "muted", "")
	// Drop decorative chrome when it would consume the entire pane.
	if width <= 2 {
		gutter = ""
	}
	available := width - style.TextWidth(gutter)
	widths := slices.Clone(natural)
	minimum, total := 2*(len(widths)-1), 2*(len(widths)-1)
	for _, w := range widths {
		minimum += min(w, minimumTableColumn)
		total += w
	}
	if minimum > available {
		return prefixLines(stackedTable(rows, available, gutter), first, cont)
	}
	for total > available {
		widest := -1
		for i, w := range widths {
			if w > min(natural[i], minimumTableColumn) && (widest < 0 || w > widths[widest]) {
				widest = i
			}
		}
		if widest < 0 {
			break
		}
		widths[widest]--
		total--
	}
	var out []string
	previousHeight := 1
	for rowIndex, row := range rows {
		cells := make([][]string, len(widths))
		height := 1
		for i, w := range widths {
			value := ""
			if i < len(row) {
				value = row[i]
			}
			cells[i] = wrapTableCell(value, max(1, w))
			height = max(height, len(cells[i]))
		}
		if rowIndex > 1 && (height > 1 || previousHeight > 1) {
			out = append(out, tableBlank(gutter))
		}
		for line := 0; line < height; line++ {
			parts := make([]string, len(widths))
			for i, w := range widths {
				value := ""
				if line < len(cells[i]) {
					value = cells[i][line]
				}
				parts[i] = padTableCell(value, w, tableAlignment(table, i))
			}
			out = append(out, gutter+strings.TrimRight(strings.Join(parts, "  "), " "))
		}
		if rowIndex == 0 {
			rules := make([]string, len(widths))
			for i, w := range widths {
				rules[i] = strings.Repeat("─", w)
			}
			out = append(out, gutter+style.Styled(strings.Join(rules, "  "), "muted", ""))
		}
		previousHeight = height
	}
	return prefixLines(out, first, cont)
}

func stackedTable(rows [][]string, width int, gutter string) []string {
	var out []string
	headers := rows[0]
	for rowIndex, row := range rows[1:] {
		if rowIndex > 0 {
			out = append(out, tableBlank(gutter))
		}
		for i, header := range headers {
			label := strings.TrimSpace(ui.CellsToString(style.ParseCells(header, ui.StyleClear)))
			if label == "" {
				label = fmt.Sprintf("Column %d", i+1)
			}
			label = style.Styled(label+":", "", "bold")
			value := ""
			if i < len(row) {
				value = row[i]
			}
			indent := style.TextWidth(label) + 1
			if width-indent < minimumTableColumn {
				for _, line := range wrapTableCell(label, width) {
					out = append(out, gutter+line)
				}
				for _, line := range wrapTableCell(value, width) {
					out = append(out, gutter+line)
				}
			} else {
				for j, line := range wrapTableCell(value, width-indent) {
					prefix := strings.Repeat(" ", indent)
					if j == 0 {
						prefix = label + " "
					}
					out = append(out, gutter+prefix+line)
				}
			}
		}
	}
	// Header-only tables still expose their fields while the first row streams.
	if len(rows) == 1 {
		empty := make([]string, len(headers))
		return stackedTable(append(slices.Clone(rows), empty), width, gutter)
	}
	return out
}

type tableGrapheme struct {
	cells []ui.Cell
	width int
	space bool
}

func wrapTableCell(value string, width int) []string {
	cells := style.ParseCells(value, ui.StyleClear)
	var glyphs []tableGrapheme
	g := uniseg.NewGraphemes(ui.CellsToString(cells))
	offset := 0
	for g.Next() {
		text := g.Str()
		count := utf8.RuneCountInString(text)
		part := cells[offset : offset+count]
		// Match the terminal cell painter's width accounting, but never split a
		// grapheme when breaking an oversized token.
		glyphs = append(glyphs, tableGrapheme{part, style.CellsWidth(part), strings.IndexFunc(text, func(r rune) bool { return !unicode.IsSpace(r) || r == '\u00a0' || r == '\u202f' }) < 0})
		offset += count
	}
	var lines []string
	for len(glyphs) > 0 {
		end, used, lastSpace := 0, 0, -1
		for end < len(glyphs) && (used+glyphs[end].width <= width || end == 0) {
			used += glyphs[end].width
			if glyphs[end].space {
				lastSpace = end
			}
			end++
		}
		next := end
		if end < len(glyphs) && !glyphs[end].space && lastSpace > 0 {
			end = lastSpace
			next = end + 1
		}
		for end > 0 && glyphs[end-1].space {
			end--
		}
		var line []ui.Cell
		for _, glyph := range glyphs[:end] {
			line = append(line, glyph.cells...)
		}
		lines = append(lines, tableCellMarkup(line))
		for next < len(glyphs) && glyphs[next].space {
			next++
		}
		glyphs = glyphs[next:]
	}
	if len(lines) == 0 {
		return []string{""}
	}
	return lines
}

// Re-encode styled runs after wrapping. Names retain palette colors rather
// than converting them to RGB, so terminal themes keep working.
func tableCellMarkup(cells []ui.Cell) string {
	var out strings.Builder
	for len(cells) > 0 {
		end := 1
		for end < len(cells) && cells[end].Style == cells[0].Style {
			end++
		}
		s := cells[0].Style
		text := style.Escape(ui.CellsToString(cells[:end]))
		var attrs []string
		for _, color := range []struct {
			key   string
			value ui.Color
		}{{"fg", s.Fg}, {"bg", s.Bg}} {
			if color.value == ui.ColorClear {
				continue
			}
			name := ""
			for candidate, value := range ui.StyleParserColorMap {
				if value == color.value && (name == "" || candidate < name) {
					name = candidate
				}
			}
			if name == "" {
				name = fmt.Sprintf("#%06x", color.value.Hex())
			}
			attrs = append(attrs, color.key+":"+name)
		}
		for _, mod := range []struct {
			name  string
			value ui.Modifier
		}{{"bold", ui.ModifierBold}, {"italic", ui.ModifierItalic}, {"strike", ui.ModifierStrike}, {"dim", ui.ModifierDim}, {"reverse", ui.ModifierReverse}, {"blink", ui.ModifierBlink}} {
			if s.Modifier&mod.value != 0 {
				attrs = append(attrs, "mod:"+mod.name)
			}
		}
		if len(attrs) > 0 {
			text = "[" + text + "](" + strings.Join(attrs, ",") + ")"
		}
		out.WriteString(text)
		cells = cells[end:]
	}
	return out.String()
}

func tableBlank(gutter string) string {
	if gutter == "" {
		return ""
	}
	return style.Styled("│", "muted", "")
}
