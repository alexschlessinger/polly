package main

import (
	"testing"

	tcell "github.com/gdamore/tcell/v3"
	ui "github.com/metaspartan/gotui/v5"
)

func TestHeadlessControlKeys(t *testing.T) {
	for c := 'a'; c <= 'z'; c++ {
		name := "c-" + string(c)
		t.Run(name, func(t *testing.T) {
			step, err := parseHeadlessStep(1, ":key "+name)
			if err != nil {
				t.Fatal(err)
			}
			event, ok := headlessKeyEvent(step.text)
			if !ok || event.Type != ui.KeyboardEvent {
				t.Fatalf("parsed key %q could not be played", name)
			}
			key, ok := event.Payload.(*tcell.EventKey)
			if !ok {
				t.Fatalf("key payload = %T, want *tcell.EventKey", event.Payload)
			}
			if want := convertTcellKey(key).ID; event.ID != want {
				t.Fatalf("script key ID = %q, terminal event ID = %q", event.ID, want)
			}
			if c == 'i' && (event.ID != "<Tab>" || key.Key() != tcell.KeyTab) {
				t.Fatalf("Ctrl-I = %+v, want Tab", event)
			}
			if c == 'm' && (event.ID != "<Enter>" || key.Key() != tcell.KeyEnter) {
				t.Fatalf("Ctrl-M = %+v, want Enter", event)
			}
		})
	}
}
