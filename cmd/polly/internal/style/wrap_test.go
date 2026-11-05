package style

import (
	"reflect"
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
	rows := VisualRows(text, ui.NewStyle(ui.ColorClear), width)
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
		if got := CellsWidth(row); got > 4 {
			t.Fatalf("row %d display width = %d, want <= 4", i, got)
		}
	}
}

func TestTranscriptVisualRowsKeepsLeadingZeroWidthCellWithWideRune(t *testing.T) {
	requireTranscriptRows(t, "\u200b界x", 1, []string{"\u200b界", "x"})
}

// A wrapped prompt keeps the user gutter on every row, in the marker's own
// style, so a long question still reads as one block the user wrote.
func TestTranscriptVisualRowsRepeatsUserGutter(t *testing.T) {
	rows := requireTranscriptRows(t,
		Styled(string(UserGutterGlyph)+" ", "accent", "bold")+"alpha beta gamma",
		12,
		[]string{"▎ alpha beta", "▎ gamma"},
	)

	want := ui.ParseStyles(Styled("▎", "accent", "bold"), ui.StyleClear)[0].Style
	if rows[0][0].Style != want {
		t.Fatal("prompt marker lost its source style")
	}
	if rows[1][0].Style != want {
		t.Fatal("continuation gutter lost the marker style")
	}
	if rows[1][2].Style != ui.NewStyle(ui.ColorClear) {
		t.Fatal("wrapped prompt content lost its source style")
	}
}

