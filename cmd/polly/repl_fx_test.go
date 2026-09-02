package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	rw "github.com/mattn/go-runewidth"
	ui "github.com/metaspartan/gotui/v5"
)

func TestStreamCursorOnlyWhileStreaming(t *testing.T) {
	m := newReplModel()
	m.beginTurn("explain")
	m.appendAssistant("partial answer")

	// Waiting/thinking/tool states stream no visible text, so no caret.
	for _, s := range []turnState{turnStateWaiting, turnStateThinking, turnStateTool} {
		m.state = s
		m.refreshStreamCursor()
		if got := strings.Join(m.transcriptDisplayBlocks(), "\n"); strings.Contains(got, streamCursorGlyph) {
			t.Fatalf("state %v should show no stream caret, got %q", s, got)
		}
	}

	m.state = turnStateStreaming
	m.refreshStreamCursor()
	blocks := m.transcriptDisplayBlocks()
	last := blocks[len(blocks)-1]
	if !strings.Contains(last, streamCursorGlyph) {
		t.Fatalf("streaming block should end with the caret, got %q", last)
	}
	if !strings.HasPrefix(last, "partial answer") {
		t.Fatalf("caret should append to the streamed text, got %q", last)
	}

	// The caret is display-only chrome: the transcript itself stays clean.
	if strings.Contains(strings.Join(m.transcript, "\n"), streamCursorGlyph) {
		t.Fatalf("caret must never enter the transcript, got %#v", m.transcript)
	}

	// Cancel freezes the stream; the caret disappears on the next refresh.
	m.canceling = true
	m.refreshStreamCursor()
	if got := strings.Join(m.transcriptDisplayBlocks(), "\n"); strings.Contains(got, streamCursorGlyph) {
		t.Fatalf("canceling should hide the caret, got %q", got)
	}
}

func TestStreamCursorPulseInvalidatesVisualCache(t *testing.T) {
	m := newReplModel()
	m.beginTurn("explain")
	m.appendAssistant("answer")
	m.state = turnStateStreaming

	m.turnStarted = time.Now()
	m.refreshStreamCursor()
	m.transcriptRows(80)
	if !m.visualCacheValid {
		t.Fatal("cache should be valid after rendering rows")
	}

	// Same pulse phase: refresh must not thrash the cache.
	m.refreshStreamCursor()
	if !m.visualCacheValid {
		t.Fatal("unchanged caret frame should keep the visual cache")
	}

	// Advance time across a pulse boundary: the frame flips and invalidates.
	m.turnStarted = time.Now().Add(-arrowPulsePeriod)
	m.refreshStreamCursor()
	if m.visualCacheValid {
		t.Fatal("pulse flip should invalidate the visual cache")
	}
}

func TestFrameTitleReflectsTurnState(t *testing.T) {
	m := newReplModel()
	m.contextName = "mychat"

	if got := m.frameTitle(); got != "polly · mychat" {
		t.Fatalf("idle title = %q", got)
	}

	m.beginTurn("explain")
	m.state = turnStateThinking
	m.turnStarted = time.Now().Add(-12 * time.Second)
	if got := m.frameTitle(); got != "polly · mychat — thinking · 12s" {
		t.Fatalf("busy title = %q", got)
	}

	m.approval = &approvalState{}
	if got := m.frameTitle(); got != "polly · mychat — approval needed" {
		t.Fatalf("approval title = %q", got)
	}
	m.approval = nil

	m.busy = false
	m.lastOutcome = turnOutcomeDone
	m.lastElapsed = 1800 * time.Millisecond
	if got := m.frameTitle(); got != "polly · mychat — done · 1.8s" {
		t.Fatalf("done title = %q", got)
	}

	// The placeholder context name is not worth a title segment.
	m.contextName = "-"
	m.lastOutcome = turnOutcomeFailed
	if got := m.frameTitle(); got != "polly — failed" {
		t.Fatalf("failed title = %q", got)
	}
}

func TestFrameProgressStates(t *testing.T) {
	m := newReplModel()
	if got := m.frameProgress(); got != progressNone {
		t.Fatalf("idle progress = %q", got)
	}
	m.busy = true
	if got := m.frameProgress(); got != progressBusy {
		t.Fatalf("busy progress = %q", got)
	}
	m.busy = false
	m.lastOutcome = turnOutcomeFailed
	if got := m.frameProgress(); got != progressFail {
		t.Fatalf("failed progress = %q", got)
	}
	m.lastOutcome = turnOutcomeDone
	if got := m.frameProgress(); got != progressNone {
		t.Fatalf("done progress = %q", got)
	}
}

