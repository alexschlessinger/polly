package main

import (
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
	m.depth = depthCollapsed // the checks below open a folded line by hand
	m.busy = true
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}

	calls := []messages.ChatMessageToolCall{{ID: "a", Name: "grep"}, {ID: "b", Name: "bash"}}
	tui.AppendToolStart(calls)

	// Success lines name the call and its duration, never an output size.
	tui.AppendToolEnd(calls[0], "hit one\nhit two\nhit three", time.Second, nil)
	record := m.currentToolDisclosure()
	if record == nil || !m.toggleToolDisclosure(record.id) {
		t.Fatalf("annotated tool disclosure did not expand: %#v", record)
	}
	if got := strings.Join(m.flattenTranscript(), "\n"); !strings.Contains(got, "grep · 1.0s") || strings.Contains(got, "lines") {
		t.Fatalf("success line should carry no line count, got %q", got)
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

func TestThinkingDisclosureStartsCollapsedWithoutLeakingReasoning(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.depth = depthCollapsed // the checks below open a folded line by hand
	m.beginTurn("explain")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}

	const privateReasoning = "private chain about the parse loop"
	tui.ShowThinking(privateReasoning)
	m.refreshReasoningRecords(testThinkingWidth)

	record := m.currentReasoningRecord()
	if record == nil || m.reasoningRecords.count() != 1 || len(m.reasoningOrder) != 1 {
		t.Fatalf("reasoning record = %#v, records/order = %d/%d", record, m.reasoningRecords.count(), len(m.reasoningOrder))
	}
	if record.expanded {
		t.Fatal("a turn's reasoning disclosure should start collapsed")
	}
	block := plainStyledText(m.transcript[record.transcriptIndex].text)
	if !strings.Contains(block, "▸ thought") {
		t.Fatalf("collapsed disclosure = %q, want a thought label", block)
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

func TestThinkingDisclosureExpansionShowsBoundedLiveTail(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.depth = depthCollapsed // the checks below open a folded line by hand
	m.beginTurn("explain")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}

	for i := 0; i < 16; i++ {
		tui.ShowThinking(fmt.Sprintf("reasoning sentence number %d about the parse loop. ", i))
	}
	record := m.currentReasoningRecord()
	if record == nil || !m.toggleReasoning(record.id, testThinkingWidth) {
		t.Fatal("active reasoning disclosure did not expand")
	}

	m.refreshReasoningRecords(testThinkingWidth)
	// The entry keeps every retained row; the abridged line shows the
	// thought as one run, its newest activityRunRows rows of text, the top
	// one marked as cut.
	entry := m.transcript[record.transcriptIndex].text
	if plain := plainStyledText(entry); !strings.Contains(plain, "number 0 ") || !strings.Contains(plain, "▾ thought") || !strings.Contains(entry, "fg:muted") {
		t.Fatalf("entry lost retained rows, its open label or preview styling: %q", entry)
	}
	window := func(stage string) string {
		t.Helper()
		block := plainStyledText(activityBlocks(m, testThinkingWidth)[0].activityReasoningDetail)
		lines := strings.Split(block, "\n")
		if got := len(lines); got != activityRunRows || strings.Contains(block, "earlier lines") {
			t.Fatalf("%s: window rows = %d, want %d: %q", stage, got, activityRunRows, block)
		}
		if !strings.HasPrefix(lines[0], reasoningCutIndent) {
			t.Fatalf("%s: the cut thought wants its top row marked: %q", stage, block)
		}
		if !strings.Contains(block, "number 15") || strings.Contains(block, "number 0 ") {
			t.Fatalf("%s: window should show only the newest tail: %q", stage, block)
		}
		return block
	}
	window("expanded")

	// New chunks repaint the open tail immediately without growing the window.
	tui.ShowThinking(" newest-live-marker")
	m.refreshReasoningRecords(testThinkingWidth)
	if block := window("live"); !strings.Contains(block, "newest-live-marker") {
		t.Fatalf("open disclosure did not follow the live stream: %q", block)
	}
}

func TestThinkingDisclosurePreviewUsesFullTranscriptWidth(t *testing.T) {
	const width = 140
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.depth = depthCollapsed // the checks below open a folded line by hand
	m.beginTurn("explain")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}
	tui.ShowThinking(strings.Repeat("wide reasoning preview content ", 32))
	record := m.currentReasoningRecord()
	if record == nil || !m.toggleReasoning(record.id, width) {
		t.Fatal("reasoning disclosure did not expand")
	}

	m.refreshReasoningRecords(width)
	want := width - rw.StringWidth(reasoningBlockIndent) - stepColumn(width)
	if record.previewWidth != want {
		t.Fatalf("preview width = %d, want full available width %d", record.previewWidth, want)
	}
	if got := strings.Count(plainStyledText(activityBlocks(m, width)[0].activityReasoningDetail), "\n") + 1; got > activityTailRows {
		t.Fatalf("expanded window rows = %d, want at most the tail's %d", got, activityTailRows)
	}
}

