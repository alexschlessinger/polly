package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/swarm"
	tcell "github.com/gdamore/tcell/v3"
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
	if strings.Contains(strings.Join(transcriptTexts(m), "\n"), streamCursorGlyph) {
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
	if !m.visual.valid {
		t.Fatal("cache should be valid after rendering rows")
	}

	// Same pulse phase: refresh must not thrash the cache.
	m.refreshStreamCursor()
	if !m.visual.valid {
		t.Fatal("unchanged caret frame should keep the visual cache")
	}

	// Advance time across a pulse boundary: the frame flips and invalidates.
	m.turnStarted = time.Now().Add(-arrowPulsePeriod)
	m.refreshStreamCursor()
	if m.visual.valid {
		t.Fatal("pulse flip should invalidate the visual cache")
	}
}

func TestFrameTitleReflectsTurnState(t *testing.T) {
	m := newReplModel()
	m.status.contextName = "mychat"

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
	m.status.contextName = "-"
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
	r.model.notices = nil

	// A long turn cut short by a cap is exactly the walk-away case.
	r.model.beginTurn("keep going")
	r.model.turnStarted = time.Now().Add(-30 * time.Second)
	r.endTurn(llm.ErrMaxIterations)
	if len(r.model.notices) != 1 || !strings.Contains(r.model.notices[0], "incomplete after 30s") {
		t.Fatalf("incomplete turn notices = %#v", r.model.notices)
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
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}

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
	if !m.lineOpenOf(record) {
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

// The pending call shows above the prompt for every call in a batch, and
// nothing is written into the transcript to show it.
func TestApprovalShowsEachCallAboveThePrompt(t *testing.T) {
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
	before := len(m.flattenTranscript())
	composer := plainStyledText(m.inputDisplay())
	for _, want := range []string{"╭─ bash", "rm -rf ./build", "make all", "Allow (1/2) bash"} {
		if !strings.Contains(composer, want) {
			t.Fatalf("approval composer lacks %q: %q", want, composer)
		}
	}
	if len(m.flattenTranscript()) != before {
		t.Fatal("showing the call wrote into the transcript")
	}
	// Answering advances to the next call, whose arguments replace the block.
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "y"})
	composer = plainStyledText(m.inputDisplay())
	if !strings.Contains(composer, "╭─ grep") || !strings.Contains(composer, "TODO") || strings.Contains(composer, "rm -rf") || strings.Contains(composer, "allow all") {
		t.Fatalf("second call not shown on its own: %q", composer)
	}
	if len(m.flattenTranscript()) != before {
		t.Fatal("advancing the batch wrote into the transcript")
	}
}

const testThinkingWidth = 60

func TestThoughtStartsClosedWithoutLeakingReasoning(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("explain")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}

	const privateReasoning = "private chain about the parse loop"
	tui.ShowThinking(privateReasoning)
	m.refreshReasoningRecords(testThinkingWidth)

	record := m.currentReasoningRecord()
	if record == nil || m.reasoningRecords.count() != 1 || len(m.reasoningOrder) != 1 {
		t.Fatalf("reasoning record = %#v, records/order = %d/%d", record, m.reasoningRecords.count(), len(m.reasoningOrder))
	}
	if record.anyOpen() {
		t.Fatal("a turn's thought should start closed")
	}
	// The thought is one row, with no call to lead yet: no fold, no text.
	shown := activityText(t, m, testThinkingWidth, 0)
	if shown != thoughtGlyph+" Thinking…" {
		t.Fatalf("live thought row = %q", shown)
	}
	block := plainStyledText(m.transcript[record.transcriptIndex].text)
	if strings.Contains(block, privateReasoning) || strings.Contains(shown, privateReasoning) {
		t.Fatalf("a closed thought leaked reasoning: %q / %q", block, shown)
	}
	if strings.Contains(block, "tok") {
		t.Fatalf("quiet disclosure should not show an approximate token count: %q", block)
	}
	if tail := string(record.tail); tail != privateReasoning {
		t.Fatalf("retained reasoning tail = %q, want %q", tail, privateReasoning)
	}
}

