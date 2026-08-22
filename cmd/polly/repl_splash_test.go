package main

import (
	"context"
	"strings"
	"testing"

	ui "github.com/metaspartan/gotui/v5"
)

func plainLogo(rows [][]ui.Cell) string {
	lines := make([]string, len(rows))
	for y, row := range rows {
		var line strings.Builder
		for _, cell := range row {
			line.WriteRune(cell.Rune)
		}
		lines[y] = strings.TrimRight(line.String(), " ")
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

func TestStartupLogoIsSmallTopLeftAndTextFree(t *testing.T) {
	for _, width := range []int{120, 80, 20, 13, 8, 1} {
		rows := pollyLogoRows(width)
		if len(rows) != startupLogoHeight {
			t.Fatalf("width %d logo has %d rows, want %d", width, len(rows), startupLogoHeight)
		}
		nonBlank := false
		for y, row := range rows {
			if len(row) > width {
				t.Fatalf("width %d logo row %d has %d cells", width, y, len(row))
			}
			for _, cell := range row {
				if cell.Rune != ' ' {
					nonBlank = true
				}
			}
		}
		if !nonBlank {
			t.Fatalf("width %d logo was entirely blank", width)
		}
		plain := plainLogo(rows)
		if strings.Contains(plain, "POLLY") || strings.Contains(plain, "type") {
			t.Fatalf("startup mark contains text:\n%s", plain)
		}
	}

	rows := pollyLogoRows(80)
	left := 80
	for _, row := range rows {
		for x, cell := range row {
			if cell.Rune != ' ' {
				left = min(left, x)
			}
		}
	}
	if left != 0 {
		t.Fatalf("80-column logo begins at column %d, want 0", left)
	}
}

func TestStartupLogoUsesSuppliedPollyPalette(t *testing.T) {
	colors := map[ui.Color]bool{}
	for _, row := range pollyLogoRows(80) {
		for _, cell := range row {
			if cell.Rune == ' ' {
				continue
			}
			colors[cell.Style.Fg] = true
			colors[cell.Style.Bg] = true
		}
	}
	for _, color := range []ui.Color{pollyGreen, pollyWing, pollyCrown, pollyBeak, pollyFace, pollyFoot} {
		if !colors[color] {
			t.Fatalf("startup logo did not use palette color %v", color)
		}
	}
}

func TestStartupLogoOnlyUsesSpareTranscriptHeight(t *testing.T) {
	if got := startupLogoRowCount(startupLogoHeight+1, true); got != startupLogoHeight {
		t.Fatalf("roomy terminal logo rows = %d", got)
	}
	if got := startupLogoRowCount(startupLogoHeight, true); got != 0 {
		t.Fatalf("short terminal reserved %d logo rows", got)
	}
	if got := startupLogoRowCount(100, false); got != 0 {
		t.Fatalf("hidden logo reserved %d rows", got)
	}
}

func TestStartupLogoIsNotAnInterstitial(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	r.startupLogoVisible = true
	if quit := r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "p"}); quit {
		t.Fatal("typing with the startup logo unexpectedly quit")
	}
	if !r.startupLogoVisible {
		t.Fatal("typing alone hid the startup logo like an interstitial")
	}
	if got := r.model.ed.text(); got != "p" {
		t.Fatalf("live composer did not receive first key: %q", got)
	}
}

func TestStartupLogoLeavesWhenFirstTurnStarts(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	r.startupLogoVisible = true
	done := r.startTurn(context.Background(), "hello", func(context.Context, string, TurnUI) error {
		return nil
	})
	if r.startupLogoVisible {
		t.Fatal("first turn did not release startup logo rows")
	}
	if err := <-done; err != nil {
		t.Fatalf("test turn failed: %v", err)
	}
}