func TestSettledThinkingPreviewKeepsNarrowTerminalWidth(t *testing.T) {
	const width = 24
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.depth = depthCollapsed // the checks below open a folded line by hand
	m.reasoningWidth = width
	m.beginTurn("explain")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}
	tui.ShowThinking(strings.Repeat("narrow reasoning preview content ", 32))
	record := m.currentReasoningRecord()
	if record == nil || !m.toggleReasoning(record.id, width) {
		t.Fatal("reasoning disclosure did not expand")
	}

	m.refreshReasoningRecords(width)
	wantWidth := width - rw.StringWidth(reasoningBlockIndent)
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
	rows := transcriptBlockRows(activityBlocks(m, width)[0].text, false, width)
	// Rows laid out at the fallback width would wrap here and overrun the
	// tail's rows.
	if got := len(rows) - 1; got > activityTailRows {
		t.Fatalf("settled window uses %d physical detail rows, want at most the tail's %d", got, activityTailRows)
	}
}

func TestSettledThoughtStaysOpenAndIdleCtrlOSetsDepth(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("explain")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}
	tui.ShowThinking(strings.Repeat("bounded reasoning tail ", 16))
	record := m.currentReasoningRecord()
	if record == nil || !record.expanded {
		t.Fatal("the newest turn's thought did not open on its own")
	}

	r.endTurn(nil)
	if !record.expanded || !record.complete || record.active || m.currentReasoningRecord() != nil {
		t.Fatalf("completed reasoning record did not settle where it stood: %#v", record)
	}

	// With no active turn, Ctrl-O sets the view's depth: the open thought
	// stays open and shows the whole of itself, then nothing at all, then
	// its newest rows again.
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
	if m.depth != depthFull || !record.expanded || !record.segments[0].full {
		t.Fatalf("idle Ctrl-O did not move the view to full: depth=%v expanded=%v segments=%+v", m.depth, record.expanded, record.segments)
	}
	m.refreshReasoningRecords(testThinkingWidth)
	if opened := plainStyledText(m.transcript[record.transcriptIndex].text); !strings.Contains(opened, "bounded reasoning") {
		t.Fatalf("opened completed thought = %q", opened)
	}
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
	if m.depth != depthCalls || !record.expanded || record.segments[0].full {
		t.Fatalf("second idle Ctrl-O did not move the view to calls: depth=%v expanded=%v segments=%+v", m.depth, record.expanded, record.segments)
	}
	if body := plainStyledText(activityBlocks(m, testThinkingWidth)[0].activityToolDetail); strings.Contains(body, "bounded reasoning") {
		t.Fatalf("calls depth showed the thought: %q", body)
	}
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
	if m.depth != depthCollapsed || record.expanded {
		t.Fatalf("third idle Ctrl-O did not collapse: depth=%v expanded=%v", m.depth, record.expanded)
	}
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
	if m.depth != depthAbridged || !record.expanded {
		t.Fatalf("fourth idle Ctrl-O did not abridge and reopen: depth=%v expanded=%v", m.depth, record.expanded)
	}
}

