package main

import (
	"image"
	"testing"
	"time"

	tcell "github.com/gdamore/tcell/v3"
	rw "github.com/mattn/go-runewidth"
)

func TestHeadlessKeysPlayAsTerminalKeys(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   string
		key  tcell.Key
		str  string
	}{
		{name: "s-tab", id: "<S-Tab>", key: tcell.KeyBacktab},
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

func TestHeadlessClickTakesACellOrText(t *testing.T) {
	for _, tc := range []struct {
		line, text string
		x, y       int
		timeout    time.Duration
	}{
		{":click 140 39", "", 140, 39, 0},
		{":click 3 running", "3 running", 0, 0, headlessWaitDefault},
		{`:click "12 3"`, "12 3", 0, 0, headlessWaitDefault},
		{":click Agents 5", "Agents", 0, 0, 5 * time.Second},
	} {
		step, err := parseHeadlessStep(1, tc.line)
		if err != nil || step.kind != "click" || step.arg != tc.text || step.x != tc.x || step.y != tc.y || step.duration != tc.timeout {
			t.Errorf("%s: %+v, %v", tc.line, step, err)
		}
	}
	for _, line := range []string{":click", ":click -1 2"} {
		if _, err := parseHeadlessStep(1, line); err == nil {
			t.Errorf("%s parsed", line)
		}
	}
}

// cellRows is a screen of text rows, a wide rune taking two cells.
type cellRows []string

func (s cellRows) Size() (int, int) { return 12, len(s) }

func (s cellRows) Get(x, y int) (string, tcell.Style, int) {
	col := 0
	for _, r := range s[y] {
		w := rw.RuneWidth(r)
		if col == x {
			return string(r), tcell.StyleDefault, w
		}
		col += w
	}
	return " ", tcell.StyleDefault, 1
}

func TestHeadlessFindCellCountsWideCells(t *testing.T) {
	screen := cellRows{"日本 go", "  go agents", "go"}
	for _, tc := range []struct {
		text string
		at   image.Point
		ok   bool
	}{
		{"go", image.Pt(5, 0), true},
		{"agents", image.Pt(5, 1), true},
		{"本", image.Pt(2, 0), true},
		{"missing", image.Point{}, false},
	} {
		if at, ok := headlessFindCell(screen, tc.text); at != tc.at || ok != tc.ok {
			t.Errorf("%q at %v %v, want %v %v", tc.text, at, ok, tc.at, tc.ok)
		}
	}
}