func TestEndTurnNoticeOnlyForLongTurns(t *testing.T) {
	// A quick turn never gave the user time to look away: no notice.
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	r.model.beginTurn("quick question")
	r.endTurn(nil)
	if len(r.model.notices) != 0 {
		t.Fatalf("short turn queued notices %#v", r.model.notices)
	}

	// A long successful turn notifies with elapsed and the prompt.
	r.model.beginTurn("summarize the big doc")
	r.model.turnStarted = time.Now().Add(-34 * time.Second)
	r.endTurn(nil)
	if len(r.model.notices) != 1 || !strings.Contains(r.model.notices[0], "done in 34s") ||
		!strings.Contains(r.model.notices[0], "summarize the big doc") {
		t.Fatalf("long turn notices = %#v", r.model.notices)
	}
	r.model.notices = nil

	// A long failure notifies too.
	r.model.beginTurn("another")
	r.model.turnStarted = time.Now().Add(-30 * time.Second)
	r.endTurn(errors.New("provider unavailable"))
	if len(r.model.notices) != 1 || !strings.Contains(r.model.notices[0], "failed after 30s") {
		t.Fatalf("failed turn notices = %#v", r.model.notices)
	}
}

func TestTakeNoticesGatesOnFocus(t *testing.T) {
	m := newReplModel()

	// Focus never reported: stay silent, and drop rather than stockpile.
	m.pushNotice("done in 30s")
	if got := m.takeNotices(); got != nil {
		t.Fatalf("unknown focus should emit nothing, got %#v", got)
	}
	if m.notices != nil {
		t.Fatalf("dropped notices should not linger, got %#v", m.notices)
	}

	// Focused: the user is watching; drop.
	m.focusKnown, m.focused = true, true
	m.pushNotice("done in 30s")
	if got := m.takeNotices(); got != nil {
		t.Fatalf("focused terminal should emit nothing, got %#v", got)
	}

	// Unfocused: deliver.
	m.focused = false
	m.pushNotice("done in 30s")
	if got := m.takeNotices(); len(got) != 1 || got[0] != "done in 30s" {
		t.Fatalf("unfocused terminal should deliver, got %#v", got)
	}
}

func TestFocusEventsUpdateModel(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	if r.model.focusKnown {
		t.Fatal("focus should start unknown")
	}
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: focusLostID})
	if !r.model.focusKnown || r.model.focused {
		t.Fatalf("focus lost not recorded: known=%v focused=%v", r.model.focusKnown, r.model.focused)
	}
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: focusGainedID})
	if !r.model.focusKnown || !r.model.focused {
		t.Fatalf("focus gain not recorded: known=%v focused=%v", r.model.focusKnown, r.model.focused)
	}

	// Focus reports are never input, even mid-paste.
	r.model.pasting = true
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: focusLostID})
	if !r.model.ed.empty() {
		t.Fatalf("focus event leaked into the editor: %q", r.model.ed.text())
	}
	if r.model.focused {
		t.Fatal("focus lost during paste should still be recorded")
	}
}

func TestSupportsProgressOSC(t *testing.T) {
	env := func(vals map[string]string) func(string) string {
		return func(k string) string { return vals[k] }
	}
	if supportsProgressOSC(env(map[string]string{"TERM_PROGRAM": "iTerm.app"})) {
		t.Fatal("iTerm2 must not receive OSC 9;4 — it parses OSC 9 as a notification")
	}
	if !supportsProgressOSC(env(map[string]string{"TERM_PROGRAM": "ghostty"})) {
		t.Fatal("ghostty supports OSC 9;4 progress")
	}
	if !supportsProgressOSC(env(map[string]string{"WT_SESSION": "some-guid"})) {
		t.Fatal("Windows Terminal supports OSC 9;4 progress")
	}
	if supportsProgressOSC(env(nil)) {
		t.Fatal("unknown terminal should get no progress writes")
	}
}

func TestSanitizeNoticeStripsControlRunes(t *testing.T) {
	got := sanitizeNotice("done\x1b]9;evil\x07 in 30s\n")
	if strings.ContainsAny(got, "\x1b\x07\n") {
		t.Fatalf("control runes survived: %q", got)
	}
	if !strings.Contains(got, "evil") || !strings.Contains(got, "in 30s") {
		t.Fatalf("printable text should survive: %q", got)
	}
}

// withDisplayTTY makes toolDisplayEnabled pass in tests so tool lines land in
// the transcript.
func withDisplayTTY(t *testing.T) {
	t.Helper()
	old := terminalFD
	terminalFD = func(int) bool { return true }
	t.Cleanup(func() { terminalFD = old })
}

func TestAppendToolEndAnnotatesLines(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.busy = true
	tui := &gotuiTurnUI{repl: r, config: r.config}

	calls := []messages.ChatMessageToolCall{{ID: "a", Name: "grep"}, {ID: "b", Name: "bash"}}
	tui.AppendToolStart(calls)

	// Success lines count the output the model ingested.
	tui.AppendToolEnd(calls[0], "hit one\nhit two\nhit three", time.Second, nil)
	record := m.currentToolDisclosure()
	if record == nil || !m.toggleToolDisclosure(record.id) {
		t.Fatalf("annotated tool disclosure did not expand: %#v", record)
	}
	if got := strings.Join(m.flattenTranscript(), "\n"); !strings.Contains(got, "3 lines") {
		t.Fatalf("success line should carry the line count, got %q", got)
	}

	// Failure lines carry the exit code and still no output text. The batch
	// disclosure stays expanded once every call has ended; only turn
	// settlement collapses it.
	probe := exec.Command("bash", "-c", "exit 7").Run()
	wrapped := fmt.Errorf("command failed: %w (output: secret stack trace)", probe)
	tui.AppendToolEnd(calls[1], "secret stack trace", time.Second, wrapped)
	if !record.expanded {
		t.Fatal("deliberately expanded disclosure collapsed at batch end")
	}
	got := strings.Join(m.flattenTranscript(), "\n")
	if !strings.Contains(got, "exit 7") {
		t.Fatalf("failure line should carry the exit code, got %q", got)
	}
	if strings.Contains(got, "secret stack trace") {
		t.Fatalf("failure line must not leak output text, got %q", got)
	}
}