func TestCtrlOSetsDepthAcrossTurns(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("first")
	firstUI := &gotuiTurnUI{repl: r, model: r.model, config: r.config}
	firstUI.ShowThinking("older completed reasoning")
	older := m.currentReasoningRecord()
	olderCall := messages.ChatMessageToolCall{ID: "older", Name: "inspect"}
	firstUI.AppendToolStart([]messages.ChatMessageToolCall{olderCall})
	firstUI.AppendToolEnd(olderCall, "ok", time.Millisecond, nil)
	olderTool := m.currentToolDisclosure()
	r.endTurn(nil)

	m.beginTurn("second")
	secondUI := &gotuiTurnUI{repl: r, model: r.model, config: r.config}
	secondUI.ShowThinking("new live reasoning")
	active := m.currentReasoningRecord()
	liveCall := messages.ChatMessageToolCall{ID: "live", Name: "bash"}
	secondUI.AppendToolStart([]messages.ChatMessageToolCall{liveCall})
	secondUI.AppendToolEnd(liveCall, "ok", time.Millisecond, nil)
	liveTool := m.currentToolDisclosure()
	if older == nil || olderTool == nil || active == nil || liveTool == nil {
		t.Fatalf("fixture did not leave a thought and a tool block in each turn: older=%t olderTool=%t active=%t liveTool=%t",
			older == nil, olderTool == nil, active == nil, liveTool == nil)
	}
	// The new turn left the older line open and opened its own.
	if !older.expanded || !olderTool.expanded || !active.expanded || !liveTool.expanded {
		t.Fatalf("turn start did not leave both lines open: older=%v/%v active=%v/%v", older.expanded, olderTool.expanded, active.expanded, liveTool.expanded)
	}

	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
	if m.depth != depthFull || !older.expanded || !olderTool.expanded || !active.expanded || !liveTool.expanded {
		t.Fatalf("Ctrl-O changed which lines are open: depth=%v older=%v/%v active=%v/%v", m.depth, older.expanded, olderTool.expanded, active.expanded, liveTool.expanded)
	}
	for _, record := range []*toolDisclosureRecord{olderTool, liveTool} {
		for _, row := range record.rows {
			if row.outputExpanded {
				t.Fatalf("the full depth opened output for %q", row.callID)
			}
		}
	}
	m.refreshReasoningRecords(testThinkingWidth)
	if shown := plainStyledText(m.transcript[active.transcriptIndex].text); !strings.Contains(shown, "new live reasoning") {
		t.Fatalf("expanded inline block did not show the live tail: %q", shown)
	}

	// A tool phase pauses rather than breaks the run: the continuation
	// resumes the same record, and the call joins the run's open tool
	// block, its output closed like every other.
	between := messages.ChatMessageToolCall{ID: "between", Name: "inspect"}
	secondUI.AppendToolStart([]messages.ChatMessageToolCall{between})
	secondUI.AppendToolEnd(between, "ok", time.Millisecond, nil)
	secondUI.ShowThinking("continued reasoning")
	if resumed := m.currentReasoningRecord(); resumed != active || !resumed.expanded {
		t.Fatalf("unbroken continuation did not resume the open record: %#v", resumed)
	}
	if joined := m.currentToolDisclosure(); joined != liveTool || !joined.expanded || len(joined.rows) != 2 || joined.rows[1].outputExpanded {
		t.Fatalf("tool phase did not join the run's open tool block with its output closed: %#v", joined)
	}

	// The calls depth leaves the lines as they are too.
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
	if m.depth != depthCalls || !older.expanded || !olderTool.expanded || !active.expanded || !liveTool.expanded {
		t.Fatalf("second Ctrl-O changed which lines are open: depth=%v older=%v/%v active=%v/%v", m.depth, older.expanded, olderTool.expanded, active.expanded, liveTool.expanded)
	}
	// A segment that arrives later opens as the newest turn's.
	secondUI.AppendAssistantText("interim answer")
	secondUI.ShowThinking("later reasoning segment")
	if later := m.currentReasoningRecord(); later == nil || !later.expanded {
		t.Fatalf("later reasoning segment of the newest turn did not open: %#v", later)
	}
}

