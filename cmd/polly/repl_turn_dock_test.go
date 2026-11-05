package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	ui "github.com/metaspartan/gotui/v5"
)

func TestSettledTurnRidesOnItsActivityLine(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.depth = depthCollapsed // the checks below open a folded line by hand
	m.beginTurn("explain")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config, turnID: m.turnID}

	tui.ShowThinking("inspect the response framing and preserve the newest reasoning tail")
	call := messages.ChatMessageToolCall{ID: "read", Name: "read_file"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	tui.AppendToolEnd(call, "ok", 1200*time.Millisecond, nil)
	tui.AppendAssistantText("The stream closed before its final event.")
	tui.RecordTurnTokens(18600, 1200, false)
	m.turnStarted = time.Now().Add(-34 * time.Second)

	assistantIndex := m.currentAssistant
	transcriptLen := len(m.transcript)
	record := m.currentToolDisclosure()
	r.endTurn(nil)
	m.renderPendingMarkdown()
	if len(m.transcript) != transcriptLen {
		t.Fatalf("settling appended a trailer row: before=%d after=%d transcript=%#v", transcriptLen, len(m.transcript), m.transcript)
	}
	if assistantIndex < 0 || !strings.Contains(plainStyledText(m.transcript[assistantIndex].text), "stream closed") {
		t.Fatalf("assistant entry moved or changed at settlement: index=%d transcript=%#v", assistantIndex, m.transcript)
	}
	if m.turnDock.visible {
		t.Fatalf("settled dock remained attached to bottom row: %#v", m.turnDock)
	}
	// The tool record is the turn's last activity, so it carries the dock
	// and its line ends with the turn's time; the tokens are /context's.
	if record.trailer == nil || record.trailer.inputTokens != 18600 || m.turnTrailers.count() != 0 {
		t.Fatalf("settled turn did not land on its activity record: trailer=%#v rows=%d", record.trailer, m.turnTrailers.count())
	}
	visible := plainStyledText(strings.Join(rowsText(m.transcriptRows(160)), "\n"))
	var line string
	for _, row := range strings.Split(visible, "\n") {
		if strings.Contains(row, "1 tool call") {
			line = row
		}
	}
	if !strings.HasPrefix(line, "▸ thought") || !strings.HasSuffix(line, "  34.0s") || strings.Contains(visible, "18.6k") {
		t.Fatalf("settled activity line = %q in %q", line, visible)
	}
	if got := m.lastTurnSummary(); got != "34.0s · 18.6k in / 1.2k out" {
		t.Fatalf("last turn summary = %q", got)
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
	if !strings.Contains(visible, "thought") || !strings.Contains(visible, "read") {
		t.Fatalf("live activity missing from transcript: %q", visible)
	}
	// There is no dock row while the turn runs: the clock is in the status
	// row, and the tokens are /context's.
	if dock, _ := m.turnDockRow(100); strings.TrimSpace(plainStyledText(dock)) != "" {
		t.Fatalf("live dock drew a row: %q", plainStyledText(dock))
	}
}

func TestAttachedTrailerRemainsWhenNextTurnStarts(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("first")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config, turnID: m.turnID}
	tui.AppendAssistantText("first answer")
	tui.RecordTurnTokens(1200, 300, false)
	m.turnStarted = time.Now().Add(-2 * time.Second)
	r.endTurn(nil)

	m.beginTurn("second")
	history := plainStyledText(strings.Join(transcriptTexts(m), "\n"))
	if !strings.Contains(history, "✓ 2.0s") || strings.Contains(history, "1.2k in") {
		t.Fatalf("previous turn trailer wants the outcome and time alone: %q", history)
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
	m.depth = depthCollapsed // the check below opens a folded line by hand
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
	m.refreshReasoningRecords(100)
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
	m.depth = depthCollapsed // the checks below open a folded line by hand
	m.beginTurn("inspect")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config, turnID: m.turnID}
	calls := []messages.ChatMessageToolCall{
		{ID: "grep", Name: "grep", Arguments: `{"pattern":"["}`},
		{ID: "read", Name: "read_file", Arguments: `{"path":"notes.txt"}`},
	}
	assertRendered := func(stage string, rows [][]ui.Cell) {
		t.Helper()
		shown := strings.Join(rowsText(rows), "\n")
		for _, want := range []string{"grep [", "read notes.txt"} {
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

func TestHydratedHistoryHandsTurnsToTheirActivityLines(t *testing.T) {
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
	m.depth = depthCollapsed // the checks below open a folded line by hand
	m.hydrateHistory(history, "ctx")
	transcript := plainStyledText(strings.Join(transcriptTexts(m), "\n"))
	// Both turns had activity, so neither leaves a trailer row, and the
	// transcript repeats no token count.
	if m.turnDock.visible || m.turnTrailers.count() != 0 || strings.Contains(transcript, " in / ") {
		t.Fatalf("hydrated trailers/dock = trailers:%#v dock:%#v transcript:%q", m.turnTrailers, m.turnDock, transcript)
	}
	var docks []int
	for _, record := range m.toolDisclosures.all() {
		if record.trailer != nil {
			docks = append(docks, record.trailer.inputTokens)
		}
	}
	for _, record := range m.reasoningRecords.all() {
		if record.trailer != nil {
			docks = append(docks, record.trailer.inputTokens)
		}
	}
	if len(docks) != 2 || docks[0]+docks[1] != 2800 {
		t.Fatalf("hydrated turns did not land on their activity records: %v", docks)
	}
	if strings.Count(transcript, "▸ thought") != 2 {
		t.Fatalf("hydrated reasoning missing its inline rows: %q", transcript)
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

func TestLastTurnSummaryShowsEstimatesAndCost(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("explain")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config, turnID: m.turnID}

	tui.RecordTurnTokens(18600, 40, true)
	tui.RecordTurnCost(0.0043210, true)
	if got := m.lastTurnSummary(); got != "~18.6k in / 40 out · ~$0.004321" {
		t.Fatalf("live summary = %q", got)
	}
	if live, _ := m.turnDockRow(160); strings.TrimSpace(plainStyledText(live)) != "" {
		t.Fatalf("live dock = %q", plainStyledText(live))
	}

	tui.RecordTurnTokens(18600, 1200, false)
	tui.RecordTurnCost(0.02, false)
	m.turnStarted = time.Now().Add(-34 * time.Second)
	r.endTurn(nil)
	m.renderPendingMarkdown()
	// A turn with no activity line leaves the outcome and time as a row.
	if got := plainStyledText(m.transcript[len(m.transcript)-1].text); got != "  ✓ 34.0s" {
		t.Fatalf("settled trailer = %q", got)
	}
	if got := m.lastTurnSummary(); got != "34.0s · 18.6k in / 1.2k out · $0.02" {
		t.Fatalf("settled summary = %q", got)
	}
}

func TestStatusRowKeepsSpendPrecisionStable(t *testing.T) {
	m := newReplModel()
	m.status.modelName = "gpt-mini"
	m.status.contextName = "ctx"
	m.status.recordContextUsage(1000, 10000)
	var spend sessionSpend
	m.status.spend = &spend

	for _, width := range []int{120, 40} {
		var modelField, sessionField, contextField statusSessionPlacement
		for i, tc := range []struct {
			usd  float64
			want string
		}{
			{0, "$0.00"},
			{0.00001234, "$0.00"},
			{0.004321, "$0.00"},
			{0.009, "$0.01"},
			{0.1, "$0.10"},
			{0.1234, "$0.12"},
			{0.999, "$1.00"},
			{1, "$1.00"},
			{1.23456, "$1.23"},
			{9.99, "$9.99"},
		} {
			for _, estimated := range []bool{false, true} {
				spend.observeTurn(turnCost{usd: tc.usd, known: true, estimated: estimated})
				row := strings.TrimSpace(plainStyledText(m.statusRow(width)))
				mark := " "
				if estimated {
					mark = "~"
				}
				want := "gpt-mini · ctx · " + mark + tc.want + " · 1.0k/10.0k"
				if row != want {
					t.Fatalf("status row at width %d, cost %v, estimated %v = %q, want %q", width, tc.usd, estimated, row, want)
				}
				if i == 0 && !estimated {
					modelField, sessionField, contextField = m.status.modelField, m.status.sessionField, m.status.contextField
				}
				if m.status.modelField != modelField || m.status.sessionField != sessionField || m.status.contextField != contextField {
					t.Fatalf("status fields shifted at width %d, cost %v, estimated %v", width, tc.usd, estimated)
				}
			}
		}
	}
}

func TestStatusRowShowsSessionSpend(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	if strings.Contains(plainStyledText(m.statusRow(120)), "$") {
		t.Fatal("status row shows a cost with no spend")
	}
	var spend sessionSpend
	m.status.spend = &spend
	spend.add(turnCost{usd: 0.5, known: true})
	spend.observeTurn(turnCost{usd: 0.34, known: true, estimated: true})
	row := plainStyledText(m.statusRow(120))
	if !strings.Contains(row, "~$0.84") {
		t.Fatalf("status row = %q, want the running session total", row)
	}
	// The total is dropped before the session name on a narrow row.
	if narrow := plainStyledText(m.statusRow(len("ctx"))); strings.Contains(narrow, "$") {
		t.Fatalf("narrow status row kept the cost: %q", narrow)
	}
}