func TestReasoningTailCutIsHonest(t *testing.T) {
	exact := strings.Repeat("界", reasoningTailRetainRunes)
	record := &reasoningRecord{}
	appendReasoningTail(record, exact, false)
	if record.tailCut || len(record.tail) != reasoningTailRetainRunes {
		t.Fatal("exactly fitting first chunk falsely reports truncation")
	}
	appendReasoningTail(record, exact, false)
	if !record.tailCut || len(record.tail) != reasoningTailRetainRunes {
		t.Fatal("replaced prefix did not report bounded truncation")
	}
}

// An opened thought shows every retained row of its step, not a window of
// the newest, and keeps showing the stream as it grows.
func TestOpenedThoughtShowsItsWholeTextAndFollowsTheStream(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("explain")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}

	for i := 0; i < 16; i++ {
		tui.ShowThinking(fmt.Sprintf("reasoning sentence number %d about the parse loop. ", i))
	}
	record := m.currentReasoningRecord()
	clickThought(t, m, testThinkingWidth, record.id, 0)
	if !record.partOpen(0) {
		t.Fatal("a click on the live thought's row did not open it")
	}
	entry := m.transcript[record.transcriptIndex].text
	if plain := plainStyledText(entry); !strings.Contains(plain, "number 0 ") || !strings.Contains(entry, "fg:muted") {
		t.Fatalf("entry lost retained rows or preview styling: %q", entry)
	}
	shown := activityText(t, m, testThinkingWidth, 0)
	if !strings.Contains(shown, "number 0 ") || !strings.Contains(shown, "number 15") {
		t.Fatalf("opened thought should show the whole retained text: %q", shown)
	}

	// New chunks repaint the open text immediately.
	tui.ShowThinking(" newest-live-marker")
	m.refreshReasoningRecords(testThinkingWidth)
	if shown := activityText(t, m, testThinkingWidth, 0); !strings.Contains(shown, "newest-live-marker") {
		t.Fatalf("open thought did not follow the live stream: %q", shown)
	}
	// A second click hides the text and leaves the row.
	clickThought(t, m, testThinkingWidth, record.id, 0)
	if shown := activityText(t, m, testThinkingWidth, 0); strings.Contains(shown, "number") || !strings.HasPrefix(shown, thoughtGlyph+" Thinking…") {
		t.Fatalf("closing the thought left its text: %q", shown)
	}
}

func TestThoughtTextUsesFullTranscriptWidth(t *testing.T) {
	const width = 140
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("explain")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}
	tui.ShowThinking(strings.Repeat("wide reasoning preview content ", 32))
	record := m.currentReasoningRecord()
	clickThought(t, m, width, record.id, 0)
	// The text sits under its row like a call's output: the block indent
	// and two more columns in, and nothing held back for a time column.
	want := width - rw.StringWidth(reasoningBlockIndent) - toolBodyCols
	if record.previewWidth != want {
		t.Fatalf("preview width = %d, want full available width %d", record.previewWidth, want)
	}
}

func TestSettledThinkingPreviewKeepsNarrowTerminalWidth(t *testing.T) {
	const width = 24
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.reasoningWidth = width
	m.beginTurn("explain")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}
	tui.ShowThinking(strings.Repeat("narrow reasoning preview content ", 32))
	record := m.currentReasoningRecord()
	clickThought(t, m, width, record.id, 0)
	wantWidth := width - rw.StringWidth(reasoningBlockIndent) - toolBodyCols
	if record.previewWidth != wantWidth {
		t.Fatalf("live preview width = %d, want %d", record.previewWidth, wantWidth)
	}

	// Starting a tool settles the reasoning segment from a provider callback.
	// The settled preview must not silently rerender at the 80-column fallback.
	call := messages.ChatMessageToolCall{ID: "narrow", Name: "inspect"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	m.refreshReasoningRecords(width)
	if record.previewWidth != wantWidth {
		t.Fatalf("settled preview width = %d, want retained terminal width %d", record.previewWidth, wantWidth)
	}
	for _, row := range transcriptRowsText(transcriptBlockRows(activityBlocks(m, width)[0].text, false, width)) {
		if rw.StringWidth(row) > width {
			t.Fatalf("row wider than the pane: %q", row)
		}
	}
}