func TestCtrlOLeavesHandOpenedAndFoldedRunsAlone(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("explain")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}

	// Prose after each round: two settled runs, each with its own record,
	// both open as the newest turn's.
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

	// A hand fold survives the depth change.
	if !m.toggleReasoning(ids[0], testThinkingWidth) || m.reasoningRecords.get(ids[0]).expanded {
		t.Fatal("fixture reasoning record did not fold")
	}
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
	if m.depth != depthFull || m.reasoningRecords.get(ids[0]).expanded || !m.reasoningRecords.get(ids[1]).expanded {
		t.Fatalf("Ctrl-O moved a hand-folded run: depth=%v first=%v second=%v",
			m.depth, m.reasoningRecords.get(ids[0]).expanded, m.reasoningRecords.get(ids[1]).expanded)
	}
	// So does a hand reopen.
	if !m.toggleReasoning(ids[0], testThinkingWidth) || !m.reasoningRecords.get(ids[0]).expanded {
		t.Fatal("fixture reasoning record did not reopen")
	}
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
	if m.depth != depthCalls || !m.reasoningRecords.get(ids[0]).expanded || !m.reasoningRecords.get(ids[1]).expanded {
		t.Fatalf("second Ctrl-O moved a hand-opened run: depth=%v first=%v second=%v",
			m.depth, m.reasoningRecords.get(ids[0]).expanded, m.reasoningRecords.get(ids[1]).expanded)
	}
	// A run that arrives later opens as the newest turn's.
	tui.ShowThinking("third reasoning phase")
	if later := m.currentReasoningRecord(); later == nil || !later.expanded {
		t.Fatalf("later reasoning record of the newest turn did not open: %#v", later)
	}
}