func TestApprovalViewExpandsArgsOncePerCall(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	first, _ := json.Marshal(map[string]any{"command": "rm -rf ./build &&\nmake all"})
	second, _ := json.Marshal(map[string]any{"pattern": "TODO"})
	m.approval = &approvalState{
		calls: []messages.ChatMessageToolCall{
			{Name: "bash", Arguments: string(first)},
			{Name: "grep", Arguments: string(second)},
		},
		reply: make(chan []bool, 1),
	}

	view := ui.Event{Type: ui.KeyboardEvent, ID: "v"}
	r.handleEvent(view)
	got := strings.Join(m.flattenTranscript(), "\n")
	if !strings.Contains(got, "rm -rf ./build") || !strings.Contains(got, "make all") {
		t.Fatalf("[v]iew should expand the full command, got %q", got)
	}
	if !strings.Contains(got, "╭─ bash") {
		t.Fatalf("[v]iew block should name the tool, got %q", got)
	}

	// Holding v must not spam the transcript.
	before := len(m.flattenTranscript())
	r.handleEvent(view)
	if len(m.flattenTranscript()) != before {
		t.Fatal("repeated [v]iew duplicated the args block")
	}

	// Answering advances to the next call, which is viewable again.
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "y"})
	r.handleEvent(view)
	if got := strings.Join(m.flattenTranscript(), "\n"); !strings.Contains(got, "TODO") {
		t.Fatalf("[v]iew after advancing should expand the next call, got %q", got)
	}
}

const testThinkingWidth = 60

func TestThinkingDisclosureStartsCollapsedWithoutLeakingReasoning(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("explain")
	tui := &gotuiTurnUI{repl: r, config: r.config}

	const privateReasoning = "private chain about the parse loop"
	tui.ShowThinking(privateReasoning)
	m.refreshReasoningRecords(testThinkingWidth)

	record := m.currentReasoningRecord()
	if record == nil || len(m.reasoningRecords) != 1 || len(m.reasoningOrder) != 1 {
		t.Fatalf("reasoning record = %#v, records/order = %d/%d", record, len(m.reasoningRecords), len(m.reasoningOrder))
	}
	if record.expanded {
		t.Fatal("a turn's reasoning disclosure should start collapsed")
	}
	block := plainStyledText(m.transcript[record.transcriptIndex])
	if !strings.Contains(block, "▸ thinking") {
		t.Fatalf("collapsed disclosure = %q, want a thinking label", block)
	}
	if strings.Contains(block, privateReasoning) {
		t.Fatalf("collapsed disclosure leaked reasoning: %q", block)
	}
	if strings.Contains(block, "tok") {
		t.Fatalf("quiet disclosure should not show an approximate token count: %q", block)
	}
	if tail := string(record.tail); tail != privateReasoning {
		t.Fatalf("retained reasoning tail = %q, want %q", tail, privateReasoning)
	}
}

func TestThinkingDisclosureExpansionShowsBoundedLiveTail(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("explain")
	tui := &gotuiTurnUI{repl: r, config: r.config}

	for i := 0; i < 16; i++ {
		tui.ShowThinking(fmt.Sprintf("reasoning sentence number %d about the parse loop. ", i))
	}
	record := m.currentReasoningRecord()
	if record == nil || !m.toggleReasoning(record.id, testThinkingWidth) {
		t.Fatal("active reasoning disclosure did not expand")
	}

	block := plainStyledText(m.transcript[record.transcriptIndex])
	lines := strings.Split(block, "\n")
	if got := len(lines) - 1; got != reasoningPreviewLines {
		t.Fatalf("expanded preview rows = %d, want %d: %q", got, reasoningPreviewLines, block)
	}
	if !strings.Contains(block, "number 15") || strings.Contains(block, "number 0 ") {
		t.Fatalf("expanded disclosure should show only the newest tail: %q", block)
	}
	if !strings.Contains(block, "▾ thinking") || !strings.Contains(m.transcript[record.transcriptIndex], "mod:italic") {
		t.Fatalf("expanded disclosure lost its open label or preview styling: %q", m.transcript[record.transcriptIndex])
	}

	// New chunks repaint the open tail immediately without growing beyond the cap.
	tui.ShowThinking(" newest-live-marker")
	m.refreshReasoningRecord(record, testThinkingWidth)
	block = plainStyledText(m.transcript[record.transcriptIndex])
	if !strings.Contains(block, "newest-live-marker") {
		t.Fatalf("open disclosure did not follow the live stream: %q", block)
	}
	if got := strings.Count(block, "\n"); got != reasoningPreviewLines {
		t.Fatalf("live preview grew to %d rows, want %d: %q", got, reasoningPreviewLines, block)
	}
}

