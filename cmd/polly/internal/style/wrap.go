package style

import (
	"unicode"

	rw "github.com/mattn/go-runewidth"
	ui "github.com/metaspartan/gotui/v5"
)

// UserGutterGlyph marks every row the user wrote: the composer while typing,
// the echoed prompt afterwards, and the queued or not-sent trailer under it.
// Assistant text carries no marker, so the bar alone says who is speaking.
const UserGutterGlyph = '▎'

// WrapCells wraps parsed transcript cells to terminal width while
// retaining the style carried by every source cell. Prose prefers whitespace
// boundaries; tokens wider than a row are hard-wrapped. Explicit newlines are
// preserved.
//
// Prompt lines repeat their styled user gutter as a hanging indent. Code
// lines repeat their styled "│ " gutter and hard-wrap so source whitespace is
// never reflowed.
func WrapCells(cells []ui.Cell, width int) []ui.Cell {
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

// VisualRows parses gotui inline styles, wraps the resulting cells,
// and returns the terminal rows ready for drawing or visual-row scroll math.
func VisualRows(text string, style ui.Style, width int) [][]ui.Cell {
	cells := ParseCells(text, style)
	return ui.SplitCells(WrapCells(cells, width), '\n')
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

	if prefix, content, hard, ok := transcriptHangingPrefix(line); ok {
		// A prefix that consumes the row cannot hang alongside content. Fall back
		// to ordinary hard wrapping so narrow terminals still make progress.
		if CellsWidth(prefix) >= width {
			return wrapTranscriptRows(nil, line, width, true)
		}
		return wrapTranscriptRows(prefix, content, width, hard)
	}

	return wrapTranscriptRows(nil, line, width, false)
}

// transcriptHangingPrefix recognizes the two transcript prefixes whose visual
// continuation has semantic meaning. Both repeat the original gutter cells and
// their style on every row: the user gutter wraps softly at words, the code
// gutter hard.
func transcriptHangingPrefix(line []ui.Cell) (prefix, content []ui.Cell, hard, ok bool) {
	if len(line) < 2 || line[1].Rune != ' ' {
		return nil, nil, false, false
	}

	switch line[0].Rune {
	case UserGutterGlyph:
		// Only the REPL-owned, accent/bold bar is a user prompt. The same
		// rune in assistant prose is unstyled and wraps as ordinary text.
		accent, known := ui.StyleParserColorMap["accent"]
		if !known || line[0].Style.Fg != accent || line[0].Style.Modifier&ui.ModifierBold == 0 {
			return nil, nil, false, false
		}
		return line[:2], line[2:], false, true
	case '│':
		// The code and table renderers own a muted gutter. Do not reinterpret
		// a literal vertical bar in ordinary assistant prose as fenced code.
		// Tables rely on the hard wrap to keep their column positions intact.
		muted, known := ui.StyleParserColorMap["muted"]
		if !known || line[0].Style.Fg != muted {
			return nil, nil, false, false
		}
		return line[:2], line[2:], true, true
	default:
		return nil, nil, false, false
	}
}

// wrapTranscriptRows lays content out in rows that each start with prefix.
// The caller guarantees prefix is narrower than width, so every row has room
// for at least one content cell. Hard rows cut at the fit index; soft rows
// prefer whitespace boundaries.
func wrapTranscriptRows(prefix, content []ui.Cell, width int, hard bool) [][]ui.Cell {
	if len(content) == 0 {
		return [][]ui.Cell{append([]ui.Cell(nil), prefix...)}
	}

	rows := make([][]ui.Cell, 0, 1)
	capacity := width - CellsWidth(prefix)
	for rest := content; len(rest) > 0; {
		var part []ui.Cell
		if hard {
			end := max(1, transcriptFitIndex(rest, capacity))
			part, rest = rest[:end], rest[end:]
		} else {
			part, rest = takeTranscriptWordRow(rest, capacity)
		}
		row := make([]ui.Cell, 0, len(prefix)+len(part))
		row = append(row, prefix...)
		row = append(row, part...)
		rows = append(rows, row)
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
	if IsWrapSpace(cells[end].Rune) {
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
		if IsWrapSpace(cells[i].Rune) {
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
		cellWidth := CellWidth(cell)
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

// TextWidth measures the display width of a string carrying gotui style
// markup: markup syntax, zero-width escapes, and private literal-bracket runes
// all measure as the cells they render to.
func TextWidth(s string) int {
	return CellsWidth(ParseCells(s, ui.StyleClear))
}

func CellsWidth(cells []ui.Cell) int {
	width := 0
	for _, cell := range cells {
		width += CellWidth(cell)
	}
	return width
}

func CellWidth(cell ui.Cell) int {
	width := rw.RuneWidth(cell.Rune)
	if width < 0 {
		return 0
	}
	return width
}

// IsWrapSpace reports whether r is whitespace a soft wrap may break at.
// No-break spaces are text, not boundaries.
func IsWrapSpace(r rune) bool {
	return r != '\u00a0' && r != '\u202f' && unicode.IsSpace(r)
}

func trimTranscriptSpaceRight(cells []ui.Cell, end int) int {
	for end > 0 && IsWrapSpace(cells[end-1].Rune) {
		end--
	}
	return end
}

func trimTranscriptSpaceLeft(cells []ui.Cell, start int) int {
	for start < len(cells) && IsWrapSpace(cells[start].Rune) {
		start++
	}
	return start
}
