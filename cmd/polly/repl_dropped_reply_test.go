package main

import (
	"strings"
	"testing"
)

// A reply the agent dropped stays on screen, closed and labeled as not part
// of the conversation; the label is not the turn's outcome.
func TestDropAssistantBlockLabelsTheReply(t *testing.T) {
	m := newReplModel()
	m.appendAssistant("let me look")
	m.dropAssistantBlock()
	got := m.flattenTranscript()
	if len(got) < 2 || !strings.Contains(got[len(got)-1], "dropped · not in the conversation") {
		t.Fatalf("transcript after drop: %q", got)
	}
	if m.currentAssistant != -1 || m.outcomeLabeled {
		t.Fatalf("currentAssistant=%d outcomeLabeled=%v", m.currentAssistant, m.outcomeLabeled)
	}
	// With nothing streamed there is nothing to label.
	m.dropAssistantBlock()
	if again := m.flattenTranscript(); len(again) != len(got) {
		t.Fatalf("empty drop added lines: %q", again)
	}
}
