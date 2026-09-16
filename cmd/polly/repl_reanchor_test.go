package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
)

// TestTrailerExpansionReanchorsAcrossMergedActivity pins that a settled
// trailer re-anchors a held viewport in display rows. The turn's thought and
// tool entries merge into one activity block above the trailer, so their raw
// per-entry heights over-count the rows before it; measuring in that space
// shifted the held row when the overlay opened.
func TestTrailerExpansionReanchorsAcrossMergedActivity(t *testing.T) {
	const width = 60
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.reasoningWidth = width
	m.beginTurn("investigate")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config, turnID: m.turnID}
	tui.ShowThinking(strings.Repeat("weighing the options carefully ", 6))
	call := messages.ChatMessageToolCall{ID: "probe", Name: "bash"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	tui.AppendToolEnd(call, "ok", time.Second, nil)
	tui.AppendAssistantText("Done.")
	r.endTurn(nil)
	trailer := m.turnTrailers.latest()
	if trailer == nil {
		t.Fatal("turn did not settle a trailer")
	}

	var prose []string
	for i := 0; i < 30; i++ {
		prose = append(prose, fmt.Sprintf("ctx-%02d", i))
	}
	m.appendLine(strings.Join(prose, "\n"))

	spans := m.displayRecordSpans(width, matchTurnTrailerBlock(trailer.id))
	if len(spans) == 0 {
		t.Fatal("trailer block was not laid out")
	}
	m.followBottom = false
	m.scrollAnchor = spans[0].start + spans[0].count
	beforeRows := transcriptRowsText(m.transcriptRows(width))
	beforeTop := beforeRows[m.scrollAnchor]
	if !strings.HasPrefix(beforeTop, "ctx-") {
		t.Fatalf("fixture top row = %q, want the prose just below the trailer", beforeTop)
	}

	if !m.toggleToolDisclosure(m.currentToolDisclosure().id) {
		t.Fatal("tool disclosure did not expand")
	}
	afterRows := transcriptRowsText(m.transcriptRows(width))
	if got := afterRows[m.scrollAnchor]; got != beforeTop {
		t.Fatalf("held row moved under the trailer overlay: %q -> %q", beforeTop, got)
	}
}

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
		tui.AppendAssistantText(fmt.Sprintf("Answer %d.", i))
		r.endTurn(nil)
	}
	if m.reasoningRecords.count() != 2 {
		t.Fatalf("fixture left %d thought records, want 2", m.reasoningRecords.count())
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

	if !m.toggleAllDisclosures(width) {
		t.Fatal("view held nothing to expand")
	}
	for _, record := range m.reasoningRecords.all() {
		if !record.expanded {
			t.Fatal("toggle did not expand every thought")
		}
	}
	afterRows := transcriptRowsText(m.transcriptRows(width))
	if len(afterRows) <= len(rows) {
		t.Fatalf("expanding two thoughts did not grow the transcript: %d -> %d rows", len(rows), len(afterRows))
	}
	if got := afterRows[m.scrollAnchor]; got != beforeTop {
		t.Fatalf("held row moved when every block opened: %q -> %q", beforeTop, got)
	}

	if !m.toggleAllDisclosures(width) {
		t.Fatal("view held nothing to collapse")
	}
	afterRows = transcriptRowsText(m.transcriptRows(width))
	if got := afterRows[m.scrollAnchor]; got != beforeTop {
		t.Fatalf("held row moved when every block closed: %q -> %q", beforeTop, got)
	}
}
