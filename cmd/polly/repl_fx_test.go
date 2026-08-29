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

func TestBusyStatusShowsElapsedAndThinkingEstimate(t *testing.T) {
	m := newReplModel()
	m.beginTurn("explain")
	m.turnStarted = time.Now().Add(-12 * time.Second)

	m.state = turnStateWaiting
	raw, _ := m.statusActivity()
	if !strings.Contains(raw, "waiting · 12.") {
		t.Fatalf("busy status should carry live elapsed, got %q", raw)
	}

	// While reasoning streams, a ~chars/4 token estimate precedes the elapsed.
	m.state = turnStateThinking
	m.thinkingChars = 4800
	raw, _ = m.statusActivity()
	if !strings.Contains(raw, "thinking · ~1.2k tok · 12.") {
		t.Fatalf("thinking status should carry the token estimate, got %q", raw)
	}

	// Sub-token dribble shows no "~0 tok".
	m.thinkingChars = 3
	raw, _ = m.statusActivity()
	if strings.Contains(raw, "tok") {
		t.Fatalf("tiny reasoning stream should show no estimate, got %q", raw)
	}

	// The counter is per-turn: the next turn starts from zero.
	m.thinkingChars = 4800
	m.beginTurn("again")
	if m.thinkingChars != 0 {
		t.Fatalf("thinkingChars = %d after beginTurn, want 0", m.thinkingChars)
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

func TestThinkingBlockShowsBoundedTailThenCollapsesToRollup(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("explain")
	tui := &gotuiTurnUI{repl: r, config: r.config}

	// Enough reasoning to settle well past the visible cap.
	for i := 0; i < 12; i++ {
		tui.ShowThinking(fmt.Sprintf("this is reasoning sentence number %d about the parse loop. ", i))
	}
	m.refreshThinkingBlock(testThinkingWidth)

	block := m.transcript[m.thinkingIndex]
	if got := strings.Count(block, "\n") + 1; got != thinkingBlockLines {
		t.Fatalf("block should hold %d lines, got %d: %q", thinkingBlockLines, got, block)
	}
	if !strings.Contains(block, streamCursorGlyph) || !strings.Contains(block, "mod:italic") {
		t.Fatalf("block should carry the caret and italic styling, got %q", block)
	}
	// The window keeps the newest reasoning and drops the oldest.
	if !strings.Contains(block, "number 11") || strings.Contains(block, "number 0 ") {
		t.Fatalf("block should show the newest lines, got %q", block)
	}
	// No settled line may exceed the width it was wrapped to.
	for _, line := range m.thinkingWrapped {
		if len(line) > testThinkingWidth {
			t.Fatalf("line wider than the wrap width: %q", line)
		}
	}

	// The first content chunk closes the block into its permanent rollup.
	tui.AppendAssistantText("Here is the answer")
	joined := strings.Join(m.transcript, "\n")
	if !strings.Contains(joined, "thought for") || !strings.Contains(joined, "tok") {
		t.Fatalf("thinking should leave a rollup behind: %#v", m.transcript)
	}
	if strings.Contains(joined, "parse loop") {
		t.Fatalf("reasoning text should not survive in the transcript: %#v", m.transcript)
	}
	if m.thinkingIndex != -1 {
		t.Fatalf("block should be closed, index = %d", m.thinkingIndex)
	}
	if !strings.Contains(m.transcript[len(m.transcript)-1], "Here is the answer") {
		t.Fatalf("answer should follow the rollup: %#v", m.transcript)
	}

	// The full text stays available for /thinking even though the transcript
	// only ever showed a window of it.
	if len(m.thinkingLog) != 1 || !strings.Contains(m.thinkingLog[0], "number 0") {
		t.Fatalf("reasoning log should keep the whole segment: %#v", m.thinkingLog)
	}

	// The next turn starts with a clean slate but keeps the log.
	m.beginTurn("again")
	if m.thinkingIndex != -1 || m.thinkingWrapped != nil || m.thinkingPending != nil {
		t.Fatalf("thinking state should reset per turn: %d %v %q",
			m.thinkingIndex, m.thinkingWrapped, string(m.thinkingPending))
	}
	if len(m.thinkingLog) != 1 {
		t.Fatalf("log should survive the turn boundary: %#v", m.thinkingLog)
	}
}

// An agentic turn reasons repeatedly between tool calls. A rollup per segment
// would bury the prose under a wall of "thought for" lines, so the turn gets
// exactly one, accumulating every segment's time and size.
func TestTurnKeepsOneAccumulatingThinkingRollup(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("explain")
	tui := &gotuiTurnUI{repl: r, config: r.config}

	// Six reasoning segments, each broken by a tool call.
	for i := 0; i < 6; i++ {
		tui.ShowThinking(fmt.Sprintf("segment %d reasoning about the file\n", i))
		m.refreshThinkingBlock(testThinkingWidth)
		call := messages.ChatMessageToolCall{ID: fmt.Sprintf("c%d", i), Name: "bash"}
		tui.AppendToolStart([]messages.ChatMessageToolCall{call})
		tui.AppendToolEnd(call, "ok", time.Second, nil)
	}
	tui.AppendAssistantText("Fixed.")

	rollups := 0
	for _, e := range m.transcript {
		if strings.Contains(e, "thought for") {
			rollups++
		}
	}
	if rollups != 1 {
		t.Fatalf("a turn should keep exactly one reasoning rollup, got %d: %#v", rollups, m.transcript)
	}
	// It reports the whole turn's reasoning, not just the last segment's.
	if m.thinkingTurnChars < 6*len("segment 0 reasoning about the file\n") {
		t.Fatalf("rollup totals should accumulate, got %d chars", m.thinkingTurnChars)
	}
	// Every segment is still recoverable through /thinking.
	if len(m.thinkingLog) != 6 {
		t.Fatalf("log should hold every segment: %#v", m.thinkingLog)
	}
	// The rollup sits where the first segment was, above the activity.
	if m.thinkingRollupIndex < 0 || m.thinkingRollupIndex > 1 {
		t.Fatalf("rollup should stay at the first segment's position, got %d", m.thinkingRollupIndex)
	}

	// A new turn starts its own rollup rather than extending the last.
	m.beginTurn("again")
	if m.thinkingRollupIndex != -1 || m.thinkingTurnChars != 0 || m.thinkingTurnDur != 0 {
		t.Fatalf("turn totals should reset: %d %d %v",
			m.thinkingRollupIndex, m.thinkingTurnChars, m.thinkingTurnDur)
	}
}

// The block tracks the stream: the newest words show as they arrive instead of
// waiting to fill a line, and the oldest scroll off the top.
func TestThinkingBlockShowsUnsettledTailLive(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("explain")
	tui := &gotuiTurnUI{repl: r, config: r.config}

	// A partial line, far too short to settle, must still be visible.
	tui.ShowThinking("just starting")
	m.refreshThinkingBlock(testThinkingWidth)
	if !strings.Contains(m.transcript[m.thinkingIndex], "just starting") {
		t.Fatalf("partial text should render immediately: %q", m.transcript[m.thinkingIndex])
	}

	// Growing it word by word keeps the newest text on screen every frame.
	for _, word := range []string{" and", " then", " continuing", " onward"} {
		tui.ShowThinking(word)
		m.refreshThinkingBlock(testThinkingWidth)
		if !strings.Contains(plainStyledText(m.transcript[m.thinkingIndex]), strings.TrimSpace(word)) {
			t.Fatalf("newest word %q missing: %q", word, m.transcript[m.thinkingIndex])
		}
	}

	// Past the cap the block stays bounded and drops the oldest lines.
	for i := 0; i < 40; i++ {
		tui.ShowThinking(fmt.Sprintf("filler phrase %d ", i))
		m.refreshThinkingBlock(testThinkingWidth)
	}
	block := m.transcript[m.thinkingIndex]
	if got := strings.Count(block, "\n") + 1; got > thinkingBlockLines {
		t.Fatalf("block grew past %d lines: %d", thinkingBlockLines, got)
	}
	if strings.Contains(block, "just starting") {
		t.Fatalf("oldest text should have scrolled off: %q", block)
	}
	if !strings.Contains(block, "39") {
		t.Fatalf("newest text should be visible: %q", block)
	}
}

func TestThinkingLineSettlingBreaksOnWordBoundaries(t *testing.T) {
	// A hard newline settles immediately, however short the line.
	line, rest, ok := takeThinkingLine([]rune("short\nmore text"), 40)
	if !ok || line != "short" || string(rest) != "more text" {
		t.Fatalf("newline should settle a line: %q %q %v", line, string(rest), ok)
	}

	// Text that cannot fill a line is held back rather than shown partially.
	if _, _, ok := takeThinkingLine([]rune("not yet wide"), 40); ok {
		t.Fatal("short pending text should not settle")
	}

	// A long run breaks at the last space that fits, never mid-word.
	line, rest, ok = takeThinkingLine([]rune("alpha beta gamma delta epsilon zeta"), 20)
	if !ok {
		t.Fatal("long text should settle a line")
	}
	if len(line) > 20 {
		t.Fatalf("settled line too wide: %q", line)
	}
	if strings.HasSuffix(line, "gamm") || strings.HasPrefix(strings.TrimSpace(string(rest)), "amma") {
		t.Fatalf("line broke mid-word: %q | %q", line, string(rest))
	}
	if !strings.HasPrefix(line, "alpha beta") {
		t.Fatalf("line should start at the beginning: %q", line)
	}

	// A paragraph longer than the width must wrap to it rather than settling
	// as one over-wide line just because a newline eventually follows.
	long := strings.Repeat("word ", 30) + "\ntail"
	line, _, ok = takeThinkingLine([]rune(long), 24)
	if !ok || len(line) > 24 {
		t.Fatalf("a long paragraph must wrap to the width, got %d chars: %q", len(line), line)
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