func TestThinkingDisclosurePreviewUsesFullTranscriptWidth(t *testing.T) {
	const width = 140
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("explain")
	tui := &gotuiTurnUI{repl: r, config: r.config}
	tui.ShowThinking(strings.Repeat("wide reasoning preview content ", 32))
	record := m.currentReasoningRecord()
	if record == nil || !m.toggleReasoning(record.id, width) {
		t.Fatal("reasoning disclosure did not expand")
	}

	want := width - rw.StringWidth(reasoningBlockIndent)
	if record.previewWidth != want {
		t.Fatalf("preview width = %d, want full available width %d", record.previewWidth, want)
	}
	if got := strings.Count(m.transcript[record.transcriptIndex], "\n"); got > reasoningPreviewLines {
		t.Fatalf("expanded preview rows = %d, want at most %d", got, reasoningPreviewLines)
	}
}

func TestSettledThinkingPreviewKeepsNarrowTerminalWidth(t *testing.T) {
	const width = 24
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.reasoningWidth = width
	m.beginTurn("explain")
	tui := &gotuiTurnUI{repl: r, config: r.config}
	tui.ShowThinking(strings.Repeat("narrow reasoning preview content ", 32))
	record := m.currentReasoningRecord()
	if record == nil || !m.toggleReasoning(record.id, width) {
		t.Fatal("reasoning disclosure did not expand")
	}

	wantWidth := width - rw.StringWidth(reasoningBlockIndent)
	if record.previewWidth != wantWidth {
		t.Fatalf("live preview width = %d, want %d", record.previewWidth, wantWidth)
	}

	// Starting a tool settles the reasoning segment from a provider callback.
	// The settled preview must not silently rerender at the 80-column fallback.
	call := messages.ChatMessageToolCall{ID: "narrow", Name: "inspect"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	if record.previewWidth != wantWidth {
		t.Fatalf("settled preview width = %d, want retained terminal width %d", record.previewWidth, wantWidth)
	}
	rows := transcriptBlockRows(m.transcript[record.transcriptIndex], false, width)
	if got := len(rows) - 1; got > reasoningPreviewLines {
		t.Fatalf("settled preview uses %d physical detail rows, want at most %d", got, reasoningPreviewLines)
	}
}

func TestThinkingDockAutoCollapsesAndIdleCtrlOReopens(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("explain")
	tui := &gotuiTurnUI{repl: r, config: r.config}
	tui.ShowThinking(strings.Repeat("bounded reasoning tail ", 16))
	record := m.currentReasoningRecord()
	if record == nil || !m.toggleReasoning(record.id, testThinkingWidth) || !record.expanded {
		t.Fatal("fixture reasoning disclosure did not expand")
	}

	r.endTurn(nil)
	if record.expanded || !record.complete || record.active || m.currentReasoningRecord() != nil {
		t.Fatalf("completed reasoning record did not settle collapsed: %#v", record)
	}
	if m.turnDock.overlay != turnDockOverlayNone {
		t.Fatalf("completed dock retained its open drawer: %#v", m.turnDock)
	}

	// With no active turn, Ctrl-O targets the Thought control in the dock.
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
	trailer := m.turnTrailers[m.turnTrailerSeq]
	if trailer == nil || trailer.dock.overlay != turnDockOverlayThought {
		t.Fatal("idle Ctrl-O did not open the completed Thought drawer")
	}
	opened := plainStyledText(m.transcript[trailer.transcriptIndex])
	if !strings.Contains(opened, "bounded reasoning") {
		t.Fatalf("reopened completed Thought drawer = %q", opened)
	}
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
	if trailer.dock.overlay != turnDockOverlayNone {
		t.Fatal("second idle Ctrl-O did not close the completed Thought drawer")
	}
}

func TestCtrlOPrearmsActiveTurnBeforeFirstReasoningChunk(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("first")
	firstUI := &gotuiTurnUI{repl: r, config: r.config}
	firstUI.ShowThinking("older completed reasoning")
	older := m.currentReasoningRecord()
	r.endTurn(nil)

	m.beginTurn("second")
	if m.currentReasoningRecord() != nil {
		t.Fatal("active turn unexpectedly had reasoning before its first chunk")
	}
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
	if !m.turnReasoningOpen || older.expanded {
		t.Fatalf("pre-arm targeted the wrong turn: pending=%v older=%#v", m.turnReasoningOpen, older)
	}

	secondUI := &gotuiTurnUI{repl: r, config: r.config}
	secondUI.ShowThinking("new live reasoning")
	active := m.currentReasoningRecord()
	m.refreshReasoningRecords(testThinkingWidth)
	if active == nil || !active.expanded {
		t.Fatalf("first reasoning chunk did not honor the pre-armed toggle: record=%#v", active)
	}
	if m.turnReasoningOpen {
		t.Fatal("first reasoning record did not consume the pending Ctrl-O pre-arm")
	}
	if shown := plainStyledText(m.transcript[active.transcriptIndex]); !strings.Contains(shown, "new live reasoning") {
		t.Fatalf("pre-armed inline block did not show the live tail: %q", shown)
	}

	// A tool phase pauses rather than breaks the run: the continuation
	// resumes the same record and keeps the deliberate expansion.
	call := messages.ChatMessageToolCall{ID: "between", Name: "inspect"}
	secondUI.AppendToolStart([]messages.ChatMessageToolCall{call})
	secondUI.AppendToolEnd(call, "ok", time.Millisecond, nil)
	secondUI.ShowThinking("continued reasoning")
	if resumed := m.currentReasoningRecord(); resumed != active || !resumed.expanded {
		t.Fatalf("unbroken continuation did not resume the expanded record: %#v", resumed)
	}
	// Prose is the aggregation boundary; the next run must not inherit the
	// consumed pre-arm or the previous record's expansion.
	secondUI.AppendAssistantText("interim answer")
	secondUI.ShowThinking("later reasoning segment")
	later := m.currentReasoningRecord()
	if later == nil || later == active || later.expanded {
		t.Fatalf("consumed pre-arm leaked into a post-prose record: %#v", later)
	}
}

func TestBusyCtrlOTogglesLatestSettledReasoningRun(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("inspect")
	tui := &gotuiTurnUI{repl: r, config: r.config}

	// Prose after each round: two settled runs, each with its own record.
	for i, thought := range []string{"first reasoning phase", "second reasoning phase"} {
		tui.ShowThinking(thought)
		call := messages.ChatMessageToolCall{ID: fmt.Sprintf("group-%d", i), Name: "inspect"}
		tui.AppendToolStart([]messages.ChatMessageToolCall{call})
		tui.AppendToolEnd(call, "ok", time.Millisecond, nil)
		tui.AppendAssistantText(fmt.Sprintf("answer %d. ", i))
	}
	ids := append([]int64(nil), m.turnReasoningIDs...)
	if len(ids) != 2 || m.currentReasoningRecord() != nil {
		t.Fatalf("fixture did not leave two settled current-turn records: ids=%v current=%#v", ids, m.currentReasoningRecord())
	}

	// Ctrl-O targets the run holding the turn's latest record; earlier
	// prose-separated runs keep their own state.
	if !m.toggleReasoning(ids[0], testThinkingWidth) {
		t.Fatal("fixture reasoning record did not expand")
	}
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
	if !m.reasoningRecords[ids[1]].expanded || !m.reasoningRecords[ids[0]].expanded {
		t.Fatalf("Ctrl-O did not expand the latest run while leaving the earlier one open: first=%v second=%v",
			m.reasoningRecords[ids[0]].expanded, m.reasoningRecords[ids[1]].expanded)
	}
	if m.turnReasoningOpen {
		t.Fatal("toggling an existing record incorrectly armed a future segment")
	}

	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
	if m.reasoningRecords[ids[1]].expanded || !m.reasoningRecords[ids[0]].expanded {
		t.Fatalf("second Ctrl-O did not collapse only the latest run: first=%v second=%v",
			m.reasoningRecords[ids[0]].expanded, m.reasoningRecords[ids[1]].expanded)
	}
	if m.turnReasoningOpen {
		t.Fatal("collapsing an existing record incorrectly became a pending pre-arm")
	}

	// A later run defaults closed even though an earlier record remains
	// open: existing-record state is independent of the one-shot pre-arm.
	tui.ShowThinking("third reasoning phase")
	later := m.currentReasoningRecord()
	if later == nil || later.expanded {
		t.Fatalf("existing record toggle leaked into a later reasoning record: %#v", later)
	}
}

func TestFailedReasoningDisclosureIsLocalAndMarkedUnsaved(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("explain")
	tui := &gotuiTurnUI{repl: r, config: r.config}
	tui.ShowThinking("reasoning from a failed turn")
	record := m.currentReasoningRecord()
	r.endTurn(errors.New("provider failed"))
	if record == nil || !record.complete || !record.unsaved || record.expanded {
		t.Fatalf("failed reasoning disclosure = %#v", record)
	}
	if collapsed := plainStyledText(m.transcript[record.transcriptIndex]); !strings.Contains(collapsed, "not saved") || strings.Contains(collapsed, "reasoning from") {
		t.Fatalf("failed collapsed disclosure = %q", collapsed)
	}
	if !m.toggleReasoning(record.id, testThinkingWidth) {
		t.Fatal("failed reasoning disclosure did not expand locally")
	}
	if expanded := plainStyledText(m.transcript[record.transcriptIndex]); !strings.Contains(expanded, "reasoning from a failed turn") {
		t.Fatalf("failed local reasoning tail = %q", expanded)
	}
}

func TestClearDuringReasoningStartsOneCleanDisclosure(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("explain")
	tui := &gotuiTurnUI{repl: r, config: r.config}
	tui.ShowThinking("reasoning before clear")
	m.reasoningPlacements = []disclosurePlacement{{recordID: m.turnReasoningID, Cols: 10}}
	m.clearDisplay()
	if len(m.reasoningRecords) != 0 || len(m.reasoningAt) != 0 || len(m.reasoningPlacements) != 0 || m.currentReasoningRecord() != nil {
		t.Fatalf("clear retained reasoning state: records=%d at=%d placements=%d current=%#v",
			len(m.reasoningRecords), len(m.reasoningAt), len(m.reasoningPlacements), m.currentReasoningRecord())
	}

	tui.ShowThinking("reasoning after clear")
	record := m.currentReasoningRecord()
	if record == nil || len(m.reasoningRecords) != 1 || len(m.reasoningOrder) != 1 || strings.Contains(string(record.tail), "before clear") {
		t.Fatalf("post-clear reasoning disclosure = %#v records=%d order=%d", record, len(m.reasoningRecords), len(m.reasoningOrder))
	}
}

func TestThinkingDisclosureIsPerSegmentAndExpansionIsPerSegment(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("first")
	tui := &gotuiTurnUI{repl: r, config: r.config}

	tui.ShowThinking("first phase reasoning")
	first := m.currentReasoningRecord()
	if first == nil || !m.toggleReasoning(first.id, testThinkingWidth) {
		t.Fatal("first disclosure did not expand")
	}
	tui.AppendAssistantText("interim prose")
	call := messages.ChatMessageToolCall{ID: "c1", Name: "bash"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	tui.AppendToolEnd(call, "ok", time.Second, nil)
	// The tool phase settles the open reasoning segment in place — expansion
	// survives until the turn settles — and a later segment opens its own
	// disclosure at its own transcript position.
	if !first.expanded || !first.complete {
		t.Fatalf("tool phase did not settle the first segment in place: %#v", first)
	}
	tui.ShowThinking("second phase reasoning")
	second := m.currentReasoningRecord()
	if second == nil || second.id == first.id || len(m.reasoningRecords) != 2 {
		t.Fatalf("second segment did not open its own disclosure: records=%d first=%#v second=%#v", len(m.reasoningRecords), first, second)
	}
	if got := string(first.tail); !strings.Contains(got, "first phase reasoning") || strings.Contains(got, "second phase") {
		t.Fatalf("first segment tail changed after settling: %q", got)
	}
	if got := string(second.tail); !strings.Contains(got, "second phase reasoning") {
		t.Fatalf("second segment tail = %q", got)
	}
	if second.transcriptIndex <= first.transcriptIndex {
		t.Fatalf("second segment did not land after the first: first=%d second=%d", first.transcriptIndex, second.transcriptIndex)
	}
	plain := plainStyledText(strings.Join(m.transcript, "\n"))
	if strings.Index(plain, "interim prose") < strings.Index(plain, "thought") {
		t.Fatalf("interim prose should follow the first reasoning block: %q", plain)
	}

	tui.AppendAssistantText("final prose")
	r.endTurn(nil)
	if first.expanded || second.expanded || !second.complete {
		t.Fatal("turn completion did not leave every segment collapsed")
	}

	m.beginTurn("second turn")
	secondUI := &gotuiTurnUI{repl: r, config: r.config}
	secondUI.ShowThinking("independent second-turn reasoning")
	third := m.currentReasoningRecord()
	if third == nil || third.id == first.id || third.id == second.id || third.expanded {
		t.Fatalf("second turn did not start with an independent collapsed record: third=%#v", third)
	}
	if !m.toggleReasoning(first.id, testThinkingWidth) || !first.expanded || third.expanded {
		t.Fatalf("opening the older turn affected the active turn: first=%#v third=%#v", first, third)
	}
	if !m.toggleLatestReasoning(testThinkingWidth) || !third.expanded || !first.expanded {
		t.Fatalf("active-turn toggle did not remain per-turn: first=%#v third=%#v", first, third)
	}
}

func TestReasoningTailLinesUseDisplayWidthForCJK(t *testing.T) {
	const width = 6
	lines := reasoningTailLines("甲乙丙丁戊己庚辛壬癸子丑寅卯辰巳午未", width, reasoningPreviewLines)
	if len(lines) != reasoningPreviewLines {
		t.Fatalf("CJK preview rows = %#v, want %d", lines, reasoningPreviewLines)
	}
	for i, line := range lines {
		if got := rw.StringWidth(line); got > width {
			t.Fatalf("CJK row %d display width = %d, want <= %d: %q", i, got, width, line)
		}
	}
	joined := strings.Join(lines, "")
	if !strings.Contains(joined, "未") || strings.Contains(joined, "甲") {
		t.Fatalf("CJK preview should retain the newest bounded rows: %#v", lines)
	}
}

func TestReasoningDisclosureStaysBoundedOnTinyTerminal(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("explain")
	tui := &gotuiTurnUI{repl: r, config: r.config}
	tui.ShowThinking("甲乙丙丁 lots of reasoning that would otherwise wrap")
	record := m.currentReasoningRecord()
	if record == nil {
		t.Fatal("reasoning record was not created")
	}
	record.expanded = true
	m.refreshReasoningRecord(record, rw.StringWidth(reasoningBlockIndent)+1)
	if got := strings.Count(m.transcript[record.transcriptIndex], "\n"); got != 0 {
		t.Fatalf("tiny terminal rendered wrapped preview rows: %q", m.transcript[record.transcriptIndex])
	}
}

func TestExpandedShortReasoningReservesTwoDetailRows(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("explain")
	tui := &gotuiTurnUI{repl: r, config: r.config}
	tui.ShowThinking("short")
	record := m.currentReasoningRecord()
	if record == nil || !m.toggleReasoning(record.id, testThinkingWidth) {
		t.Fatal("short reasoning disclosure did not expand")
	}
	shown := m.transcript[record.transcriptIndex]
	if got := strings.Count(shown, "\n"); got != 2 {
		t.Fatalf("expanded short reasoning has %d detail rows, want 2: %q", got, shown)
	}
	if plain := plainStyledText(shown); !strings.Contains(plain, "short") {
		t.Fatalf("expanded short reasoning lost its content: %q", plain)
	}
}

func TestLiveReasoningGrowthReanchorsScrolledViewport(t *testing.T) {
	const width = 24
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("explain")
	tui := &gotuiTurnUI{repl: r, config: r.config}
	tui.ShowThinking("short")
	record := m.currentReasoningRecord()
	if record == nil || !m.toggleReasoning(record.id, width) {
		t.Fatal("reasoning fixture did not expand")
	}
	for i := 0; i < 8; i++ {
		m.appendLine(fmt.Sprintf("later %d", i))
	}
	// Hold the viewport below the reasoning block: growth above the anchor
	// must shift it so the same content stays on screen.
	m.followBottom = false
	m.scrollAnchor = m.entryVisualStart(len(m.transcript)-1, width)
	before := m.scrollAnchor

	tui.ShowThinking(strings.Repeat(" additional words", 20))
	m.refreshReasoningRecords(width)
	grown := m.entryVisualLineCount(record.transcriptIndex, width)
	if grown < 2 {
		t.Fatalf("grown reasoning block rows = %d, want growth", grown)
	}
	if m.scrollAnchor <= before {
		t.Fatalf("inline growth did not re-anchor held viewport: anchor=%d, was %d", m.scrollAnchor, before)
	}
}

func TestCompletedReasoningKeepsInlineHitboxAndTrailerControl(t *testing.T) {
	const width = 50
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("explain")
	tui := &gotuiTurnUI{repl: r, config: r.config}
	tui.ShowThinking("clickable completed reasoning")
	record := m.currentReasoningRecord()
	r.endTurn(nil)
	for i := 0; i < 6; i++ {
		m.appendLine(fmt.Sprintf("later row %d", i))
	}

	// The settled reasoning block stays inline and clickable.
	rows := m.transcriptRows(width)
	visible := m.visibleReasoningPlacements(fullViewport(len(rows), width))
	if len(visible) != 1 || visible[0].recordID != record.id {
		t.Fatalf("completed reasoning lost its transcript hitbox: %#v", visible)
	}

	// The trailer also keeps its Thought control.
	rows = m.transcriptRows(width)
	placements := m.visibleTurnTrailerPlacements(fullViewport(len(rows), width))
	if len(placements) != 1 || placements[0].overlay != turnDockOverlayThought {
		t.Fatalf("completed Thought trailer placement = %#v record=%#v", placements, record)
	}
	m.turnTrailerPlacements = placements
	p := placements[0]
	if !m.toggleTurnTrailerAt(p.X+1, p.Y) || m.openTurnTrailerID == 0 {
		t.Fatal("clicking the completed Thought trailer control did not open it")
	}
}

func TestActivityTickerWhileScrolledUp(t *testing.T) {
	m := newReplModel()

	// Following the bottom: no ticker, whatever the geometry.
	if got := m.activityTicker(100, 0, 20); got != "" {
		t.Fatalf("ticker while following = %q, want empty", got)
	}

	m.followBottom = false
	got := m.activityTicker(100, 30, 20) // rows 30-49 visible, 50 below
	for _, want := range []string{"↓ 50 rows below", "End to follow"} {
		if !strings.Contains(got, want) {
			t.Fatalf("ticker %q missing %q", got, want)
		}
	}

	// Singular row, and live activity while a turn runs. The ticker shows the
	// busy label but not the elapsed timer (that lives in the status row).
	m.busy = true
	m.state = turnStateTool
	m.toolName = "bash"
	m.turnStarted = time.Now().Add(-8 * time.Second)
	got = m.activityTicker(52, 30, 21)
	for _, want := range []string{"↓ 1 row below", "running bash"} {
		if !strings.Contains(got, want) {
			t.Fatalf("busy ticker %q missing %q", got, want)
		}
	}
	if strings.Contains(got, "8.") {
		t.Fatalf("busy ticker %q should not show the elapsed timer", got)
	}

	// Scrolled but nothing below (clamp edge): no ticker.
	if got := m.activityTicker(50, 30, 20); got != "" {
		t.Fatalf("ticker with nothing below = %q, want empty", got)
	}
}

func TestDividerRowCount(t *testing.T) {
	// Normal terminal: one divider row between transcript and bottom chrome.
	if got := dividerRowCount(24, 1, 1, 0, false); got != 1 {
		t.Fatalf("divider on roomy terminal = %d, want 1", got)
	}
	// Quiet mode stays chromeless.
	if got := dividerRowCount(24, 1, 0, 0, true); got != 0 {
		t.Fatalf("divider in quiet mode = %d, want 0", got)
	}
	// Too short to spare a row: the transcript wins.
	if got := dividerRowCount(3, 1, 1, 0, false); got != 0 {
		t.Fatalf("divider on cramped terminal = %d, want 0", got)
	}
	// Boundary: exactly one transcript row left after chrome keeps the divider.
	if got := dividerRowCount(4, 1, 1, 0, false); got != 1 {
		t.Fatalf("divider at boundary = %d, want 1", got)
	}
	if got := dividerRowCount(4, 1, 1, 1, false); got != 0 {
		t.Fatalf("divider with a dock on cramped terminal = %d, want 0", got)
	}
}

func TestTurnDockRowCountPreservesCrampedTranscript(t *testing.T) {
	if got := turnDockRowCount(24, 1, 1, true); got != 1 {
		t.Fatalf("roomy terminal dock rows = %d, want 1", got)
	}
	if got := turnDockRowCount(3, 1, 1, true); got != 0 {
		t.Fatalf("cramped terminal dock rows = %d, want 0", got)
	}
	if got := turnDockRowCount(24, 1, 1, false); got != 0 {
		t.Fatalf("hidden dock rows = %d, want 0", got)
	}
}

func TestAnchorForResizedEntryStraddlePreservesRelativePosition(t *testing.T) {
	m := newReplModel()
	m.followBottom = false

	// Entry occupies rows [10, 20); anchor sits halfway inside it.
	m.scrollAnchor = 15
	m.anchorForResizedEntry(10, 10, 20)
	// Doubling the entry keeps the anchor at the same fractional depth
	// (0.5 -> row 20), not snapped to the entry top.
	if m.scrollAnchor != 20 {
		t.Fatalf("straddling anchor = %d, want 20 (relative position preserved)", m.scrollAnchor)
	}

	// Entry wholly above the anchor shifts by the delta.
	m.scrollAnchor = 50
	m.anchorForResizedEntry(10, 10, 20)
	if m.scrollAnchor != 60 {
		t.Fatalf("above-anchor shift = %d, want 60", m.scrollAnchor)
	}

	// Entry wholly above the anchor shrinks: shift up by the delta.
	m.scrollAnchor = 50
	m.anchorForResizedEntry(10, 10, 4)
	if m.scrollAnchor != 44 {
		t.Fatalf("above-anchor shrink = %d, want 44", m.scrollAnchor)
	}

	// A negative anchor is clamped to zero.
	m.scrollAnchor = 2
	m.anchorForResizedEntry(0, 10, 0)
	if m.scrollAnchor != 0 {
		t.Fatalf("collapsed entry above anchor = %d, want clamp to 0", m.scrollAnchor)
	}
}

func TestToggleToolDisclosureGroupAppliesBeforeRefresh(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("work")
	tui := &gotuiTurnUI{repl: r, config: r.config}

	// Two prose-separated tool runs -> two disclosures in one turn.
	a := messages.ChatMessageToolCall{ID: "a", Name: "alpha"}
	b := messages.ChatMessageToolCall{ID: "b", Name: "beta"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{a})
	tui.AppendToolEnd(a, "ok", time.Millisecond, nil)
	tui.AppendAssistantText("checkpoint prose. ")
	tui.AppendToolStart([]messages.ChatMessageToolCall{b})
	tui.AppendToolEnd(b, "ok", time.Millisecond, nil)

	if len(m.turnToolDisclosureIDs) != 2 {
		t.Fatalf("turn disclosures = %v, want 2", m.turnToolDisclosureIDs)
	}
	ids := append([]int64(nil), m.turnToolDisclosureIDs...)

	// Expand the group; every record must end expanded.
	if !m.toggleToolDisclosureGroup(ids) {
		t.Fatal("group expand returned false")
	}
	for _, id := range ids {
		if !m.toolDisclosures[id].expanded {
			t.Fatalf("disclosure %d not expanded after group expand", id)
		}
	}

	// A mixed group still advertises an open control, so its first click must
	// collapse every record rather than re-expand the closed member.
	m.toolDisclosures[ids[1]].expanded = false
	m.refreshToolDisclosure(m.toolDisclosures[ids[1]])
	if !m.toggleToolDisclosureGroup(ids) {
		t.Fatal("mixed group collapse returned false")
	}
	for _, id := range ids {
		if m.toolDisclosures[id].expanded {
			t.Fatalf("disclosure %d still expanded after mixed group collapse", id)
		}
	}
}
