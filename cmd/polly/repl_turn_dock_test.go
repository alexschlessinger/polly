package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	ui "github.com/metaspartan/gotui/v5"
)

func TestTurnDockDetachesIntoTranscriptTrailerOnSettlement(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("explain")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config, turnID: m.turnID}

	tui.ShowThinking("inspect the response framing and preserve the newest reasoning tail")
	call := messages.ChatMessageToolCall{ID: "read", Name: "read_file"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	tui.AppendToolEnd(call, "ok", 1200*time.Millisecond, nil)
	tui.AppendAssistantText("The stream closed before its final event.")
	tui.RecordTurnTokens(18600, 1200)
	m.turnStarted = time.Now().Add(-34 * time.Second)

	assistantIndex := m.currentAssistant
	transcriptLen := len(m.transcript)
	r.endTurn(nil)
	m.renderPendingMarkdown()
	if len(m.transcript) != transcriptLen+1 {
		t.Fatalf("settling did not append exactly one trailer: before=%d after=%d transcript=%#v", transcriptLen, len(m.transcript), m.transcript)
	}
	if assistantIndex < 0 || !strings.Contains(plainStyledText(m.transcript[assistantIndex].text), "stream closed") {
		t.Fatalf("assistant entry moved or changed at settlement: index=%d transcript=%#v", assistantIndex, m.transcript)
	}
	if m.turnDock.visible {
		t.Fatalf("settled dock remained attached to bottom row: %#v", m.turnDock)
	}
	plain := plainStyledText(m.transcript[len(m.transcript)-1].text)
	if plain != "  ✓ 34.0s · 18.6k in / 1.2k out" {
		t.Errorf("settled trailer = %q, want the status row alone", plain)
	}
	if m.turnTrailers.count() != 1 {
		t.Fatalf("attached trailer records = %#v", m.turnTrailers)
	}
	visible := plainStyledText(strings.Join(rowsText(m.transcriptRows(160)), "\n"))
	// The activity stays inline where it occurred, once; the trailer only
	// reports the outcome, the time, and the tokens.
	if strings.Count(visible, "1 tool") != 1 || strings.Count(visible, "thought") != 1 {
		t.Fatalf("settled activity did not render inline exactly once: %q", visible)
	}
}

func TestLiveActivityRendersInlineNotInDock(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("work")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config, turnID: m.turnID}
	tui.ShowThinking("reasoning detail")
	tui.AppendToolStart([]messages.ChatMessageToolCall{{ID: "read", Name: "read_file"}})

	// Live activity is inline in the transcript where it occurs. The reasoning
	// segment settled when the tool phase began, so it already reads "thought".
	visible := plainStyledText(strings.Join(rowsText(m.transcriptRows(100)), "\n"))
	if !strings.Contains(visible, "thought") || !strings.Contains(visible, "1 tool") {
		t.Fatalf("live activity missing from transcript: %q", visible)
	}
	// The dock is status-only: no thought/tools fields while the turn runs.
	dock, _ := m.turnDockRow(100)
	plainDock := plainStyledText(dock)
	if strings.Contains(plainDock, "thought") || strings.Contains(plainDock, "1 tool") {
		t.Fatalf("live dock carried activity disclosures: %q", plainDock)
	}
}

func TestAttachedTrailerRemainsWhenNextTurnStarts(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("first")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config, turnID: m.turnID}
	tui.AppendAssistantText("first answer")
	tui.RecordTurnTokens(1200, 300)
	m.turnStarted = time.Now().Add(-2 * time.Second)
	r.endTurn(nil)

	m.beginTurn("second")
	history := plainStyledText(strings.Join(transcriptTexts(m), "\n"))
	if !strings.Contains(history, "✓ 2.0s") || !strings.Contains(history, "1.2k in / 300 out") {
		t.Fatalf("previous turn trailer missing from transcript history: %q", history)
	}
	if !strings.Contains(history, "▎ second") {
		t.Fatalf("second prompt missing from transcript: %q", history)
	}
	if !m.turnDock.visible || m.turnDock.settled || m.turnDock.inputTokens != 0 || m.turnDock.outputTokens != 0 {
		t.Fatalf("new turn did not immediately own a clean dock: %#v", m.turnDock)
	}
	if dock, _ := m.turnDockRow(100); strings.Contains(plainStyledText(dock), "1.2k") {
		t.Fatalf("new turn dock retained previous metrics: %q", plainStyledText(dock))
	}
}

