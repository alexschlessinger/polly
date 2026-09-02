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
	tui := &gotuiTurnUI{repl: r, config: r.config, turnID: m.turnID}
	tui.ShowThinking(strings.Repeat("weighing the options carefully ", 6))
	call := messages.ChatMessageToolCall{ID: "probe", Name: "bash"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	tui.AppendToolEnd(call, "ok", time.Second, nil)
	tui.AppendAssistantText("Done.")
	r.endTurn(nil)
	trailer := m.turnTrailers[m.turnTrailerSeq]
	if trailer == nil {
		t.Fatal("turn did not settle a trailer")
	}

	var prose []string
	for i := 0; i < 30; i++ {
		prose = append(prose, fmt.Sprintf("ctx-%02d", i))
	}
	m.appendLine(strings.Join(prose, "\n"))

	start, count, ok := m.displayRecordSpan(width, matchTurnTrailerBlock(trailer.id))
	if !ok {
		t.Fatal("trailer block was not laid out")
	}
	m.followBottom = false
	m.scrollAnchor = start + count
	beforeRows := transcriptRowsText(m.transcriptRows(width))
	beforeTop := beforeRows[m.scrollAnchor]
	if !strings.HasPrefix(beforeTop, "ctx-") {
		t.Fatalf("fixture top row = %q, want the prose just below the trailer", beforeTop)
	}

	if !m.toggleTurnTrailerOverlay(trailer, turnDockOverlayTools) {
		t.Fatal("trailer did not expand")
	}
	afterRows := transcriptRowsText(m.transcriptRows(width))
	if got := afterRows[m.scrollAnchor]; got != beforeTop {
		t.Fatalf("held row moved under the trailer overlay: %q -> %q", beforeTop, got)
	}
}
