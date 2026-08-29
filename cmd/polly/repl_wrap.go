package main

import (
	"unicode"

	rw "github.com/mattn/go-runewidth"
	ui "github.com/metaspartan/gotui/v5"
)

// wrapTranscriptCells wraps parsed transcript cells to terminal width while
// retaining the style carried by every source cell. Prose prefers whitespace
// boundaries; tokens wider than a row are hard-wrapped. Explicit newlines are
// preserved.
//
// Prompt lines use a two-column hanging indent after their leading "> ". Code
// lines repeat their styled "│ " gutter and hard-wrap so source whitespace is
// never reflowed.
func wrapTranscriptCells(cells []ui.Cell, width int) []ui.Cell {
	if len(cells) == 0 || width <= 0 {
		return append([]ui.Cell(nil), cells...)
	}

	out := make([]ui.Cell, 0, len(cells))
	lineStart := 0
	for i, cell := range cells {
		if cell.Rune != '\n' {
			continue
		}
		out = appendTranscriptRows(out, wrapTranscriptLine(cells[lineStart:i], width))
		out = append(out, cell)
		lineStart = i + 1
	}
	if lineStart < len(cells) {
		out = appendTranscriptRows(out, wrapTranscriptLine(cells[lineStart:], width))
	}
	return out
}

// transcriptVisualRows parses gotui inline styles, wraps the resulting cells,
// and returns the terminal rows ready for drawing or visual-row scroll math.
func transcriptVisualRows(text string, style ui.Style, width int) [][]ui.Cell {
	cells := parseStyledCells(text, style)
	return ui.SplitCells(wrapTranscriptCells(cells, width), '\n')
}

func appendTranscriptRows(dst []ui.Cell, rows [][]ui.Cell) []ui.Cell {
	for i, row := range rows {
		if i > 0 {
			dst = append(dst, ui.Cell{Rune: '\n', Style: ui.StyleClear})
		}
		dst = append(dst, row...)
	}
	return dst
}

func wrapTranscriptLine(line []ui.Cell, width int) [][]ui.Cell {
	if len(line) == 0 {
		return [][]ui.Cell{{}}
	}

	if prefix, continuation, content, hard, ok := transcriptHangingPrefix(line); ok {
		// A prefix that consumes the row cannot hang alongside content. Fall back
		// to ordinary hard wrapping so narrow terminals still make progress.
		if transcriptCellsWidth(prefix) >= width || transcriptCellsWidth(continuation) >= width {
			return wrapTranscriptHard(nil, nil, line, width)
		}
		if hard {
			return wrapTranscriptHard(prefix, continuation, content, width)
		}
		return wrapTranscriptWords(prefix, continuation, content, width)
	}

	return wrapTranscriptWords(nil, nil, line, width)
}

// transcriptHangingPrefix recognizes the two transcript prefixes whose visual
// continuation has semantic meaning. The prompt continuation is whitespace;
// the code continuation repeats the original gutter cells and their style.
func transcriptHangingPrefix(line []ui.Cell) (prefix, continuation, content []ui.Cell, hard, ok bool) {
	if len(line) < 2 || line[1].Rune != ' ' {
		return nil, nil, nil, false, false
	}

	switch line[0].Rune {
	case '>':
		// Only the REPL-owned, accent/bold marker is a user prompt. A plain
		// assistant Markdown blockquote must retain ordinary prose wrapping.
		accent, known := ui.StyleParserColorMap["accent"]
		if !known || line[0].Style.Fg != accent || line[0].Style.Modifier&ui.ModifierBold == 0 {
			return nil, nil, nil, false, false
		}
		prefix = append([]ui.Cell(nil), line[:2]...)
		continuation = []ui.Cell{
			{Rune: ' ', Style: line[0].Style},
			{Rune: ' ', Style: line[1].Style},
		}
		return prefix, continuation, line[2:], false, true
	case '│':
		// The code and table renderers own a muted gutter. Do not reinterpret
		// a literal vertical bar in ordinary assistant prose as fenced code.
		// Tables rely on the hard wrap to keep their column positions intact.
		muted, known := ui.StyleParserColorMap["muted"]
		if !known || line[0].Style.Fg != muted {
			return nil, nil, nil, false, false
		}
		prefix = append([]ui.Cell(nil), line[:2]...)
		continuation = append([]ui.Cell(nil), prefix...)
		return prefix, continuation, line[2:], true, true
	default:
		return nil, nil, nil, false, false
	}
}

