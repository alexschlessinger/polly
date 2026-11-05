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
// Prompt lines repeat their styled user gutter as a hanging indent. Code and
// activity lines repeat their styled "│ " gutter (including layout indentation)
// and hard-wrap so source whitespace is never reflowed.
func WrapCells(cells []ui.Cell, width int) []ui.Cell {
	return wrapCells(cells, width, nil)
}

// WrapCellsWithLineStarts also maps each logical line to its first wrapped row.
// The final offset is the total row count, excluding an empty trailing line.
func WrapCellsWithLineStarts(cells []ui.Cell, width int) ([]ui.Cell, []int) {
	starts := []int{0}
	wrapped := wrapCells(cells, width, &starts)
	return wrapped, starts
}

func wrapCells(cells []ui.Cell, width int, starts *[]int) []ui.Cell {
	if len(cells) == 0 || width <= 0 {
		if starts != nil {
			for i, cell := range cells {
				if cell.Rune == '\n' || i == len(cells)-1 {
					*starts = append(*starts, len(*starts))
				}
			}
		}
		return append([]ui.Cell(nil), cells...)
	}
	out := make([]ui.Cell, 0, len(cells))
	lineStart, row := 0, 0
	appendLine := func(line []ui.Cell) {
		count := walkTranscriptLine(line, width, func(prefix, part []ui.Cell, row int) {
			if row > 0 {
				out = append(out, ui.Cell{Rune: '\n', Style: ui.StyleClear})
			}
			out = append(out, prefix...)
			out = append(out, part...)
		})
		if starts != nil {
			row += count
			*starts = append(*starts, row)
		}
	}
	for i, cell := range cells {
		if cell.Rune != '\n' {
			continue
		}
		appendLine(cells[lineStart:i])
		out = append(out, cell)
		lineStart = i + 1
	}
	if lineStart < len(cells) {
		appendLine(cells[lineStart:])
	}
	return out
}

// WrapCellRows returns the same rows as splitting WrapCells at newlines, plus
// the logical-line offsets from WrapCellsWithLineStarts. Rows borrow cells;
// callers must keep both the input and returned cells immutable. Only hanging
// gutter continuations need their own cell storage. Row capacities are capped
// so appending cannot overwrite another row or the source separators.
func WrapCellRows(cells []ui.Cell, width int) ([][]ui.Cell, []int) {
	lines := 1
	for _, cell := range cells {
		if cell.Rune == '\n' {
			lines++
		}
	}
	rows := make([][]ui.Cell, 0, lines)
	starts := make([]int, 1, lines+1)
	appendLine := func(line []ui.Cell) {
		count := walkTranscriptLine(line, width, func(prefix, part []ui.Cell, row int) {
			var cells []ui.Cell
			switch {
			case len(prefix) == 0:
				cells = part
			case row == 0:
				// The first gutter is already contiguous with its content.
				cells = line[:len(prefix)+len(part)]
			default:
				cells = make([]ui.Cell, 0, len(prefix)+len(part))
				cells = append(cells, prefix...)
				cells = append(cells, part...)
			}
			rows = append(rows, cells[:len(cells):len(cells)])
		})
		starts = append(starts, starts[len(starts)-1]+count)
	}
	start := 0
	for i, cell := range cells {
		if cell.Rune == '\n' {
			appendLine(cells[start:i])
			start = i + 1
		}
	}
	if start < len(cells) {
		appendLine(cells[start:])
	}
	return rows, starts
}

// VisualRows parses gotui inline styles, wraps the resulting cells,
// and returns the terminal rows ready for drawing or visual-row scroll math.
func VisualRows(text string, style ui.Style, width int) [][]ui.Cell {
	cells := ParseCells(text, style)
	return ui.SplitCells(WrapCells(cells, width), '\n')
}

// walkTranscriptLine yields borrowed row segments. A hanging prefix is separate
// so callers can either append directly to owned storage or retain row views.
func walkTranscriptLine(line []ui.Cell, width int, emit func(prefix, part []ui.Cell, row int)) int {
	if len(line) == 0 || width <= 0 {
		emit(nil, line, 0)
		return 1
	}

	if prefix, content, hard, ok := transcriptHangingPrefix(line); ok {
		// A prefix that consumes the row cannot hang alongside content. Fall back
		// to ordinary hard wrapping so narrow terminals still make progress.
		if CellsWidth(prefix) >= width {
			return walkTranscriptRows(nil, line, width, true, emit)
		}
		return walkTranscriptRows(prefix, content, width, hard, emit)
	}

	return walkTranscriptRows(nil, line, width, false, emit)
}

// transcriptHangingPrefix repeats the original gutter cells and their styles:
// user prompts wrap at words; code, tables and activity rails wrap literally.
func transcriptHangingPrefix(line []ui.Cell) (prefix, content []ui.Cell, hard, ok bool) {
	gutter := 0
	for gutter < len(line) && line[gutter].Rune == ' ' {
		gutter++
	}
	// Activity details sit behind two muted spaces (Rail), and a tool
	// row's body behind two more that are also dim. Only those
	// renderer-owned cells repeat as a hard-wrapping gutter; any further
	// indentation belongs to the source, even when it is also muted.
	const railCols = len(RailIndent)
	if muted, known := ui.StyleParserColorMap["muted"]; known && gutter >= railCols {
		rail := 0
		for rail < railCols && line[rail].Style.Fg == muted {
			rail++
		}
		if rail == railCols {
			for rail < railCols+2 && rail < gutter && line[rail].Style.Fg == muted && line[rail].Style.Modifier&ui.ModifierDim != 0 {
				rail++
			}
			if rail == railCols+1 {
				rail = railCols
			}
			return line[:rail], line[rail:], true, true
		}
	}
	if len(line) < gutter+2 || line[gutter+1].Rune != ' ' {
		return nil, nil, false, false
	}

	switch line[gutter].Rune {
	case UserGutterGlyph:
		// Only the REPL-owned, accent/bold bar is a user prompt. The same
		// rune in assistant prose is unstyled and wraps as ordinary text.
		accent, known := ui.StyleParserColorMap["accent"]
		if gutter != 0 || !known || line[0].Style.Fg != accent || line[0].Style.Modifier&ui.ModifierBold == 0 {
			return nil, nil, false, false
		}
		return line[:2], line[2:], false, true
	case '│':
		// Code, tables and activity details own a muted gutter. Do not
		// reinterpret a literal vertical bar in ordinary assistant prose.
		muted, known := ui.StyleParserColorMap["muted"]
		if !known || line[gutter].Style.Fg != muted {
			return nil, nil, false, false
		}
		end := gutter + 2
		return line[:end], line[end:], true, true
	default:
		return nil, nil, false, false
	}
}

// walkTranscriptRows lays content out in rows that each start with prefix.
// The caller guarantees prefix is narrower than width, so every row has room
// for at least one content cell. Hard rows cut at the fit index; soft rows
// prefer whitespace boundaries.
func walkTranscriptRows(prefix, content []ui.Cell, width int, hard bool, emit func(prefix, part []ui.Cell, row int)) int {
	if len(content) == 0 {
		emit(prefix, content, 0)
		return 1
	}

	rows := 0
	capacity := width - CellsWidth(prefix)
	for rest := content; len(rest) > 0; {
		var part []ui.Cell
		if hard {
			end := max(1, transcriptFitIndex(rest, capacity))
			part, rest = rest[:end], rest[end:]
		} else {
			part, rest = takeTranscriptWordRow(rest, capacity)
		}
		emit(prefix, part, rows)
		rows++
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
