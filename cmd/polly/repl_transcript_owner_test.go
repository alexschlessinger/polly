package main

import (
	"strings"
	"testing"
)

// TestQueuedPromptRewriteInvalidatesRender pins that every in-place rewrite
// of a transcript entry reaches the next render. A queued prompt whose turn
// carries no images is the case that used to slip through: the decoration
// step had nothing to add, so nothing marked the visual cache stale.
func TestQueuedPromptRewriteInvalidatesRender(t *testing.T) {
	for _, tc := range []struct {
		name    string
		rewrite func(m *replModel, item queuedREPLInput)
		marker  string
	}{
		{"activate", (*replModel).activateQueuedInput, "(queued)"},
		{"not sent", (*replModel).markQueuedInputNotSent, "(queued)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newReplModel()
			turn := textManagedTurn("later")
			item := queuedREPLInput{text: "later", turn: &turn}
			m.appendQueuedInput(&item)
			before := strings.Join(rowsText(m.transcriptRows(80)), "\n")
			if !strings.Contains(before, tc.marker) {
				t.Fatalf("queued echo missing %q: %q", tc.marker, before)
			}
			tc.rewrite(m, item)
			after := strings.Join(rowsText(m.transcriptRows(80)), "\n")
			if strings.Contains(after, tc.marker) {
				t.Fatalf("rewritten prompt still renders %q from a stale cache: %q", tc.marker, after)
			}
		})
	}
}
