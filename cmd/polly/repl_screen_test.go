package main

import (
	"testing"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/headlessscreen"
)

func newTestScreen(t *testing.T, w, h int) *headlessscreen.Screen {
	t.Helper()
	screen, err := headlessscreen.New(w, h)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(screen.Fini)
	return screen
}

func screenSnapshot(t *testing.T, screen *headlessscreen.Screen) *headlessscreen.Frame {
	t.Helper()
	frame, err := screen.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	return frame
}
