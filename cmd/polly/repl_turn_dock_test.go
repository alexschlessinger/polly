package main

import (
	"context"
	"errors"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/messages"
	ui "github.com/metaspartan/gotui/v5"
)

func TestSettledTurnRidesOnItsPromptRow(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("explain")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config, turnID: m.turnID}

	tui.ShowThinking("inspect the response framing and preserve the newest reasoning tail")
	call := messages.ChatMessageToolCall{ID: "read", Name: "read_file", Arguments: `{"path":"stream.go"}`}
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
	// The prompt that started the turn carries the dock, so its row ends
	// with the turn's time and no chunk's does; the tokens are /context's.
	prompt := m.transcript[m.newestTurnStart()]
	if prompt.trailer == nil || prompt.trailer.inputTokens != 18600 || m.turnTrailers.count() != 0 {
		t.Fatalf("settled turn did not land on its prompt: trailer=%#v rows=%d", prompt.trailer, m.turnTrailers.count())
	}
	visible := plainStyledText(strings.Join(rowsText(m.transcriptRows(160)), "\n"))
	var line, header string
	for _, row := range strings.Split(visible, "\n") {
		if strings.HasPrefix(row, "▎ ") {
			line = row
		}
		if strings.HasPrefix(row, "▸ ") {
			header = row
		}
	}
	if !strings.HasSuffix(line, "  34.0s") || len([]rune(line)) != 160 || header != "▸ Read stream.go" || strings.Contains(visible, "18.6k") || record == nil {
		t.Fatalf("settled prompt row = %q, header = %q in %q", line, header, visible)
	}
	if got := m.lastTurnSummary(); got != "34.0s · 18.6k in / 1.2k out" {
		t.Fatalf("last turn summary = %q", got)
	}
}

// A settled turn's time sits at the right edge of its prompt's last row, two
// columns clear of the words. A row without room for the outcome keeps the
// time alone, and one without room for that keeps neither: settling never
// wraps or clips what the reader wrote.
func TestPromptTrailerFitsItsRowOrStaysOff(t *testing.T) {
	withDisplayTTY(t)
	for _, tc := range []struct {
		name, prompt string
		err          error
	}{
		{"done", "explain the layout", nil},
		{"failed", "explain the layout", errors.New("provider unavailable")},
		{"two lines", "explain the layout\nand the folds under it", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newManagedREPL(&Config{}, "ctx", 0, 0)
			m := r.model
			m.beginTurn(tc.prompt)
			tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
			tui.AppendAssistantText("An answer.")
			m.turnStarted = time.Now().Add(-34 * time.Second)
			prompt := func(width int) []string {
				var rows []string
				for _, row := range strings.Split(shown(m, width), "\n") {
					if strings.HasPrefix(row, "▎") {
						rows = append(rows, row)
					}
				}
				return rows
			}
			widths := []int{100, 44, 36, 30, 28, 26, 24, 22, 20, 12}
			before := map[int][]string{}
			for _, width := range widths {
				before[width] = prompt(width)
			}
			r.endTurn(tc.err)
			entry := m.transcript[m.newestTurnStart()]
			if entry.trailer == nil || m.turnTrailers.count() != 0 {
				t.Fatalf("the settled turn did not land on its prompt: %#v, %d status rows", entry.trailer, m.turnTrailers.count())
			}
			field := m.promptTrailerField(*entry.trailer)
			if field.elapsed != "34.0s" || (tc.err == nil) != (field.raw == field.elapsed) || (tc.err != nil && field.raw != "✗ failed · 34.0s") {
				t.Fatalf("prompt field = %+v", field)
			}
			fitted := map[string]bool{}
			for _, width := range widths {
				was, now := before[width], prompt(width)
				last := len(was) - 1
				if len(now) != len(was) || !slices.Equal(now[:last], was[:last]) {
					t.Fatalf("width %d: settling re-wrapped the prompt: %q -> %q", width, was, now)
				}
				room := width - style.TextWidth(was[last])
				want := was[last]
				for _, text := range []string{field.raw, field.elapsed} {
					if room >= 2+style.TextWidth(text) {
						want += strings.Repeat(" ", room-style.TextWidth(text)) + text
						fitted[text] = true
						break
					}
				}
				if want == was[last] {
					fitted[""] = true
				}
				if now[last] != want {
					t.Fatalf("width %d: prompt row %q, want %q", width, now[last], want)
				}
			}
			// The widths cover each case: the whole field, the time alone
			// where an outcome leads it, and nothing.
			if !fitted[field.raw] || !fitted[field.elapsed] || !fitted[""] {
				t.Fatalf("fixture widths missed a case: %v", fitted)
			}
		})
	}
}

