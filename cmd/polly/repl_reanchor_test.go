package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
)

// TestToggleAllDisclosuresReanchorsAcrossEveryBlock pins that a whole-view
// toggle keeps a held viewport steady when several blocks above it change
// height at once: the anchor moves by their summed growth, not by the first
// block's alone.
func TestToggleAllDisclosuresReanchorsAcrossEveryBlock(t *testing.T) {
	const width = 60
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.reasoningWidth = width
	for i, topic := range []string{"first", "second"} {
		m.beginTurn(topic)
		tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config, turnID: m.turnID}
		tui.ShowThinking(strings.Repeat(topic+" deliberation ", 8))
		call := messages.ChatMessageToolCall{ID: topic, Name: "bash"}
		tui.AppendToolStart([]messages.ChatMessageToolCall{call})
		tui.AppendToolEnd(call, strings.Repeat(topic+" output detail\n", 3), time.Second, nil)
		tui.AppendAssistantText(fmt.Sprintf("Answer %d.", i))
		r.endTurn(nil)
	}
	if m.reasoningRecords.count() != 2 || m.toolDisclosures.count() != 2 {
		t.Fatalf("fixture left %d thought and %d tool records, want 2 of each", m.reasoningRecords.count(), m.toolDisclosures.count())
	}
	var prose []string
	for i := 0; i < 30; i++ {
		prose = append(prose, fmt.Sprintf("ctx-%02d", i))
	}
	m.appendLine(strings.Join(prose, "\n"))

	rows := transcriptRowsText(m.transcriptRows(width))
	m.followBottom = false
	m.scrollAnchor = len(rows) - 10
	beforeTop := rows[m.scrollAnchor]
	if !strings.HasPrefix(beforeTop, "ctx-") {
		t.Fatalf("fixture top row = %q, want the prose below both thoughts", beforeTop)
	}

	// The default keeps the newest turn's line open, so that is the baseline
	// the cycle returns to.
	newestThoughts, newestTools := m.newestLineRecords()
	foldedCount, openedCount := len(rows), 0
	for _, step := range []struct {
		name    string
		mode    disclosureMode
		all     bool
		details bool
	}{
		{"opened", disclosuresOpen, true, false},
		{"detailed", disclosuresDetailed, true, true},
		{"folded", disclosuresFolded, false, false},
		{"newest open", disclosuresDefault, false, false},
		{"reopened", disclosuresOpen, true, false},
	} {
		beforeCount, beforeAnchor := len(rows), m.scrollAnchor
		if !m.cycleInlineDisclosures(width) {
			t.Fatalf("%s: view held nothing to toggle", step.name)
		}
		if m.disclosures != step.mode {
			t.Fatalf("%s: mode = %v, want %v", step.name, m.disclosures, step.mode)
		}
		for id, record := range m.reasoningRecords.all() {
			if want := step.all || step.mode == disclosuresDefault && newestThoughts[id]; record.expanded != want {
				t.Fatalf("%s: thought %d expanded = %v, want %v", step.name, record.id, record.expanded, want)
			}
		}
		for id, record := range m.toolDisclosures.all() {
			if want := step.all || step.mode == disclosuresDefault && newestTools[id]; record.expanded != want {
				t.Fatalf("%s: tool block %d expanded = %v, want %v", step.name, record.id, record.expanded, want)
			}
			for _, row := range record.rows {
				if row.outputExpanded != step.details {
					t.Fatalf("%s: output %q expanded = %v, want %v", step.name, row.callID, row.outputExpanded, step.details)
				}
			}
		}
		rows = transcriptRowsText(m.transcriptRows(width))
		text := strings.Join(rows, "\n")
		for _, topic := range []string{"first", "second"} {
			if got := strings.Contains(text, topic+" output detail"); got != step.details {
				t.Fatalf("%s: %s output visible = %v, want %v", step.name, topic, got, step.details)
			}
		}
		switch step.name {
		case "opened":
			openedCount = len(rows)
			if openedCount <= foldedCount {
				t.Fatalf("opening every block did not grow the transcript: %d -> %d rows", foldedCount, openedCount)
			}
		case "detailed":
			if len(rows) <= openedCount {
				t.Fatalf("showing output details did not grow the transcript: %d -> %d rows", openedCount, len(rows))
			}
		case "folded":
			if len(rows) >= foldedCount {
				t.Fatalf("folding every block did not shrink below the default: got %d, default %d", len(rows), foldedCount)
			}
		case "newest open":
			if len(rows) != foldedCount {
				t.Fatalf("returning to the default did not restore the original row count: got %d, want %d", len(rows), foldedCount)
			}
		case "reopened":
			if len(rows) != openedCount {
				t.Fatalf("reopening did not restore the opened row count: got %d, want %d", len(rows), openedCount)
			}
		}
		if want := beforeAnchor + len(rows) - beforeCount; m.scrollAnchor != want {
			t.Fatalf("%s: anchor = %d, want %d after all blocks resized", step.name, m.scrollAnchor, want)
		}
		if got := rows[m.scrollAnchor]; got != beforeTop {
			t.Fatalf("%s: held row moved: %q -> %q", step.name, beforeTop, got)
		}
	}
}
