package main

import (
	"fmt"
	"reflect"
	"slices"
	"testing"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	ui "github.com/metaspartan/gotui/v5"
)

func TestTranscriptCellRowsPreserveWrappedSplitSemantics(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
	}{
		{"empty", ""},
		{"single", "single row"},
		{"newline", "\n"},
		{"blank rows", "\n\n"},
		{"leading blank", "\nfirst\nsecond"},
		{"interior blank", "first\n\nsecond"},
		{"trailing blank", "first\nsecond\n"},
		{"multiple trailing blanks", "first\n\n\n"},
		{"styled newlines", style.Styled("first\n\n", "accent", "bold") + style.Styled("second\n", "muted", "italic")},
		{"wide and combining", style.Styled("界 e\u0301 中\n\nnext 界 e\u0301", "accent", "bold")},
	} {
		for _, width := range []int{1, 4, 80} {
			for _, followed := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/width=%d/followed=%v", tc.name, width, followed), func(t *testing.T) {
					cells := style.ParseCells(tc.text, ui.StyleClear)
					checkTranscriptCellRows(t, cells, followed, width, nil, false)
				})
			}
		}
	}
}

func TestTranscriptCellRowsKeepImageMarkerSourceImmutable(t *testing.T) {
	images := []style.Image{{Width: 8, Height: 4}}
	text := "before\n" + style.Styled(string(style.ImageMarker(0)), "accent", "bold") + "\ncaption\n"
	cells := style.ParseCells(text, ui.StyleClear)
	for _, native := range []bool{false, true} {
		for _, followed := range []bool{false, true} {
			t.Run(fmt.Sprintf("native=%v/followed=%v", native, followed), func(t *testing.T) {
				checkTranscriptCellRows(t, cells, followed, 80, images, native)
			})
		}
	}
}

func checkTranscriptCellRows(t *testing.T, source []ui.Cell, followed bool, width int, images []style.Image, native bool) {
	t.Helper()
	// Spare source capacity catches a synthetic separator append as well as
	// accidental writes through borrowed rows, including their separators.
	backing := make([]ui.Cell, len(source)+4)
	copy(backing, source)
	for i := len(source); i < len(backing); i++ {
		backing[i] = ui.Cell{Rune: '!', Style: ui.StyleClear}
	}
	before := slices.Clone(backing)
	cells := backing[:len(source)]

	// Keep the previous wrap-then-copying-split path as an independent
	// reference for wrapping, blank rows, styled newlines, and separators.
	copyForSplit := style.WrapCells(slices.Clone(source), width)
	if followed {
		copyForSplit = append(copyForSplit, ui.Cell{Rune: '\n', Style: ui.StyleClear})
	}
	want, wantSpans := locateTranscriptImages(ui.SplitCells(copyForSplit, '\n'), images, native, width, 10, 20)
	got, spans := transcriptCellRowsWithImages(cells, followed, width, images, native, 10, 20)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rows differ, including empty-slice shape: got %#v, want %#v", got, want)
	}
	if !slices.Equal(spans, wantSpans) {
		t.Fatalf("image spans differ: got %+v, want %+v", spans, wantSpans)
	}
	for i := range got {
		if len(images) > 0 {
			for _, cell := range got[i] {
				if _, marker := style.ImageMarkerIndex(cell.Rune); marker {
					t.Fatalf("image marker survived in row %d", i)
				}
			}
		}
	}
	if !slices.Equal(backing, before) {
		t.Fatal("wrapping, splitting, or image marker removal changed the source buffer")
	}

	for i, row := range got {
		// Two appended cells cross the intervening newline in an uncapped
		// row view, corrupting the next row even though row itself is intact.
		extended := append(row, ui.Cell{Rune: '?'}, ui.Cell{Rune: '#'})
		if len(extended) != len(row)+2 {
			t.Fatal("row append did not extend the row")
		}
		for j := range got {
			if !reflect.DeepEqual(got[j], want[j]) {
				t.Fatalf("appending to row %d changed row %d", i, j)
			}
		}
		if !slices.Equal(backing, before) {
			t.Fatalf("appending to row %d changed the source buffer", i)
		}
	}
}