// The depth is sticky: it applies to rows that arrive later, in later runs,
// batches and turns, while which lines are open follows the turns.
func TestDepthHoldsForLaterBlocksAndTurns(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("first")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}
	tui.ShowThinking("first run reasoning")
	first := m.currentReasoningRecord()
	call := messages.ChatMessageToolCall{ID: "c1", Name: "bash"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	tui.AppendToolEnd(call, "ok", time.Second, nil)
	firstTool := m.currentToolDisclosure()

	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
	if m.depth != depthFull || !first.expanded || !firstTool.expanded || firstTool.rows[0].outputExpanded {
		t.Fatalf("Ctrl-O did not move the present blocks to full with outputs closed: thought=%v tools=%v output=%v", first.expanded, firstTool.expanded, firstTool.rows[0].outputExpanded)
	}

	// Later blocks in the same turn open: a new run after prose, and a new
	// batch after the prose settled the first one.
	tui.AppendAssistantText("interim answer")
	tui.ShowThinking("later run reasoning")
	later := m.currentReasoningRecord()
	if later == nil || later.id == first.id || !later.expanded {
		t.Fatalf("later reasoning run did not open: %#v", later)
	}
	laterCall := messages.ChatMessageToolCall{ID: "c2", Name: "inspect"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{laterCall})
	tui.AppendToolEnd(laterCall, "ok", time.Second, nil)
	laterTool := m.currentToolDisclosure()
	if laterTool == nil || laterTool.id == firstTool.id || !laterTool.expanded || laterTool.rows[0].outputExpanded {
		t.Fatalf("later tool batch did not open with its output closed: %#v", laterTool)
	}

	// Settlement keeps the turn's blocks as they are.
	r.endTurn(nil)
	if !first.expanded || !later.expanded || !firstTool.expanded || !laterTool.expanded {
		t.Fatalf("turn settlement folded a block: first=%v later=%v tools=%v/%v",
			first.expanded, later.expanded, firstTool.expanded, laterTool.expanded)
	}

	// The next turn leaves the first turn's lines open and opens its own,
	// at the same depth.
	m.beginTurn("second")
	secondUI := &gotuiTurnUI{repl: r, model: r.model, config: r.config}
	secondUI.ShowThinking("second turn reasoning")
	secondTurn := m.currentReasoningRecord()
	if secondTurn == nil || !secondTurn.expanded || !first.expanded || !later.expanded || !firstTool.expanded || !laterTool.expanded {
		t.Fatalf("second turn did not open its own line beside the first: second=%v first=%v/%v tools=%v/%v", secondTurn != nil && secondTurn.expanded, first.expanded, later.expanded, firstTool.expanded, laterTool.expanded)
	}
	secondCall := messages.ChatMessageToolCall{ID: "c3", Name: "inspect"}
	secondUI.AppendToolStart([]messages.ChatMessageToolCall{secondCall})
	secondUI.AppendToolEnd(secondCall, "ok", time.Second, nil)
	secondTool := m.currentToolDisclosure()
	if secondTool == nil || !secondTool.expanded || len(secondTool.rows) != 1 || secondTool.rows[0].outputExpanded {
		t.Fatalf("second-turn tool batch did not open with its output closed: %#v", secondTool)
	}

	// The next depth leaves the lines alone too.
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
	if m.depth != depthCalls || !first.expanded || !later.expanded || !secondTurn.expanded {
		t.Fatalf("second Ctrl-O moved a line: depth=%v first=%v later=%v second=%v", m.depth, first.expanded, later.expanded, secondTurn.expanded)
	}
	if !firstTool.expanded || !laterTool.expanded || !secondTool.expanded {
		t.Fatalf("second Ctrl-O tool blocks: first=%v later=%v second=%v", firstTool.expanded, laterTool.expanded, secondTool.expanded)
	}
}

func TestClearDuringReasoningStartsOneCleanDisclosure(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("explain")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}
	tui.ShowThinking("reasoning before clear")
	m.disclosurePlacements[activityThought] = []disclosurePlacement{{recordID: m.turnReasoningID, Cols: 10}}
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

func TestThinkingDisclosureIsPerSegmentAndExpansionIsPerSegment(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.depth = depthCollapsed // the checks below open a folded line by hand
	m.beginTurn("first")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}

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
	if second == nil || second.id == first.id || m.reasoningRecords.count() != 2 {
		t.Fatalf("second segment did not open its own disclosure: records=%d first=%#v second=%#v", m.reasoningRecords.count(), first, second)
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
	if !first.expanded || second.expanded || !second.complete {
		t.Fatal("turn completion changed the segments' state")
	}

	m.beginTurn("second turn")
	secondUI := &gotuiTurnUI{repl: r, model: r.model, config: r.config}
	secondUI.ShowThinking("independent second-turn reasoning")
	third := m.currentReasoningRecord()
	if third == nil || third.id == first.id || third.id == second.id || third.expanded {
		t.Fatalf("second turn did not start with an independent collapsed record: third=%#v", third)
	}
	if !m.toggleReasoning(first.id, testThinkingWidth) || first.expanded || third.expanded {
		t.Fatalf("folding the older turn affected the active turn: first=%#v third=%#v", first, third)
	}
	// Leaving the collapsed depth opens every line.
	if !m.toggleAllDisclosures(testThinkingWidth) || m.depth != depthAbridged || !first.expanded || !second.expanded || !third.expanded {
		t.Fatalf("leaving collapsed did not open every line: first=%v second=%v third=%v",
			first.expanded, second.expanded, third.expanded)
	}
}