func TestLiveTurnDockIsStatusOnly(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("work")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config, turnID: m.turnID}
	tui.ShowThinking(strings.Repeat("newest reasoning detail ", 20))
	call := messages.ChatMessageToolCall{ID: "bash", Name: "bash"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})

	// The live dock carries only token counts (the running elapsed time is
	// the status row's job); activity renders inline in the transcript, so
	// the dock exposes no clickable overlay fields.
	_, placements := m.turnDockRow(80)
	for _, p := range placements {
		if p.kind != activityNone {
			t.Fatalf("live dock exposed an activity field: %#v", placements)
		}
	}

	// The activity itself is inline: one collapsed reasoning block and one
	// collapsed tool block, both visible in the transcript projection.
	entries := m.transcriptDisplayEntries(100)
	var reasoning, tools int
	for _, e := range entries {
		if len(e.reasoningIDs) > 0 {
			reasoning++
		}
		if len(e.toolDisclosureIDs) > 0 {
			tools++
		}
	}
	if reasoning != 1 || tools != 1 {
		t.Fatalf("inline activity blocks: reasoning=%d tools=%d, want 1 each", reasoning, tools)
	}
}

func TestPriorThoughtExpandsInlineWithoutCoveringCurrentDock(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("first")
	firstUI := &gotuiTurnUI{repl: r, model: r.model, config: r.config, turnID: m.turnID}
	firstUI.ShowThinking("prior reasoning detail")
	firstUI.AppendAssistantText("first answer")
	r.endTurn(nil)
	prior := m.reasoningRecords.get(m.reasoningOrder[0])

	m.beginTurn("second")
	if !m.turnDock.visible {
		t.Fatal("second turn did not own the fixed dock")
	}
	if !m.toggleReasoning(prior.id, 100) {
		t.Fatal("prior thought did not expand")
	}
	shown := plainStyledText(m.transcript[prior.transcriptIndex].text)
	if !strings.Contains(shown, "prior reasoning detail") {
		t.Fatalf("prior detail did not expand beneath its row: %q", shown)
	}
	if !m.turnDock.visible {
		t.Fatal("expanding a prior thought displaced the current fixed dock")
	}
}

func TestExpandedToolsPreserveLiteralBracketsWithoutLeakingStyleMarkup(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("inspect")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config, turnID: m.turnID}
	calls := []messages.ChatMessageToolCall{
		{ID: "grep", Name: "grep", Arguments: `{"pattern":"["}`},
		{ID: "read", Name: "read_file", Arguments: `{"path":"notes.txt"}`},
	}
	assertRendered := func(stage string, rows [][]ui.Cell) {
		t.Helper()
		shown := strings.Join(rowsText(rows), "\n")
		for _, want := range []string{"grep [", "read_file notes.txt"} {
			if !strings.Contains(shown, want) {
				t.Fatalf("%s expanded tools %q missing %q", stage, shown, want)
			}
		}
		for _, leaked := range []string{"fg:muted", "fg:ok", "mod:bold"} {
			if strings.Contains(shown, leaked) {
				t.Fatalf("%s expanded tools exposed style markup %q: %q", stage, leaked, shown)
			}
		}
	}
	tui.AppendToolStart(calls)
	live := m.currentToolDisclosure()
	if live == nil || !m.toggleToolDisclosure(live.id) {
		t.Fatal("live tool disclosure did not expand")
	}
	assertRendered("live", m.transcriptRows(100))
	if !m.toggleToolDisclosure(live.id) {
		t.Fatal("live tool disclosure did not collapse")
	}
	for _, call := range calls {
		tui.AppendToolEnd(call, "one\ntwo", 100*time.Millisecond, nil)
	}
	tui.AppendAssistantText("Done.")
	r.endTurn(nil)

	if !m.toggleToolDisclosure(live.id) {
		t.Fatal("completed tool disclosure did not expand")
	}
	assertRendered("completed", m.transcriptRows(100))
}

