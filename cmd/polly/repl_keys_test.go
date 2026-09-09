package main

import (
	"strings"
	"testing"
)

// The binding table is the one source for dispatch and /keys: every event ID
// binds once per phase, and every labeled binding shows up in the help.
func TestKeyBindingsAreUniqueAndDocumented(t *testing.T) {
	seen := map[keyPhase]map[string]string{}
	help := strings.Join(defaultReplCommands.helpLinesStyled(false), "\n")
	for _, g := range keyTable {
		for _, b := range g.bindings {
			if len(b.keys) == 0 || b.run == nil {
				t.Fatalf("%s: binding %q has no keys or no action", g.title, b.label)
			}
			if (b.label == "") != (b.desc == "") {
				t.Fatalf("%s: binding %v has a label without a description or vice versa", g.title, b.keys)
			}
			if b.label != "" && !strings.Contains(help, b.label) {
				t.Fatalf("/keys does not list %q", b.label)
			}
			for _, id := range b.keys {
				if seen[b.phase] == nil {
					seen[b.phase] = map[string]string{}
				}
				if prev, dup := seen[b.phase][id]; dup {
					t.Fatalf("%s bound twice in one phase: %q and %q", id, prev, b.label)
				}
				seen[b.phase][id] = b.label
				if _, ok := keyIndex[b.phase][id]; !ok {
					t.Fatalf("%s is not indexed for dispatch", id)
				}
			}
		}
	}
	for _, id := range []string{"<Enter>", "<Tab>", "<C-a>", "<Up>", "<C-g>"} {
		if _, ok := keyIndex[composerPhase][id]; !ok {
			t.Fatalf("composer key %s missing from the table", id)
		}
	}
	if _, ok := keyIndex[scrollPhase]["<PageUp>"]; !ok {
		t.Fatal("PgUp missing from the scroll phase")
	}
	if _, ok := keyIndex[globalPhase]["<C-z>"]; !ok {
		t.Fatal("Ctrl-Z missing from the global phase")
	}
}
