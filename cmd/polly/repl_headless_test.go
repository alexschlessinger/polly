package main

import (
	"testing"

	tcell "github.com/gdamore/tcell/v3"
)

func TestHeadlessKeysPlayAsTerminalKeys(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   string
		key  tcell.Key
		str  string
	}{
		{name: "enter", id: "<Enter>", key: tcell.KeyEnter},
		{name: "esc", id: "<Escape>", key: tcell.KeyEsc},
		{name: "backspace", id: "<Backspace>", key: tcell.KeyBackspace},
		{name: "pgdn", id: "<PageDown>", key: tcell.KeyPgDn},
		{name: "space", id: " ", key: tcell.KeyRune, str: " "},
		{name: "c-a", id: "<C-a>", key: tcell.KeyCtrlA},
		{name: "c-z", id: "<C-z>", key: tcell.KeyCtrlZ},
		// A terminal sends these control characters as the keys they are.
		{name: "c-h", id: "<Backspace>", key: tcell.KeyBackspace},
		{name: "c-i", id: "<Tab>", key: tcell.KeyTab},
		{name: "c-m", id: "<Enter>", key: tcell.KeyEnter},
	} {
		step, err := parseHeadlessStep(1, ":key "+tc.name)
		if err != nil {
			t.Fatal(err)
		}
		event, ok := headlessKeyEvent(step.arg)
		if !ok {
			t.Fatalf("parsed key %q could not be played", tc.name)
		}
		key, ok := event.Payload.(*tcell.EventKey)
		if !ok {
			t.Fatalf("%s: payload = %T, want *tcell.EventKey", tc.name, event.Payload)
		}
		if event.ID != tc.id || key.Key() != tc.key || key.Str() != tc.str {
			t.Errorf("%s = %q %v %q, want %q %v %q", tc.name, event.ID, key.Key(), key.Str(), tc.id, tc.key, tc.str)
		}
	}
	for c := 'a'; c <= 'z'; c++ {
		if _, err := parseHeadlessStep(1, ":key c-"+string(c)); err != nil {
			t.Errorf("c-%c: %v", c, err)
		}
	}
	if _, err := parseHeadlessStep(1, ":key f1"); err == nil {
		t.Error("an unknown key name parsed")
	}
}
