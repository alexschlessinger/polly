package main

import (
	"context"
	"strings"
	"testing"

	ui "github.com/metaspartan/gotui/v5"
)

// TestStartupLogoImageLadder covers the graphics-capable splash: a terminal
// with room gets the image band, one without gets nothing, and terminals
// without native graphics never reserve a band (the bird is in the masthead).
func TestStartupLogoImageLadder(t *testing.T) {
	if got := startupLogoRowCount(imageLogoHeight+1, true, true); got != imageLogoHeight {
		t.Fatalf("native tall terminal logo rows = %d, want %d", got, imageLogoHeight)
	}
	if got := startupLogoRowCount(imageLogoHeight, true, true); got != 0 {
		t.Fatalf("native short terminal reserved %d rows", got)
	}
	if got := startupLogoRowCount(100, true, false); got != 0 {
		t.Fatalf("text terminal reserved %d rows", got)
	}
	if got := startupLogoRowCount(100, false, true); got != 0 {
		t.Fatalf("hidden logo reserved %d rows", got)
	}
}

func TestStartupLogoPlacementGeometry(t *testing.T) {
	width, height := embeddedLogoDims()
	if width != 418 || height != 418 {
		t.Fatalf("embedded logo dims = %dx%d, want 418x418", width, height)
	}

	logo, ok := startupLogoPlacement(80, 10, 20)
	if !ok {
		t.Fatal("no placement on a roomy terminal")
	}
	// A square source in 12 rows of 10x20 cells fits by height: 240px tall,
	// so 240px ≈ 24 columns wide, horizontally centered in 80 columns.
	if logo.Embedded != embeddedLogoAsset || logo.X != 28 || logo.Y != 0 {
		t.Fatalf("placement anchor = %+v", logo)
	}
	if logo.Rows != imageLogoArtRows || logo.Cols != 24 || !logo.FitByRows {
		t.Fatalf("placement geometry = %+v, want 24x%d fit-by-rows", logo, imageLogoArtRows)
	}

	if _, ok := startupLogoPlacement(4, 10, 20); ok {
		t.Fatal("expected no placement on a terminal too narrow for a thumbnail")
	}
}

func TestLoadPlacementImageEmbedded(t *testing.T) {
	img, err := loadPlacementImage(terminalImagePlacement{Embedded: embeddedLogoAsset})
	if err != nil {
		t.Fatal(err)
	}
	if bounds := img.Bounds(); bounds.Dx() != 418 || bounds.Dy() != 418 {
		t.Fatalf("decoded embedded logo = %dx%d, want 418x418", bounds.Dx(), bounds.Dy())
	}
	if _, err := loadPlacementImage(terminalImagePlacement{Embedded: "nope"}); err == nil {
		t.Fatal("unknown embedded asset should error")
	}
	version := placementImageVersion(terminalImagePlacement{Embedded: embeddedLogoAsset})
	if !strings.HasPrefix(version, "embedded:logo:") {
		t.Fatalf("embedded version = %q", version)
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
