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

	rows := transcriptRowsText((conversationView{}).Rows(m, width))
	m.followBottom = false
	m.scrollAnchor = len(rows) - 10
	beforeTop := rows[m.scrollAnchor]
	if !strings.HasPrefix(beforeTop, "ctx-") {
		t.Fatalf("fixture top row = %q, want the prose below both thoughts", beforeTop)
	}

	// Every chunk starts folded. Expanding opens them all and collapsing
	// folds them all; neither press opens a thought's text or an output.
	initialCount := len(rows)
	for _, step := range []struct {
		name  string
		depth disclosureDepth
	}{
		{"expanded", depthExpanded},
		{"collapsed", depthCollapsed},
		{"expanded again", depthExpanded},
	} {
		open := step.depth == depthExpanded
		beforeCount, beforeAnchor := len(rows), m.scrollAnchor
		if !m.toggleAllDisclosures(width) {
			t.Fatalf("%s: view held nothing to toggle", step.name)
		}
		if m.depth != step.depth {
			t.Fatalf("%s: state = %v, want %v", step.name, m.depth.label(), step.depth.label())
		}
		for _, record := range m.reasoningRecords.all() {
			if record.anyOpen() {
				t.Fatalf("%s: thought %d opened without a click", step.name, record.id)
			}
		}
		for _, record := range m.toolDisclosures.all() {
			if m.lineOpenOf(record) != open {
				t.Fatalf("%s: chunk of record %d open = %v, want %v", step.name, record.id, m.lineOpenOf(record), open)
			}
			for _, row := range record.rows {
				if row.outputExpanded {
					t.Fatalf("%s: output %q opened without a click", step.name, row.callID)
				}
			}
		}
		rows = transcriptRowsText((conversationView{}).Rows(m, width))
		text := strings.Join(rows, "\n")
		if strings.Contains(text, "output detail") || strings.Contains(text, "deliberation") {
			t.Fatalf("%s: an output or a thought's text showed without a click", step.name)
		}
		// An open chunk adds its thought row and its call row.
		if want := initialCount + 4; open && len(rows) != want {
			t.Fatalf("%s: rows = %d, want %d", step.name, len(rows), want)
		}
		if !open && len(rows) != initialCount {
			t.Fatalf("%s: collapsing did not restore the original row count: got %d, want %d", step.name, len(rows), initialCount)
		}
		if want := beforeAnchor + len(rows) - beforeCount; m.scrollAnchor != want {
			t.Fatalf("%s: anchor = %d, want %d after all blocks resized", step.name, m.scrollAnchor, want)
		}
		if got := rows[m.scrollAnchor]; got != beforeTop {
			t.Fatalf("%s: held row moved: %q -> %q", step.name, beforeTop, got)
		}
	}
}