// A turn that echoed no prompt has no row to carry its time; it keeps a row
// of its own, as before.
func TestTurnWithoutAPromptRowKeepsItsTrailerRow(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginManagedTurnState(textManagedTurn("unechoed"))
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	tui.AppendAssistantText("An answer.")
	m.turnStarted = time.Now().Add(-2 * time.Second)
	r.endTurn(nil)
	trailer := m.turnTrailers.latest()
	if trailer == nil || m.turnTrailers.count() != 1 {
		t.Fatalf("a turn with no prompt row left %d trailer rows", m.turnTrailers.count())
	}
	for i, entry := range m.transcript {
		if entry.trailer != nil {
			t.Fatalf("entry %d took the trailer of a turn that echoed no prompt", i)
		}
	}
	if got := shown(m, 80); !strings.HasSuffix(got, "✓ 2.0s") {
		t.Fatalf("the trailer row: %q", got)
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

	// Live activity is inline in the transcript where it occurs: one header
	// that names the call in flight, the thought that led it behind the fold.
	if visible := shown(m, 100); visible != "▎ work\n▸ Reading a file" {
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
	history := shown(m, 100)
	// The previous turn's time rides on its prompt's row, without tokens.
	if first := strings.SplitN(history, "\n", 2)[0]; !strings.HasPrefix(first, "▎ first ") || !strings.HasSuffix(first, "  2.0s") || len([]rune(first)) != 100 || strings.Contains(history, "1.2k in") {
		t.Fatalf("previous turn's prompt row wants the time alone at its right edge: %q", history)
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

func TestHydratedHistoryLeavesNoTrailerRows(t *testing.T) {
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
	// A replayed turn has no clock and finished, so it has nothing to say:
	// neither turn leaves a trailer row or a time on its prompt, and the
	// transcript repeats no token count. The last turn's tokens are
	// /context's to report.
	if m.turnDock.visible || m.turnTrailers.count() != 0 || strings.Contains(transcript, " in / ") {
		t.Fatalf("hydrated trailers/dock = trailers:%#v dock:%#v transcript:%q", m.turnTrailers, m.turnDock, transcript)
	}
	for i, entry := range m.transcript {
		if entry.trailer != nil {
			t.Fatalf("hydrated prompt %d carries a trailer with nothing to say: %#v", i, entry.trailer)
		}
	}
	if m.settledDock.inputTokens != 1800 {
		t.Fatalf("last hydrated turn's tokens = %d, want 1800", m.settledDock.inputTokens)
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
	// The time rides on the prompt's row; no row repeats it.
	if first := strings.SplitN(shown(m, 160), "\n", 2)[0]; !strings.HasPrefix(first, "▎ explain ") || !strings.HasSuffix(first, "  34.0s") || m.turnTrailers.latest() != nil {
		t.Fatalf("settled prompt row = %q, status row = %#v", first, m.turnTrailers.latest())
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

// A prompt queued while a turn runs is echoed in the middle of that turn's
// work. The turn's time still lands on the prompt that started it; the echo
// gets a time only when its own turn has run.
func TestQueuedPromptEchoNeverTakesAnotherTurnsTime(t *testing.T) {
	for _, activate := range []bool{true, false} {
		name := "discarded"
		if activate {
			name = "activated"
		}
		t.Run(name, func(t *testing.T) {
			withDisplayTTY(t)
			r := newManagedREPL(&Config{}, "ctx", 0, 0)
			m := r.model
			m.beginTurn("first")
			tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
			tui.AppendToolStart([]messages.ChatMessageToolCall{readCall("a")})
			tui.AppendToolEnd(readCall("a"), "ok", time.Second, nil)

			queuedTurn := textManagedTurn("second")
			item := queuedREPLInput{text: "second", turn: &queuedTurn}
			m.appendQueuedInput(&item)
			m.queue = append(m.queue, item)
			if !activate {
				m.discardQueuedInputs()
			}
			tui.AppendAssistantText("more work to do")
			tui.AppendToolStart([]messages.ChatMessageToolCall{readCall("b")})
			tui.AppendToolEnd(readCall("b"), "ok", time.Second, nil)
			m.turnStarted = time.Now().Add(-5 * time.Second)
			r.endTurn(nil)

			// timed maps each prompt row that carries a time to that time.
			timed := func() map[string]string {
				out := map[string]string{}
				for _, row := range strings.Split(shown(m, 60), "\n") {
					if match := regexp.MustCompile(`^▎ (\S+) +(\d+\.\ds)$`).FindStringSubmatch(row); match != nil {
						out[match[1]] = match[2]
					}
				}
				return out
			}
			if got := timed(); len(got) != 1 || got["first"] != "5.0s" {
				t.Fatalf("the first turn's time landed on %v", got)
			}
			if activate {
				m.queue = nil
				m.activateQueuedInput(item)
				m.beginManagedTurnState(queuedTurn)
			} else {
				m.beginTurn("third")
			}
			next := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
			next.AppendAssistantText("an answer")
			m.turnStarted = time.Now().Add(-7 * time.Second)
			r.endTurn(nil)
			want := map[string]string{"first": "5.0s", "third": "7.0s"}
			if activate {
				want = map[string]string{"first": "5.0s", "second": "7.0s"}
			}
			if got := timed(); !maps.Equal(got, want) {
				t.Fatalf("prompt rows with a time = %v, want %v", got, want)
			}
		})
	}
}