func TestQueuedTurnSwitchesDockWithoutAddingTrailer(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("first")
	first := &gotuiTurnUI{repl: r, model: r.model, config: r.config, turnID: m.turnID}
	first.AppendAssistantText("first answer")

	m.queue = append(m.queue, queuedREPLInput{text: "second", turn: func() *managedTurnInput {
		turn := textManagedTurn("second")
		return &turn
	}()})
	m.appendQueuedInput(&m.queue[0])
	r.endTurn(nil)
	transcriptLen := len(m.transcript)

	done := r.startNextQueued(context.Background(), func(context.Context, string, TurnUI) error { return nil })
	if done == nil {
		t.Fatal("queued turn did not start")
	}
	<-done
	history := plainStyledText(strings.Join(transcriptTexts(m), "\n"))
	if len(m.transcript) != transcriptLen {
		t.Fatalf("queued dock switch inserted transcript rows: before=%d after=%d", transcriptLen, len(m.transcript))
	}
	if strings.Contains(history, "(queued)") {
		t.Fatalf("activated queued prompt kept queued marker: %q", history)
	}
}

func TestHydratedHistoryRestoresAttachedTrailers(t *testing.T) {
	firstAssistant := messages.ChatMessage{
		Role:      messages.MessageRoleAssistant,
		Content:   "first answer",
		Reasoning: "first persisted reasoning",
		ToolCalls: []messages.ChatMessageToolCall{{ID: "a", Name: "read_file"}},
	}
	firstAssistant.SetTokenUsage(1000, 200)
	secondAssistant := messages.ChatMessage{
		Role:      messages.MessageRoleAssistant,
		Content:   "second answer",
		Reasoning: "second persisted reasoning",
	}
	secondAssistant.SetTokenUsage(1800, 350)
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "first"},
		firstAssistant,
		{Role: messages.MessageRoleTool, ToolCallID: "a", ToolName: "read_file", Content: "ok"},
		{Role: messages.MessageRoleUser, Content: "second"},
		secondAssistant,
	}

	m := newReplModel()
	m.hydrateHistory(history, "ctx")
	transcript := plainStyledText(strings.Join(transcriptTexts(m), "\n"))
	if !strings.Contains(transcript, "1.0k in / 200 out") || !strings.Contains(transcript, "1.8k in / 350 out") {
		t.Fatalf("hydrated turn trailers missing: %q", transcript)
	}
	if m.turnDock.visible || m.turnTrailers.count() != 2 {
		t.Fatalf("hydrated trailers/dock = trailers:%#v dock:%#v", m.turnTrailers, m.turnDock)
	}
	latest := m.turnTrailers.latest()
	// No duration survives in history, so the row is the outcome and tokens.
	if plain := plainStyledText(m.transcript[latest.transcriptIndex].text); plain != "  ✓ · 1.8k in / 350 out" {
		t.Fatalf("latest hydrated trailer = %q", plain)
	}
	if !strings.Contains(transcript, "▸ thought") {
		t.Fatalf("hydrated reasoning missing its inline row: %q", transcript)
	}
}

// A hydrated turn with neither a duration nor a token count would leave a
// bare check mark; it leaves nothing instead.
func TestHydratedTurnWithoutUsageLeavesNoTrailer(t *testing.T) {
	m := newReplModel()
	m.hydrateHistory([]messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "first"},
		{Role: messages.MessageRoleAssistant, Content: "first answer"},
	}, "ctx")
	if m.turnTrailers.count() != 0 {
		t.Fatalf("usage-free turn attached a trailer: %#v", m.turnTrailers)
	}
	if transcript := plainStyledText(strings.Join(transcriptTexts(m), "\n")); strings.Contains(transcript, "✓") {
		t.Fatalf("usage-free turn rendered a bare outcome: %q", transcript)
	}
}

func TestQuietModeSuppressesTurnDock(t *testing.T) {
	r := newManagedREPL(&Config{Quiet: true}, "ctx", 0, 0)
	r.model.beginTurn("quiet")
	if r.model.turnDock.visible {
		t.Fatalf("quiet turn exposed dock: %#v", r.model.turnDock)
	}
	if dock, placements := r.model.turnDockRow(80); dock != "" || placements != nil {
		t.Fatalf("quiet dock rendered %q / %#v", dock, placements)
	}
}

func rowsText(rows [][]ui.Cell) []string {
	out := make([]string, len(rows))
	for i, row := range rows {
		var b strings.Builder
		for _, cell := range row {
			b.WriteRune(cell.Rune)
		}
		out[i] = b.String()
	}
	return out
}