func wrapTranscriptWords(prefix, continuation, content []ui.Cell, width int) [][]ui.Cell {
	if len(content) == 0 {
		return [][]ui.Cell{append([]ui.Cell(nil), prefix...)}
	}

	rows := make([][]ui.Cell, 0, 1)
	rest := content
	first := true
	for len(rest) > 0 {
		rowPrefix := continuation
		if first {
			rowPrefix = prefix
		}
		capacity := width - transcriptCellsWidth(rowPrefix)
		if capacity <= 0 {
			return wrapTranscriptHard(nil, nil, append(append([]ui.Cell(nil), prefix...), content...), width)
		}

		part, remaining := takeTranscriptWordRow(rest, capacity)
		row := make([]ui.Cell, 0, len(rowPrefix)+len(part))
		row = append(row, rowPrefix...)
		row = append(row, part...)
		rows = append(rows, row)
		rest = remaining
		first = false
	}
	return rows
}

func wrapTranscriptHard(prefix, continuation, content []ui.Cell, width int) [][]ui.Cell {
	if len(content) == 0 {
		return [][]ui.Cell{append([]ui.Cell(nil), prefix...)}
	}

	rows := make([][]ui.Cell, 0, 1)
	rest := content
	first := true
	for len(rest) > 0 {
		rowPrefix := continuation
		if first {
			rowPrefix = prefix
		}
		capacity := width - transcriptCellsWidth(rowPrefix)
		if capacity <= 0 {
			// This only occurs in the no-prefix narrow fallback. Consuming one
			// source cell prevents zero-width or wide-rune input from stalling.
			capacity = width
		}

		end := transcriptFitIndex(rest, capacity)
		if end == 0 {
			end = 1
		}
		row := make([]ui.Cell, 0, len(rowPrefix)+end)
		row = append(row, rowPrefix...)
		row = append(row, rest[:end]...)
		rows = append(rows, row)
		rest = rest[end:]
		first = false
	}
	return rows
}

func takeTranscriptWordRow(cells []ui.Cell, width int) (row, rest []ui.Cell) {
	end := transcriptFitIndex(cells, width)
	if end >= len(cells) {
		return cells, nil
	}
	if end == 0 {
		return cells[:1], cells[1:]
	}

	// When the row ends exactly before whitespace, retain everything that fit
	// and consume the whitespace as the visual line boundary.
	if transcriptWrapSpace(cells[end].Rune) {
		rowEnd := trimTranscriptSpaceRight(cells, end)
		restStart := trimTranscriptSpaceLeft(cells, end)
		if rowEnd > 0 {
			return cells[:rowEnd], cells[restStart:]
		}
	}

	// Otherwise the fit ended inside a word. Rewind to the last whitespace that
	// fit; if none exists, hard-wrap the unbreakable token.
	lastSpace := -1
	for i := 0; i < end; i++ {
		if transcriptWrapSpace(cells[i].Rune) {
			lastSpace = i
		}
	}
	if lastSpace >= 0 {
		rowEnd := trimTranscriptSpaceRight(cells, lastSpace)
		restStart := trimTranscriptSpaceLeft(cells, lastSpace)
		if rowEnd > 0 {
			return cells[:rowEnd], cells[restStart:]
		}
	}
	return cells[:end], cells[end:]
}

func transcriptFitIndex(cells []ui.Cell, width int) int {
	if width <= 0 {
		return 0
	}
	used := 0
	for i, cell := range cells {
		cellWidth := transcriptCellWidth(cell)
		if cellWidth > 0 && used+cellWidth > width {
			// Keep leading combining/zero-width cells attached to the first
			// visible rune. If that rune itself is wider than the terminal, it
			// must overflow one row rather than leave an invisible row behind.
			if used == 0 {
				return i + 1
			}
			return i
		}
		used += cellWidth
	}
	return len(cells)
}

// styledTextWidth measures the display width of a string carrying gotui style
// markup: markup syntax, zero-width escapes, and private literal-bracket runes
// all measure as the cells they render to.
func styledTextWidth(s string) int {
	return transcriptCellsWidth(parseStyledCells(s, ui.StyleClear))
}

func transcriptCellsWidth(cells []ui.Cell) int {
	width := 0
	for _, cell := range cells {
		width += transcriptCellWidth(cell)
	}
	return width
}

func transcriptCellWidth(cell ui.Cell) int {
	width := rw.RuneWidth(cell.Rune)
	if width < 0 {
		return 0
	}
	return width
}

func transcriptWrapSpace(r rune) bool {
	return r != '\u00a0' && r != '\u202f' && unicode.IsSpace(r)
}

func trimTranscriptSpaceRight(cells []ui.Cell, end int) int {
	for end > 0 && transcriptWrapSpace(cells[end-1].Rune) {
		end--
	}
	return end
}

func trimTranscriptSpaceLeft(cells []ui.Cell, start int) int {
	for start < len(cells) && transcriptWrapSpace(cells[start].Rune) {
		start++
	}
	return start
}