func TestClearDuringReasoningStartsOneCleanDisclosure(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("explain")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}
	tui.ShowThinking("reasoning before clear")
	m.disclosurePlacements[activityThought] = []disclosurePlacement{{key: "stale", Cols: 10}}
	m.clearDisplay()
	if m.reasoningRecords.count() != 0 || m.reasoningRecords.count() != 0 || len(m.disclosurePlacements[activityThought]) != 0 || m.currentReasoningRecord() != nil {
		t.Fatalf("clear retained reasoning state: records=%d at=%d placements=%d current=%#v",
			m.reasoningRecords.count(), m.reasoningRecords.count(), len(m.disclosurePlacements[activityThought]), m.currentReasoningRecord())
	}

	tui.ShowThinking("reasoning after clear")
	record := m.currentReasoningRecord()
	if record == nil || m.reasoningRecords.count() != 1 || len(m.reasoningOrder) != 1 || strings.Contains(string(record.tail), "before clear") {
		t.Fatalf("post-clear reasoning disclosure = %#v records=%d order=%d", record, m.reasoningRecords.count(), len(m.reasoningOrder))
	}
}

// Prose closes a reasoning run: the thought after it is a record of its own,
// at its own place, and a thought opened by hand stays open through the
// turn's settlement and the next turn's start. Opening one opens no other.
func TestThoughtRecordsArePerRunAndOpenOneAtATime(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("first")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}

	tui.ShowThinking("first phase reasoning")
	first := m.currentReasoningRecord()
	tui.AppendAssistantText("interim prose")
	call := messages.ChatMessageToolCall{ID: "c1", Name: "bash"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	tui.AppendToolEnd(call, "ok", time.Second, nil)
	// Prose settled the first thought in place. Opening a settled thought
	// shows that one alone: only an opened live thought follows the turn.
	clickThought(t, m, testThinkingWidth, first.id, 0)
	if !first.partOpen(0) || !first.complete || m.followThoughts {
		t.Fatalf("a settled thought opened wrong: follow=%v record=%#v", m.followThoughts, first)
	}
	tui.ShowThinking("second phase reasoning")
	second := m.currentReasoningRecord()
	if second == nil || second.id == first.id || m.reasoningRecords.count() != 2 {
		t.Fatalf("second segment did not open its own record: records=%d first=%#v second=%#v", m.reasoningRecords.count(), first, second)
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
	plain := plainStyledText(strings.Join(transcriptTexts(m), "\n"))
	if strings.Index(plain, "interim prose") < strings.Index(plain, "thought") {
		t.Fatalf("interim prose should follow the first reasoning block: %q", plain)
	}

	tui.AppendAssistantText("final prose")
	r.endTurn(nil)
	if !first.partOpen(0) || second.anyOpen() || !second.complete {
		t.Fatal("turn completion changed which thoughts are open")
	}

	m.beginTurn("second turn")
	secondUI := &gotuiTurnUI{repl: r, model: r.model, config: r.config}
	secondUI.ShowThinking("independent second-turn reasoning")
	third := m.currentReasoningRecord()
	if third == nil || third.id == first.id || third.id == second.id || third.anyOpen() {
		t.Fatalf("second turn did not start with an independent closed thought: third=%#v", third)
	}
	// Hiding the older turn's thought touches nothing else, and no Ctrl-O
	// press opens a thought.
	clickThought(t, m, testThinkingWidth, first.id, 0)
	if first.anyOpen() || third.anyOpen() {
		t.Fatalf("closing the older thought affected the active turn: first=%#v third=%#v", first, third)
	}
	if !m.toggleAllDisclosures(testThinkingWidth) || m.depth != depthExpanded || first.anyOpen() || second.anyOpen() || third.anyOpen() {
		t.Fatalf("expanding every chunk opened a thought: first=%v second=%v third=%v", first.anyOpen(), second.anyOpen(), third.anyOpen())
	}
}

func TestReasoningTailLinesUseDisplayWidthForCJK(t *testing.T) {
	const width, limit = 6, 3
	lines := reasoningTailLines("甲乙丙丁戊己庚辛壬癸子丑寅卯辰巳午未", width, limit)
	if len(lines) != limit {
		t.Fatalf("CJK preview rows = %#v, want %d", lines, limit)
	}
	for i, line := range lines {
		if got := style.TextWidth(line); got > width {
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
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}
	tui.ShowThinking("甲乙丙丁 lots of reasoning that would otherwise wrap")
	record := m.currentReasoningRecord()
	if record == nil {
		t.Fatal("reasoning record was not created")
	}
	openThoughts(m, record.id)
	m.refreshReasoningRecords(rw.StringWidth(reasoningBlockIndent) + 1)
	if got := strings.Count(m.transcript[record.transcriptIndex].text, "\n"); got != 0 {
		t.Fatalf("tiny terminal rendered wrapped preview rows: %q", m.transcript[record.transcriptIndex].text)
	}
}

func TestExpandedShortReasoningReservesTwoDetailRows(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("explain")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}
	tui.ShowThinking("short")
	record := m.currentReasoningRecord()
	clickThought(t, m, testThinkingWidth, record.id, 0)
	shown := m.transcript[record.transcriptIndex].text
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
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}
	tui.ShowThinking("short")
	record := m.currentReasoningRecord()
	clickThought(t, m, width, record.id, 0)
	for i := 0; i < 8; i++ {
		m.appendLine(fmt.Sprintf("later %d", i))
	}
	// Hold the viewport below the reasoning block: growth above the anchor
	// must shift it so the same content stays on screen.
	rows := transcriptRowsText(m.transcriptRows(width))
	m.followBottom = false
	m.scrollAnchor = len(rows) - 1
	held := rows[m.scrollAnchor]

	tui.ShowThinking(strings.Repeat(" additional words", 20))
	m.refreshReasoningRecords(width)
	rows = transcriptRowsText(m.transcriptRows(width))
	if m.scrollAnchor >= len(rows) || rows[m.scrollAnchor] != held {
		t.Fatalf("inline growth did not re-anchor the held viewport: row %d of %d is not %q", m.scrollAnchor, len(rows), held)
	}
}