func TestReasoningTailLinesUseDisplayWidthForCJK(t *testing.T) {
	const width = 6
	lines := reasoningTailLines("甲乙丙丁戊己庚辛壬癸子丑寅卯辰巳午未", width, reasoningPreviewLines)
	if len(lines) != reasoningPreviewLines {
		t.Fatalf("CJK preview rows = %#v, want %d", lines, reasoningPreviewLines)
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
	record.expanded = true
	m.refreshReasoningRecords(rw.StringWidth(reasoningBlockIndent) + 1)
	if got := strings.Count(m.transcript[record.transcriptIndex].text, "\n"); got != 0 {
		t.Fatalf("tiny terminal rendered wrapped preview rows: %q", m.transcript[record.transcriptIndex].text)
	}
}

func TestExpandedShortReasoningReservesTwoDetailRows(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.depth = depthCollapsed // the checks below open a folded line by hand
	m.beginTurn("explain")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}
	tui.ShowThinking("short")
	record := m.currentReasoningRecord()
	if record == nil || !m.toggleReasoning(record.id, testThinkingWidth) {
		t.Fatal("short reasoning disclosure did not expand")
	}
	m.refreshReasoningRecords(testThinkingWidth)
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
	m.depth = depthCollapsed // the checks below open a folded line by hand
	m.beginTurn("explain")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}
	tui.ShowThinking("short")
	record := m.currentReasoningRecord()
	if record == nil || !m.toggleReasoning(record.id, width) {
		t.Fatal("reasoning fixture did not expand")
	}
	m.refreshReasoningRecords(width)
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

func TestCompletedReasoningKeepsInlineHitbox(t *testing.T) {
	const width = 50
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.depth = depthCollapsed // the checks below open a folded line by hand
	m.beginTurn("explain")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}
	tui.ShowThinking("clickable completed reasoning")
	record := m.currentReasoningRecord()
	r.endTurn(nil)
	for i := 0; i < 6; i++ {
		m.appendLine(fmt.Sprintf("later row %d", i))
	}

	// The settled reasoning block stays inline and clickable.
	rows := m.transcriptRows(width)
	visible := m.visibleDisclosurePlacements(fullViewport(len(rows), width), activityAll)
	if len(visible) != 1 || visible[0].recordID != -record.id {
		t.Fatalf("completed reasoning lost its transcript hitbox: %#v", visible)
	}

	// Clicking the line opens the thought; the settled turn rides on the
	// line's right column, not on a trailer row.
	m.disclosurePlacements[activityAll] = visible
	if !m.toggleDisclosureAt(visible[0].X, visible[0].Y, width) || !record.expanded {
		t.Fatal("clicking the completed thought control did not open it")
	}
	if trailer := m.turnTrailers.latest(); trailer != nil || record.trailer == nil {
		t.Fatalf("settled trailer = %#v, record trailer = %#v", trailer, record.trailer)
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

func TestLineToggleAddressesOneLine(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.depth = depthCollapsed // the checks below open a folded line by hand
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
	if first, second := m.toolDisclosures.get(ids[0]), m.toolDisclosures.get(ids[1]); !first.expanded || second.expanded {
		t.Fatalf("toggle opened the wrong lines: first=%v second=%v", first.expanded, second.expanded)
	}
	if !m.toggleToolDisclosure(ids[1]) || !m.toolDisclosures.get(ids[1]).expanded || !m.toolDisclosures.get(ids[0]).expanded {
		t.Fatal("toggling the second line changed the first")
	}
	if !m.toggleToolDisclosure(ids[0]) || m.toolDisclosures.get(ids[0]).expanded || !m.toolDisclosures.get(ids[1]).expanded {
		t.Fatal("folding the first line changed the second")
	}
}
