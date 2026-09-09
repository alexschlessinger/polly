package main

import (
	"context"
	"testing"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/termimg"
	ui "github.com/metaspartan/gotui/v5"
)

// TestStartupLogoImageLadder covers the graphics-capable splash: a terminal
// with room gets the image band, one without gets nothing, and terminals
// without native graphics never reserve a band (the bird is in the masthead).
func TestStartupLogoImageLadder(t *testing.T) {
	if got := startupLogoRowCount(termimg.LogoHeight+1, true, true); got != termimg.LogoHeight {
		t.Fatalf("native tall terminal logo rows = %d, want %d", got, termimg.LogoHeight)
	}
	if got := startupLogoRowCount(termimg.LogoHeight, true, true); got != 0 {
		t.Fatalf("native short terminal reserved %d rows", got)
	}
	if got := startupLogoRowCount(100, true, false); got != 0 {
		t.Fatalf("text terminal reserved %d rows", got)
	}
	if got := startupLogoRowCount(100, false, true); got != 0 {
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
