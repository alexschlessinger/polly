package main

import (
	"testing"

	ui "github.com/metaspartan/gotui/v5"
)

func TestStartupLogoIsNotAnInterstitial(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	if quit := r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "p"}); quit {
		t.Fatal("typing with the startup logo unexpectedly quit")
	}
	if got := r.model.ed.text(); got != "p" {
		t.Fatalf("live composer did not receive first key: %q", got)
	}
}