// A settled thought that led no call keeps its own row in the transcript: a
// click on it shows its text, and it is not a fold for Ctrl-O to move.
func TestCompletedThoughtKeepsItsRow(t *testing.T) {
	const width = 50
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("explain")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}
	tui.ShowThinking("clickable completed reasoning")
	record := m.currentReasoningRecord()
	r.endTurn(nil)
	for i := 0; i < 6; i++ {
		m.appendLine(fmt.Sprintf("later row %d", i))
	}

	rows := m.transcriptRows(width)
	if visible := m.visibleDisclosurePlacements(fullViewport(len(rows), width), activityAll); len(visible) != 0 {
		t.Fatalf("a thought row was laid out as a fold: %#v", visible)
	}
	link := thoughtLink(t, m, width, record.id, 0)
	if link.X != 0 || link.Cols < 3 {
		t.Fatalf("completed thought lost its row target: %#v", link)
	}
	clickThought(t, m, width, record.id, 0)
	if !record.partOpen(0) || !strings.Contains(activityText(t, m, width, 0), "clickable completed reasoning") {
		t.Fatal("clicking the completed thought's row did not show its text")
	}
	// The settled turn rides on the prompt's row, not on a trailer row.
	if trailer := m.turnTrailers.latest(); trailer != nil || m.transcript[m.newestTurnStart()].trailer == nil {
		t.Fatalf("settled trailer row = %#v, prompt trailer = %#v", trailer, m.transcript[m.newestTurnStart()].trailer)
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
	if got := dividerRowCount(24, 1, 1, false); got != 1 {
		t.Fatalf("divider on roomy terminal = %d, want 1", got)
	}
	// Quiet mode stays chromeless.
	if got := dividerRowCount(24, 1, 0, true); got != 0 {
		t.Fatalf("divider in quiet mode = %d, want 0", got)
	}
	// Too short to spare a row: the transcript wins.
	if got := dividerRowCount(3, 1, 1, false); got != 0 {
		t.Fatalf("divider on cramped terminal = %d, want 0", got)
	}
	// Boundary: exactly one transcript row left after chrome keeps the divider.
	if got := dividerRowCount(4, 1, 1, false); got != 1 {
		t.Fatalf("divider at boundary = %d, want 1", got)
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

func TestLineToggleAddressesOneLine(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("work")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}

	// Two prose-separated tool runs -> two lines in one turn.
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

	// A toggle addresses the line of the record it names and no other.
	if !m.toggleDisclosureGroup(activityTools, ids, 0) {
		t.Fatal("line toggle returned false")
	}
	if first, second := m.toolDisclosures.get(ids[0]), m.toolDisclosures.get(ids[1]); !m.lineOpenOf(first) || m.lineOpenOf(second) {
		t.Fatalf("toggle opened the wrong lines: first=%v second=%v", m.lineOpenOf(first), m.lineOpenOf(second))
	}
	if !m.toggleToolDisclosure(ids[1]) || !m.lineOpenOf(m.toolDisclosures.get(ids[1])) || !m.lineOpenOf(m.toolDisclosures.get(ids[0])) {
		t.Fatal("toggling the second line changed the first")
	}
	if !m.toggleToolDisclosure(ids[0]) || m.lineOpenOf(m.toolDisclosures.get(ids[0])) || !m.lineOpenOf(m.toolDisclosures.get(ids[1])) {
		t.Fatal("folding the first line changed the second")
	}
}

type fakeFXPrinter struct {
	writes []string
}

func (p *fakeFXPrinter) Print(s string) {
	p.writes = append(p.writes, s)
}

type fakePrintableScreen struct {
	tcell.Screen
	writes []string
}

func (s *fakePrintableScreen) Print(str string) {
	s.writes = append(s.writes, str)
}

func TestNewTerminalFXUnwrapsScreenDecorators(t *testing.T) {
	base := &fakePrintableScreen{}
	var painter framePainter
	wrapped := themedScreen{painter.track(base)}
	p := screenPrinter(wrapped)
	if p == nil {
		t.Fatal("screenPrinter failed to unwrap themedScreen/trackedPaintScreen")
	}
	p.Print("\x1b]7501;state=idle:app=polly\x1b\\")
	if len(base.writes) != 1 || base.writes[0] != "\x1b]7501;state=idle:app=polly\x1b\\" {
		t.Fatalf("unexpected writes = %q", base.writes)
	}
}

func TestSupportsProgramStatusOSC(t *testing.T) {
	env := func(vals map[string]string) func(string) string {
		return func(k string) string { return vals[k] }
	}
	if !supportsProgramStatusOSC(env(map[string]string{"REX_SESSION": "session:1"})) {
		t.Fatal("REX_SESSION should enable OSC 7501")
	}
	if !supportsProgramStatusOSC(env(map[string]string{"REX_BLOCK": "block:1"})) {
		t.Fatal("REX_BLOCK should enable OSC 7501")
	}
	if supportsProgramStatusOSC(env(map[string]string{"TERM_PROGRAM": "ghostty"})) {
		t.Fatal("non-Rex terminals should not emit OSC 7501 by default")
	}
}

func TestFormatProgramStatusOSC(t *testing.T) {
	seq := formatProgramStatusOSC(programStatusRecord{
		State: "blocked",
		Kind:  "permission",
		App:   "polly",
		Msg:   "Approve bash\n\x1b]7501;evil\x07",
	})
	if !strings.HasPrefix(seq, "\x1b]7501;") || !strings.HasSuffix(seq, "\x1b\\") {
		t.Fatalf("malformed OSC 7501 wrapper: %q", seq)
	}
	body := strings.TrimSuffix(strings.TrimPrefix(seq, "\x1b]7501;"), "\x1b\\")
	if !strings.Contains(body, "state=blocked:kind=permission:app=polly:msg=") {
		t.Fatalf("unexpected OSC 7501 body: %q", body)
	}
	_, b64, ok := strings.Cut(body, "msg=")
	if !ok {
		t.Fatalf("missing msg in %q", body)
	}
	decoded, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("decode msg base64: %v", err)
	}
	if strings.ContainsAny(string(decoded), "\x1b\x07\n\r") {
		t.Fatalf("decoded msg retained control characters: %q", string(decoded))
	}

	// Non-blocked states must omit kind.
	working := formatProgramStatusOSC(programStatusRecord{State: "working", Kind: "permission", App: "polly"})
	if strings.Contains(working, "kind=") {
		t.Fatalf("working record must not emit kind: %q", working)
	}
}

