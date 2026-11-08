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
		// Longer than the abridged tail, so the full depth shows more.
		tui.ShowThinking(strings.Repeat(topic+" deliberation ", 40))
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

	// Both lines are open; the depth changes change what they list, and
	// collapsed folds them both. No depth opens an output.
	initialCount := len(rows)
	for _, step := range []struct {
		name  string
		depth disclosureDepth
	}{
		{"full", depthFull},
		{"calls", depthCalls},
		{"collapsed", depthCollapsed},
		{"abridged", depthAbridged},
	} {
		open := step.depth != depthCollapsed
		beforeCount, beforeAnchor := len(rows), m.scrollAnchor
		if !m.toggleAllDisclosures(width) {
			t.Fatalf("%s: view held nothing to toggle", step.name)
		}
		if m.depth != step.depth {
			t.Fatalf("%s: depth = %v, want %v", step.name, m.depth, step.depth)
		}
		for _, record := range m.reasoningRecords.all() {
			if record.expanded != open {
				t.Fatalf("%s: thought %d expanded = %v, want %v", step.name, record.id, record.expanded, open)
			}
		}
		for _, record := range m.toolDisclosures.all() {
			if record.expanded != open {
				t.Fatalf("%s: tool block %d expanded = %v, want %v", step.name, record.id, record.expanded, open)
			}
			for _, row := range record.rows {
				if row.outputExpanded {
					t.Fatalf("%s: output %q opened without a click", step.name, row.callID)
				}
			}
		}
		rows = transcriptRowsText((conversationView{}).Rows(m, width))
		text := strings.Join(rows, "\n")
		if strings.Contains(text, "output detail") {
			t.Fatalf("%s: an output showed without a click", step.name)
		}
		switch step.name {
		case "full":
			if len(rows) <= initialCount {
				t.Fatalf("the full depth did not grow the transcript: %d -> %d rows", initialCount, len(rows))
			}
		case "calls":
			if len(rows) >= initialCount {
				t.Fatalf("the calls depth did not shrink the transcript below abridged: %d, abridged %d", len(rows), initialCount)
			}
		case "collapsed":
			// A folded line is its header and latest call: no taller than
			// the calls depth's one call.
			if len(rows) > beforeCount {
				t.Fatalf("the collapsed depth grew the transcript: %d, calls %d", len(rows), beforeCount)
			}
		case "abridged":
			if len(rows) != initialCount {
				t.Fatalf("abridging did not restore the original row count: got %d, want %d", len(rows), initialCount)
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
