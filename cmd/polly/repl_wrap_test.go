package main

import (
	"slices"
	"testing"

	ui "github.com/metaspartan/gotui/v5"
)

func transcriptRowsText(rows [][]ui.Cell) []string {
	out := make([]string, len(rows))
	for i, row := range rows {
		out[i] = ui.CellsToString(row)
	}
	return out
}

func requireTranscriptRows(t *testing.T, text string, width int, want []string) [][]ui.Cell {
	t.Helper()
	rows := transcriptVisualRows(text, ui.NewStyle(ui.ColorClear), width)
	if got := transcriptRowsText(rows); !slices.Equal(got, want) {
		t.Fatalf("visual rows = %#v, want %#v", got, want)
	}
	return rows
}

func TestTranscriptVisualRowsWordWrapsProse(t *testing.T) {
	requireTranscriptRows(t, "alpha beta gamma", 10, []string{"alpha beta", "gamma"})
}

func TestTranscriptVisualRowsHardWrapsLongTokenByDisplayWidth(t *testing.T) {
	rows := requireTranscriptRows(t, "ab界界界", 4, []string{"ab界", "界界"})
	for i, row := range rows {
		if got := transcriptCellsWidth(row); got > 4 {
			t.Fatalf("row %d display width = %d, want <= 4", i, got)
		}
	}
}

func TestTranscriptVisualRowsKeepsLeadingZeroWidthCellWithWideRune(t *testing.T) {
	requireTranscriptRows(t, "\u200b界x", 1, []string{"\u200b界", "x"})
}

func TestTranscriptVisualRowsUsesPromptHangingIndent(t *testing.T) {
	rows := requireTranscriptRows(t,
		"[> ](fg:blue,mod:bold)alpha beta gamma",
		12,
		[]string{"> alpha beta", "  gamma"},
	)

	if rows[0][0].Style != ui.ParseStyles("[>](fg:blue,mod:bold)", ui.StyleClear)[0].Style {
		t.Fatal("prompt marker lost its source style")
	}
	if rows[1][2].Style != ui.NewStyle(ui.ColorClear) {
		t.Fatal("wrapped prompt content lost its source style")
	}
}

func TestTranscriptVisualRowsDoesNotTreatAssistantBlockquoteAsPrompt(t *testing.T) {
	requireTranscriptRows(t, "> alpha beta", 8, []string{"> alpha", "beta"})
}

func TestTranscriptVisualRowsRepeatsStyledCodeGutter(t *testing.T) {
	text := "[│ ](fg:grey)[abcdefghij](fg:white)"
	rows := requireTranscriptRows(t, text, 6, []string{"│ abcd", "│ efgh", "│ ij"})

	wantGutter := ui.ParseStyles("[│ ](fg:grey)", ui.StyleClear)
	for i, row := range rows {
		if len(row) < 2 || row[0].Style != wantGutter[0].Style || row[1].Style != wantGutter[1].Style {
			t.Fatalf("row %d did not preserve repeated gutter style", i)
		}
	}
}

func TestTranscriptVisualRowsHardWrapsTableRowsLikeCode(t *testing.T) {
	// Table rows carry the same muted "│ " gutter as code lines, so an
	// overwide table hard-wraps with a repeated gutter instead of word
	// wrapping, keeping the unwrapped prefix's column positions intact.
	requireTranscriptRows(t,
		"[│ ](fg:muted)aaa  bbb  cc",
		8,
		[]string{"│ aaa  b", "│ bb  cc"},
	)
}

func TestTranscriptVisualRowsPreservesExplicitBlankLines(t *testing.T) {
	requireTranscriptRows(t, "alpha beta\n\ncharlie", 6, []string{"alpha", "beta", "", "charli", "e"})
}

func TestWrapTranscriptCellsPreservesSourceStylesAcrossBreaks(t *testing.T) {
	defaultStyle := ui.NewStyle(ui.ColorClear)
	text := "[alpha](fg:green,mod:bold) [beta](fg:red)"
	source := ui.ParseStyles(text, defaultStyle)
	rows := transcriptVisualRows(text, defaultStyle, 5)
	requireTranscriptRows(t, text, 5, []string{"alpha", "beta"})

	for i := range rows[0] {
		if rows[0][i].Style != source[i].Style {
			t.Fatalf("first word cell %d style changed", i)
		}
	}
	for i := range rows[1] {
		if rows[1][i].Style != source[len("alpha ")+i].Style {
			t.Fatalf("second word cell %d style changed", i)
		}
	}
}

func TestWrapTranscriptCellsLeavesNonPositiveWidthUnchanged(t *testing.T) {
	cells := ui.ParseStyles("[wide text](fg:green,mod:bold)", ui.StyleClear)
	got := wrapTranscriptCells(cells, 0)
	if !slices.Equal(got, cells) {
		t.Fatal("non-positive width changed transcript cells")
	}
}