func TestFrameProgramStatusAndTerminalFXLifecycle(t *testing.T) {
	m := newReplModel()
	if snap := m.frameProgramStatus(); snap.Root.State != "idle" || snap.Root.App != "polly" {
		t.Fatalf("idle snapshot = %+v", snap)
	}

	m.beginTurn("check")
	m.state = turnStateThinking
	if snap := m.frameProgramStatus(); snap.Root.State != "working" || snap.Root.Msg != "thinking" {
		t.Fatalf("working snapshot = %+v", snap)
	}

	m.approval = &approvalState{calls: []messages.ChatMessageToolCall{{Name: "bash"}}}
	if snap := m.frameProgramStatus(); snap.Root.State != "blocked" || snap.Root.Kind != "permission" {
		t.Fatalf("approval snapshot = %+v", snap)
	}
	m.approval = nil

	m.modal = &replModal{ask: &askDialog{req: askRequest{Header: "Pick a target"}}}
	if snap := m.frameProgramStatus(); snap.Root.State != "blocked" || snap.Root.Kind != "question" || snap.Root.Msg != "Pick a target" {
		t.Fatalf("ask snapshot = %+v", snap)
	}
	m.modal = nil

	m.busy = false
	m.lastOutcome = turnOutcomeDone
	if snap := m.frameProgramStatus(); snap.Root.State != "done" {
		t.Fatalf("done snapshot = %+v", snap)
	}
	m.lastOutcome = turnOutcomeFailed
	if snap := m.frameProgramStatus(); snap.Root.State != "error" {
		t.Fatalf("error snapshot = %+v", snap)
	}

	// Add an active worker child disclosure and verify dedupe, clear, and shutdown.
	rec := &toolDisclosureRecord{
		rows: []toolDisclosureRow{{
			agent: &agentActivity{
				viewID: "audit-1",
				label:  "Audit Worker",
				state:  swarm.AgentPresentation{Lifecycle: swarm.LifecycleActive, Busy: true},
			},
		}},
	}
	m.toolDisclosures.add(rec, 0)

	snap := m.frameProgramStatus()
	if len(snap.Children) != 1 || snap.Children[0].ID != "worker/audit-1" || snap.Children[0].State != "working" {
		t.Fatalf("worker child snapshot = %+v", snap.Children)
	}

	printer := &fakeFXPrinter{}
	fx := &terminalFX{printer: printer, programStatusOK: true}
	fx.setProgramStatus(snap)
	if len(printer.writes) != 2 {
		t.Fatalf("expected 2 writes (root + worker), got %d: %q", len(printer.writes), printer.writes)
	}
	// Repeating identical state must not emit redundant escapes.
	fx.setProgramStatus(snap)
	if len(printer.writes) != 2 {
		t.Fatalf("duplicate snapshot emitted extra writes: %q", printer.writes)
	}

	// Settling the worker emits an explicit clear for its child ID.
	rec.rows[0].agent.state = swarm.AgentPresentation{Lifecycle: swarm.LifecycleIdle, TaskStatus: "done"}
	fx.setProgramStatus(m.frameProgramStatus())
	if len(printer.writes) != 3 || printer.writes[2] != "\x1b]7501;state=clear:id=worker/audit-1\x1b\\" {
		t.Fatalf("expected child clear sequence, got %q", printer.writes)
	}

	// Shutdown clears all OSC 7501 state.
	fx.shutdown()
	if len(printer.writes) != 4 || printer.writes[3] != "\x1b]7501;state=clear\x1b\\" {
		t.Fatalf("expected shutdown clear sequence, got %q", printer.writes)
	}
}