// Assistant prose wraps as prose: a Markdown blockquote and a bare bar rune
// are ordinary words without the accent/bold gutter style.
func TestTranscriptVisualRowsDoesNotTreatAssistantTextAsPrompt(t *testing.T) {
	requireTranscriptRows(t, "> alpha beta", 8, []string{"> alpha", "beta"})
	requireTranscriptRows(t, "▎ alpha beta", 8, []string{"▎ alpha", "beta"})
	requireTranscriptRows(t, "  │ alpha beta", 10, []string{"  │ alpha", "beta"})
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

func TestTranscriptVisualRowsRepeatsActivityRail(t *testing.T) {
	for _, tc := range []struct {
		name, content, tone string
		width               int
		want                []string
	}{
		{"read", "1: ab界éfg  hij", "code", 10, []string{"  1: ab界é", "  fg  hij"}},
		{"diff", "+a  b   c  d", "syn-add", 10, []string{"  +a  b   ", "  c  d"}},
		{"source indent", "  abcdefgh", "code", 10, []string{"    abcdef", "  gh"}},
		{"inherited source indent", "  abcdefgh", "clear", 10, []string{"    abcdef", "  gh"}},
		{"muted source indent", "    abcdefghijklmnopqrstuvwxyz", "muted", 14, []string{"      abcdefgh", "  ijklmnopqrst", "  uvwxyz"}},
		{"comment source indent", "    abcdefghijklmnopqrstuvwxyz", "syn-comment", 14, []string{"      abcdefgh", "  ijklmnopqrst", "  uvwxyz"}},
		{"overwide source indent", "        x", "clear", 6, []string{"      ", "      ", "  x"}},
		{"overwide muted source indent", "        x", "muted", 6, []string{"      ", "      ", "  x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prefix := ParseCells(Rail, ui.StyleClear)
			content := ParseCells(Styled(tc.content, tc.tone, ""), ui.StyleClear)
			rows := requireTranscriptRows(t, Rail+Styled(tc.content, tc.tone, ""), tc.width, tc.want)
			var restored []ui.Cell
			for i, row := range rows {
				if len(row) < len(prefix) || !slices.Equal(row[:len(prefix)], prefix) {
					t.Fatalf("row %d lost the rail/padding styles: %#v", i, row)
				}
				if CellsWidth(row) > tc.width {
					t.Fatalf("row %d exceeds width %d: %q", i, tc.width, ui.CellsToString(row))
				}
				restored = append(restored, row[len(prefix):]...)
			}
			if !slices.Equal(restored, content) {
				t.Fatal("wrapping changed source whitespace or styles")
			}
		})
	}
}

func TestTranscriptVisualRowsActivityRailTooNarrow(t *testing.T) {
	text := Rail + Styled("abcdef", "code", "")
	source := ParseCells(text, ui.StyleClear)
	for _, width := range []int{1, 2} {
		var restored []ui.Cell
		for _, row := range VisualRows(text, ui.StyleClear, width) {
			if CellsWidth(row) > width {
				t.Fatalf("row exceeds width %d: %q", width, ui.CellsToString(row))
			}
			restored = append(restored, row...)
		}
		if !slices.Equal(restored, source) {
			t.Fatalf("width %d: a rail with no room for content did not fall back to hard wrapping", width)
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
	rows := VisualRows(text, defaultStyle, 5)
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

func TestWrapCellsWithLineStarts(t *testing.T) {
	for _, text := range []string{"", "\n", "\n\n", "alpha beta", "alpha beta\n", "alpha beta\n\n界界é", Styled("alpha\nbeta gamma", "accent", "bold"), Styled("▎ ", "accent", "bold") + "long prompt wraps"} {
		cells := ParseCells(text, ui.StyleClear)
		for _, width := range []int{-1, 0, 1, 4, 10} {
			wrapped, starts := WrapCellsWithLineStarts(cells, width)
			if !slices.Equal(wrapped, WrapCells(cells, width)) {
				t.Fatalf("%q at width %d: tracking offsets changed cells", text, width)
			}
			want := []int{0}
			for _, line := range ui.SplitCells(cells, '\n') {
				count := max(1, len(ui.SplitCells(WrapCells(line, width), '\n')))
				want = append(want, want[len(want)-1]+count)
			}
			if !slices.Equal(starts, want) {
				t.Fatalf("%q at width %d: starts = %v, want %v", text, width, starts, want)
			}
		}
	}
}

func TestWrapTranscriptCellsLeavesNonPositiveWidthUnchanged(t *testing.T) {
	cells := ui.ParseStyles("[wide text](fg:green,mod:bold)", ui.StyleClear)
	got := WrapCells(cells, 0)
	if !slices.Equal(got, cells) {
		t.Fatal("non-positive width changed transcript cells")
	}
}

func TestWrapCellsOwnsOutput(t *testing.T) {
	text := Styled("▎ ", "accent", "bold") + "ab界 cd\n\n" +
		Styled("│ ", "muted", "") + Styled("\u200b界éz", "green", "bold") +
		"\n" + string(ImageMarker(0)) + "\n"
	for _, wrap := range []struct {
		name string
		call func([]ui.Cell, int) []ui.Cell
	}{
		{"cells", WrapCells},
		{"line starts", func(cells []ui.Cell, width int) []ui.Cell {
			out, _ := WrapCellsWithLineStarts(cells, width)
			return out
		}},
	} {
		t.Run(wrap.name, func(t *testing.T) {
			for _, width := range []int{-1, 0, 1, 2, 6, 80} {
				cells := ParseCells(text, ui.StyleClear)
				before := slices.Clone(cells)
				out := wrap.call(cells, width)
				if !slices.Equal(cells, before) {
					t.Fatalf("width %d: wrapping changed the source", width)
				}
				for i := range out {
					out[i] = ui.Cell{Rune: 'x', Style: ui.StyleClear}
				}
				if !slices.Equal(cells, before) {
					t.Fatalf("width %d: changing output changed the source", width)
				}
				out = wrap.call(cells, width)
				want := slices.Clone(out)
				for i := range cells {
					cells[i] = ui.Cell{Rune: 'y', Style: ui.StyleClear}
				}
				if !slices.Equal(out, want) {
					t.Fatalf("width %d: changing the source changed retained output", width)
				}
			}
		})
	}
}

func TestWrapCellsLineStartsWithStyledWideGutters(t *testing.T) {
	text := Styled("▎ ", "accent", "bold") + "ab界 cd\n\n" +
		Styled("│ ", "muted", "") + Styled("\u200b界éz", "green", "bold") + "\n"
	wrapped, starts := WrapCellsWithLineStarts(ParseCells(text, ui.StyleClear), 6)
	wantText := Styled("▎ ", "accent", "bold") + "ab界\n" +
		Styled("▎ ", "accent", "bold") + "cd\n\n" +
		Styled("│ ", "muted", "") + Styled("\u200b界éz", "green", "bold") + "\n"
	if !slices.Equal(wrapped, ParseCells(wantText, ui.StyleClear)) {
		t.Fatal("wrapped wide gutters changed text or cell styles")
	}
	if want := []int{0, 2, 3, 4}; !slices.Equal(starts, want) {
		t.Fatalf("line starts = %v, want %v", starts, want)
	}
}

func TestWrapCellRowsMatchesOwnedWrapping(t *testing.T) {
	for _, text := range []string{
		"", "\n", "\n\n", "single", "first\n\nsecond\n", "  leading and trailing  ",
		"ab界界é\u200b end", Styled("first\n\nsecond\n", "green", "bold"),
		Styled("▎ ", "accent", "bold") + "ab界 cd éf",
		Styled("│ ", "muted", "") + "  ab界cd éf  ",
		Styled("▎ ", "accent", "bold"), Styled("│ ", "muted", ""),
		Rail + "  " + Styled("1: ab界éfg  hij\n", "code", "") + RailBar,
		Rail + Styled("+a  b   c  d", "syn-add", ""),
		Rail + Styled("    abcdefghijklmnopqrstuvwxyz", "syn-comment", ""),
		Styled(string(ImageMarker(0)), "accent", "bold") + "\ncaption\n",
	} {
		for _, width := range []int{-1, 0, 1, 2, 4, 6, 80} {
			cells := ParseCells(text, ui.StyleClear)
			before := slices.Clone(cells)
			wrapped, wantStarts := WrapCellsWithLineStarts(cells, width)
			want := ui.SplitCells(wrapped, '\n')
			got, starts := WrapCellRows(cells, width)
			if !reflect.DeepEqual(got, want) || !slices.Equal(starts, wantStarts) {
				t.Fatalf("%q at width %d: rows/starts = %#v/%v, want %#v/%v", text, width, got, starts, want, wantStarts)
			}
			if !slices.Equal(cells, before) {
				t.Fatalf("%q at width %d: wrapping changed the source", text, width)
			}
			for _, row := range got {
				if cap(row) != len(row) {
					t.Fatalf("%q at width %d: row has writable spare capacity", text, width)
				}
				extended := append(row, ui.Cell{Rune: '?'}, ui.Cell{Rune: '!'})
				if len(extended) != len(row)+2 || !reflect.DeepEqual(got, want) || !slices.Equal(cells, before) {
					t.Fatalf("%q at width %d: appending changed retained rows or source", text, width)
				}
			}
		}
	}
}
