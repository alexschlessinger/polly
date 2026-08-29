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

	// Failure lines carry the exit code and still no output text.
	probe := exec.Command("bash", "-c", "exit 7").Run()
	wrapped := fmt.Errorf("command failed: %w (output: secret stack trace)", probe)
	tui.AppendToolEnd(calls[1], "secret stack trace", time.Second, wrapped)
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
	if !strings.Contains(block, "▸ Thinking") {
		t.Fatalf("collapsed disclosure = %q, want a Thinking label", block)
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
	if !strings.Contains(block, "▾ Thinking") || !strings.Contains(m.transcript[record.transcriptIndex], "mod:italic") {
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
	if active == nil || m.turnDock.overlay != turnDockOverlayThought {
		t.Fatalf("first reasoning chunk did not honor the pre-armed dock: record=%#v dock=%#v", active, m.turnDock)
	}
	if shown := strings.Join(rowsText(m.turnDockOverlayRows(testThinkingWidth)), "\n"); !strings.Contains(shown, "new live reasoning") {
		t.Fatalf("pre-armed drawer did not show the live tail: %q", shown)
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

func TestThinkingDisclosureAggregatesToolPhasesAndExpansionIsPerTurn(t *testing.T) {
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
	if !first.expanded {
		t.Fatal("assistant/tool phase collapsed an explicitly opened disclosure")
	}
	tui.ShowThinking("second phase reasoning")
	m.refreshReasoningRecord(first, testThinkingWidth)
	if got := string(first.tail); !strings.Contains(got, "first phase reasoning\nsecond phase reasoning") {
		t.Fatalf("tool-separated reasoning was not aggregated with a boundary: %q", got)
	}
	if len(m.reasoningRecords) != 1 || m.currentReasoningRecord() != first || !first.expanded {
		t.Fatalf("tool phase created or reset the per-turn disclosure: records=%d current=%#v first=%#v", len(m.reasoningRecords), m.currentReasoningRecord(), first)
	}

	tui.AppendAssistantText("final prose")
	r.endTurn(nil)
	if first.expanded {
		t.Fatal("first turn did not auto-collapse on completion")
	}

	m.beginTurn("second")
	secondUI := &gotuiTurnUI{repl: r, config: r.config}
	secondUI.ShowThinking("independent second-turn reasoning")
	second := m.currentReasoningRecord()
	if second == nil || second.id == first.id || second.expanded {
		t.Fatalf("second turn did not start with an independent collapsed record: first=%#v second=%#v", first, second)
	}
	if !m.toggleReasoning(first.id, testThinkingWidth) || !first.expanded || second.expanded {
		t.Fatalf("opening the older turn affected the active turn: first=%#v second=%#v", first, second)
	}
	if !m.toggleLatestReasoning(testThinkingWidth) || !second.expanded || !first.expanded {
		t.Fatalf("active-turn toggle did not remain per-turn: first=%#v second=%#v", first, second)
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

func TestLiveReasoningDockGrowthPreservesScrolledVisualAnchor(t *testing.T) {
	const width = 24
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("explain")
	tui := &gotuiTurnUI{repl: r, config: r.config}
	tui.ShowThinking("short")
	if m.currentReasoningRecord() == nil || !m.toggleTurnDockOverlay(turnDockOverlayThought) {
		t.Fatal("reasoning dock fixture did not expand")
	}
	for i := 0; i < 8; i++ {
		m.appendLine(fmt.Sprintf("later %d", i))
	}
	m.followBottom = false
	m.scrollAnchor = 2
	before := m.scrollAnchor

	tui.ShowThinking(strings.Repeat(" additional words", 20))
	m.refreshReasoningRecords(width)
	if got := len(m.turnDockOverlayRows(width)); got < 2 || got > reasoningPreviewLines {
		t.Fatalf("grown reasoning drawer rows = %d", got)
	}
	if m.scrollAnchor != before {
		t.Fatalf("live drawer growth moved held viewport: anchor=%d, want %d", m.scrollAnchor, before)
	}
}

func TestCompletedReasoningUsesDockHitboxNotTranscriptHitbox(t *testing.T) {
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

	rows := m.transcriptRows(width)
	visible := m.visibleReasoningPlacements(len(rows), 3, 0, 0, width, false, 0)
	if len(visible) != 0 {
		t.Fatalf("completed reasoning retained a transcript hitbox: %#v", visible)
	}

	rows = m.transcriptRows(width)
	placements := m.visibleTurnTrailerPlacements(len(rows), len(rows), 0, 0, width, false, 0)
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

	// Singular row, and live activity while a turn runs.
	m.busy = true
	m.state = turnStateTool
	m.toolName = "bash"
	m.turnStarted = time.Now().Add(-8 * time.Second)
	got = m.activityTicker(52, 30, 21)
	for _, want := range []string{"↓ 1 row below", "running bash", "8."} {
		if !strings.Contains(got, want) {
			t.Fatalf("busy ticker %q missing %q", got, want)
		}
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
