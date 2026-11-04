package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

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
	m.disclosures = disclosuresFolded // the checks below open a folded line by hand
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
	m.disclosures = disclosuresFolded // the checks below open a folded line by hand
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

func TestThinkingDisclosureExpansionShowsBoundedLiveTail(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.disclosures = disclosuresFolded // the checks below open a folded line by hand
	m.beginTurn("explain")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}

	for i := 0; i < 16; i++ {
		tui.ShowThinking(fmt.Sprintf("reasoning sentence number %d about the parse loop. ", i))
	}
	record := m.currentReasoningRecord()
	if record == nil || !m.toggleReasoning(record.id, testThinkingWidth) {
		t.Fatal("active reasoning disclosure did not expand")
	}

	// The entry keeps every retained row; the laid-out line windows them
	// to the newest rows behind an "earlier lines" link.
	entry := m.transcript[record.transcriptIndex].text
	if plain := plainStyledText(entry); !strings.Contains(plain, "number 0 ") || !strings.Contains(plain, "▾ thought") || !strings.Contains(entry, "mod:italic") {
		t.Fatalf("entry lost retained rows, its open label or preview styling: %q", entry)
	}
	window := func(stage string) string {
		t.Helper()
		block := plainStyledText(activityBlocks(m, testThinkingWidth)[0].activityReasoningDetail)
		lines := strings.Split(block, "\n")
		if got := len(lines) - 1; got != reasoningPreviewLines || !strings.Contains(lines[0], "earlier lines") {
			t.Fatalf("%s: window rows = %d, want %d behind a link: %q", stage, got, reasoningPreviewLines, block)
		}
		if !strings.Contains(block, "number 15") || strings.Contains(block, "number 0 ") {
			t.Fatalf("%s: window should show only the newest tail: %q", stage, block)
		}
		return block
	}
	window("expanded")

	// New chunks repaint the open tail immediately without growing the window.
	tui.ShowThinking(" newest-live-marker")
	m.refreshReasoningRecord(record, testThinkingWidth)
	if block := window("live"); !strings.Contains(block, "newest-live-marker") {
		t.Fatalf("open disclosure did not follow the live stream: %q", block)
	}
}

func TestThinkingDisclosurePreviewUsesFullTranscriptWidth(t *testing.T) {
	const width = 140
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.disclosures = disclosuresFolded // the checks below open a folded line by hand
	m.beginTurn("explain")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}
	tui.ShowThinking(strings.Repeat("wide reasoning preview content ", 32))
	record := m.currentReasoningRecord()
	if record == nil || !m.toggleReasoning(record.id, width) {
		t.Fatal("reasoning disclosure did not expand")
	}

	want := width - rw.StringWidth(reasoningBlockIndent)
	if record.previewWidth != want {
		t.Fatalf("preview width = %d, want full available width %d", record.previewWidth, want)
	}
	if got := strings.Count(plainStyledText(activityBlocks(m, width)[0].activityReasoningDetail), "\n"); got >= reasoningPreviewLines+1 {
		t.Fatalf("expanded window rows = %d, want at most %d plus a link", got, reasoningPreviewLines)
	}
}

func TestSettledThinkingPreviewKeepsNarrowTerminalWidth(t *testing.T) {
	const width = 24
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.disclosures = disclosuresFolded // the checks below open a folded line by hand
	m.reasoningWidth = width
	m.beginTurn("explain")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}
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
	rows := transcriptBlockRows(activityBlocks(m, width)[0].text, false, width)
	if got := len(rows) - 1; got > reasoningPreviewLines+1 {
		t.Fatalf("settled window uses %d physical detail rows, want at most %d plus a link", got, reasoningPreviewLines)
	}
}

func TestSettledThoughtStaysOpenAndIdleCtrlOCycles(t *testing.T) {
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

	// With no active turn, Ctrl-O cycles the view: every line open, then
	// details, then every line folded, then the default again, where this
	// newest thought is open.
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
	if m.disclosures != disclosuresOpen || !record.expanded {
		t.Fatalf("idle Ctrl-O did not open the view: mode=%v expanded=%v", m.disclosures, record.expanded)
	}
	opened := plainStyledText(m.transcript[record.transcriptIndex].text)
	if !strings.Contains(opened, "bounded reasoning") {
		t.Fatalf("opened completed thought = %q", opened)
	}
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
	if m.disclosures != disclosuresDetailed || !record.expanded {
		t.Fatalf("second idle Ctrl-O did not keep the thought open in detailed mode: mode=%v expanded=%v", m.disclosures, record.expanded)
	}
	if detailed := plainStyledText(m.transcript[record.transcriptIndex].text); detailed != opened {
		t.Fatalf("detailed mode changed the reasoning preview: %q -> %q", opened, detailed)
	}
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
	if m.disclosures != disclosuresFolded || record.expanded {
		t.Fatalf("third idle Ctrl-O did not fold the thought: mode=%v expanded=%v", m.disclosures, record.expanded)
	}
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
	if m.disclosures != disclosuresDefault || !record.expanded {
		t.Fatalf("fourth idle Ctrl-O did not return to the default with the newest thought open: mode=%v expanded=%v", m.disclosures, record.expanded)
	}
}

// Ctrl-O addresses the view, not one turn: successive presses open every
// thinking and tool block, reveal details, then fold them all.
func TestCtrlOExpandsEveryBlockInTheView(t *testing.T) {
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

	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
	if m.disclosures != disclosuresOpen || !older.expanded || !active.expanded || !olderTool.expanded || !liveTool.expanded {
		t.Fatalf("Ctrl-O did not expand every block: mode=%v older=%v active=%v olderTool=%v liveTool=%v",
			m.disclosures, older.expanded, active.expanded, olderTool.expanded, liveTool.expanded)
	}
	for _, record := range []*toolDisclosureRecord{olderTool, liveTool} {
		for _, row := range record.rows {
			if row.outputExpanded {
				t.Fatalf("opened mode expanded output for %q", row.callID)
			}
		}
	}
	if shown := plainStyledText(m.transcript[active.transcriptIndex].text); !strings.Contains(shown, "new live reasoning") {
		t.Fatalf("expanded inline block did not show the live tail: %q", shown)
	}

	// A tool phase pauses rather than breaks the run: the continuation
	// resumes the same record and keeps the deliberate expansion, and the
	// call joins the run's open tool block rather than starting a closed one.
	between := messages.ChatMessageToolCall{ID: "between", Name: "inspect"}
	secondUI.AppendToolStart([]messages.ChatMessageToolCall{between})
	secondUI.AppendToolEnd(between, "ok", time.Millisecond, nil)
	secondUI.ShowThinking("continued reasoning")
	if resumed := m.currentReasoningRecord(); resumed != active || !resumed.expanded {
		t.Fatalf("unbroken continuation did not resume the expanded record: %#v", resumed)
	}
	if joined := m.currentToolDisclosure(); joined != liveTool || !joined.expanded || len(joined.rows) != 2 || joined.rows[1].outputExpanded {
		t.Fatalf("tool phase did not join the run's open tool block with output closed: %#v", joined)
	}

	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
	if m.disclosures != disclosuresDetailed || !older.expanded || !active.expanded || !olderTool.expanded || !liveTool.expanded {
		t.Fatalf("second Ctrl-O did not keep every block open in detailed mode: mode=%v older=%v active=%v olderTool=%v liveTool=%v",
			m.disclosures, older.expanded, active.expanded, olderTool.expanded, liveTool.expanded)
	}
	for _, record := range []*toolDisclosureRecord{olderTool, liveTool} {
		for _, row := range record.rows {
			if !row.outputExpanded {
				t.Fatalf("detailed mode did not expand output for %q", row.callID)
			}
		}
	}

	// The third press folds every block and closes every output.
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
	if m.disclosures != disclosuresFolded || older.expanded || active.expanded || olderTool.expanded || liveTool.expanded {
		t.Fatalf("third Ctrl-O did not fold every block: mode=%v older=%v active=%v olderTool=%v liveTool=%v",
			m.disclosures, older.expanded, active.expanded, olderTool.expanded, liveTool.expanded)
	}
	for _, record := range []*toolDisclosureRecord{olderTool, liveTool} {
		for _, row := range record.rows {
			if row.outputExpanded {
				t.Fatalf("folded mode retained expanded output for %q", row.callID)
			}
		}
	}
	// Blocks that arrive later inherit the folded level.
	secondUI.AppendAssistantText("interim answer")
	secondUI.ShowThinking("later reasoning segment")
	later := m.currentReasoningRecord()
	if later == nil || later.expanded {
		t.Fatalf("later reasoning segment inherited an expansion: %#v", later)
	}

	// The fourth press returns to the default: the newest turn's blocks open,
	// the older turn's stay folded.
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
	if m.disclosures != disclosuresDefault || older.expanded || !active.expanded || !later.expanded || olderTool.expanded || !liveTool.expanded {
		t.Fatalf("fourth Ctrl-O did not return to the default: mode=%v older=%v active=%v later=%v olderTool=%v liveTool=%v",
			m.disclosures, older.expanded, active.expanded, later.expanded, olderTool.expanded, liveTool.expanded)
	}
}

func TestBusyCtrlOExpandsAndCollapsesEveryReasoningRun(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("inspect")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}

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

	// A manual expansion does not advance the global cycle: its first press
	// opens the whole view, including the other run.
	if !m.toggleReasoning(ids[0], testThinkingWidth) {
		t.Fatal("fixture reasoning record did not expand")
	}
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
	if m.disclosures != disclosuresOpen || !m.reasoningRecords.get(ids[1]).expanded || !m.reasoningRecords.get(ids[0]).expanded {
		t.Fatalf("Ctrl-O did not expand the latest run while leaving the earlier one open: mode=%v first=%v second=%v",
			m.disclosures, m.reasoningRecords.get(ids[0]).expanded, m.reasoningRecords.get(ids[1]).expanded)
	}

	// A local collapse likewise does not restart the cycle. The second
	// press advances to details and restores every reasoning preview.
	if !m.toggleReasoning(ids[0], testThinkingWidth) || m.reasoningRecords.get(ids[0]).expanded {
		t.Fatal("fixture reasoning record did not collapse locally")
	}
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
	if m.disclosures != disclosuresDetailed || !m.reasoningRecords.get(ids[0]).expanded || !m.reasoningRecords.get(ids[1]).expanded {
		t.Fatalf("second Ctrl-O did not open every run in detailed mode: mode=%v first=%v second=%v",
			m.disclosures, m.reasoningRecords.get(ids[0]).expanded, m.reasoningRecords.get(ids[1]).expanded)
	}

	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
	if m.disclosures != disclosuresFolded || m.reasoningRecords.get(ids[0]).expanded || m.reasoningRecords.get(ids[1]).expanded {
		t.Fatalf("third Ctrl-O did not fold every run: mode=%v first=%v second=%v",
			m.disclosures, m.reasoningRecords.get(ids[0]).expanded, m.reasoningRecords.get(ids[1]).expanded)
	}
	// A run that arrives after the third press inherits folded mode.
	tui.ShowThinking("third reasoning phase")
	later := m.currentReasoningRecord()
	if later == nil || later.expanded {
		t.Fatalf("later reasoning record did not inherit folded mode: %#v", later)
	}
	// The fourth press returns to the default: every run belongs to the
	// newest turn, so all three open.
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
	if m.disclosures != disclosuresDefault || !m.reasoningRecords.get(ids[0]).expanded || !m.reasoningRecords.get(ids[1]).expanded || !later.expanded {
		t.Fatalf("fourth Ctrl-O did not open the newest turn's runs: mode=%v first=%v second=%v later=%v",
			m.disclosures, m.reasoningRecords.get(ids[0]).expanded, m.reasoningRecords.get(ids[1]).expanded, later.expanded)
	}
}

// The shortcut's mode is sticky: opened and detailed apply to blocks that
// arrive later — later runs, later batches, later turns — and settlement skips
// the auto-collapse. The third press folds existing and future blocks.
func TestCtrlOHoldsExpansionForLaterBlocksAndTurns(t *testing.T) {
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
	if !first.expanded || !firstTool.expanded {
		t.Fatalf("Ctrl-O did not expand the present blocks: thought=%v tools=%v", first.expanded, firstTool.expanded)
	}

	// Future blocks in the same turn open: a new run after prose, and a new
	// batch after the prose settled the first one.
	tui.AppendAssistantText("interim answer")
	tui.ShowThinking("later run reasoning")
	later := m.currentReasoningRecord()
	if later == nil || later.id == first.id || !later.expanded {
		t.Fatalf("later reasoning run did not inherit the sticky expansion: %#v", later)
	}
	laterCall := messages.ChatMessageToolCall{ID: "c2", Name: "inspect"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{laterCall})
	tui.AppendToolEnd(laterCall, "ok", time.Second, nil)
	laterTool := m.currentToolDisclosure()
	if laterTool == nil || laterTool.id == firstTool.id || !laterTool.expanded {
		t.Fatalf("later tool batch did not inherit the sticky expansion: %#v", laterTool)
	}

	// Settlement keeps the turn's blocks open under the sticky expansion.
	r.endTurn(nil)
	if !first.expanded || !later.expanded || !firstTool.expanded || !laterTool.expanded {
		t.Fatalf("turn settlement collapsed sticky-expanded blocks: first=%v later=%v tools=%v/%v",
			first.expanded, later.expanded, firstTool.expanded, laterTool.expanded)
	}

	// The next turn's blocks open too.
	m.beginTurn("second")
	secondUI := &gotuiTurnUI{repl: r, model: r.model, config: r.config}
	secondUI.ShowThinking("second turn reasoning")
	secondTurn := m.currentReasoningRecord()
	if secondTurn == nil || !secondTurn.expanded {
		t.Fatalf("second turn did not inherit the sticky expansion: %#v", secondTurn)
	}

	for _, record := range []*toolDisclosureRecord{firstTool, laterTool} {
		for _, row := range record.rows {
			if row.outputExpanded {
				t.Fatalf("sticky opened mode expanded output for %q", row.callID)
			}
		}
	}

	// The second press keeps the hold and also opens every output.
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
	if m.disclosures != disclosuresDetailed || !first.expanded || !later.expanded || !secondTurn.expanded || !firstTool.expanded || !laterTool.expanded {
		t.Fatalf("second Ctrl-O did not retain expanded blocks in detailed mode: mode=%v first=%v later=%v second=%v tools=%v/%v",
			m.disclosures, first.expanded, later.expanded, secondTurn.expanded, firstTool.expanded, laterTool.expanded)
	}
	secondCall := messages.ChatMessageToolCall{ID: "c3", Name: "inspect"}
	secondUI.AppendToolStart([]messages.ChatMessageToolCall{secondCall})
	secondUI.AppendToolEnd(secondCall, "ok", time.Second, nil)
	secondTool := m.currentToolDisclosure()
	if secondTool == nil || !secondTool.expanded || len(secondTool.rows) != 1 {
		t.Fatalf("second-turn tool batch did not inherit detailed mode: %#v", secondTool)
	}
	for _, record := range []*toolDisclosureRecord{firstTool, laterTool, secondTool} {
		for _, row := range record.rows {
			if !row.outputExpanded {
				t.Fatalf("sticky detailed mode did not expand output for %q", row.callID)
			}
		}
	}

	// The third press folds existing blocks; later blocks inherit that mode.
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
	if m.disclosures != disclosuresFolded || first.expanded || later.expanded || secondTurn.expanded {
		t.Fatalf("third Ctrl-O left a thought open: mode=%v first=%v later=%v second=%v",
			m.disclosures, first.expanded, later.expanded, secondTurn.expanded)
	}
	for _, record := range []*toolDisclosureRecord{firstTool, laterTool, secondTool} {
		if record.expanded {
			t.Fatalf("third Ctrl-O left tool block %d open", record.id)
		}
		for _, row := range record.rows {
			if row.outputExpanded {
				t.Fatalf("folded mode retained expanded output for %q", row.callID)
			}
		}
	}
	secondUI.AppendAssistantText("interim")
	secondUI.ShowThinking("post-collapse reasoning")
	after := m.currentReasoningRecord()
	if after == nil || after.expanded {
		t.Fatalf("post-collapse reasoning run did not inherit folded mode: %#v", after)
	}

	// The fourth press returns to the default: the first turn's blocks stay
	// folded, the second turn's open with their outputs closed.
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
	if m.disclosures != disclosuresDefault || first.expanded || later.expanded || !secondTurn.expanded || !after.expanded {
		t.Fatalf("fourth Ctrl-O did not return to the default: mode=%v first=%v later=%v second=%v after=%v",
			m.disclosures, first.expanded, later.expanded, secondTurn.expanded, after.expanded)
	}
	if firstTool.expanded || laterTool.expanded || !secondTool.expanded || secondTool.rows[0].outputExpanded {
		t.Fatalf("fourth Ctrl-O tool blocks: first=%v later=%v second=%v output=%v", firstTool.expanded, laterTool.expanded, secondTool.expanded, secondTool.rows[0].outputExpanded)
	}
}

func TestFailedReasoningDisclosureIsLocalAndMarkedUnsaved(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.disclosures = disclosuresFolded // the checks below open a folded line by hand
	m.beginTurn("explain")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}
	tui.ShowThinking("reasoning from a failed turn")
	record := m.currentReasoningRecord()
	r.endTurn(errors.New("provider failed"))
	if record == nil || !record.complete || !record.unsaved || record.expanded {
		t.Fatalf("failed reasoning disclosure = %#v", record)
	}
	if collapsed := plainStyledText(m.transcript[record.transcriptIndex].text); !strings.Contains(collapsed, "not saved") || strings.Contains(collapsed, "reasoning from") {
		t.Fatalf("failed collapsed disclosure = %q", collapsed)
	}
	if !m.toggleReasoning(record.id, testThinkingWidth) {
		t.Fatal("failed reasoning disclosure did not expand locally")
	}
	if expanded := plainStyledText(m.transcript[record.transcriptIndex].text); !strings.Contains(expanded, "reasoning from a failed turn") {
		t.Fatalf("failed local reasoning tail = %q", expanded)
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
	m.disclosures = disclosuresFolded // the checks below open a folded line by hand
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
	// The shortcut is view-wide: it opens the older turns' thoughts too.
	if !m.toggleAllDisclosures(testThinkingWidth) || !first.expanded || !second.expanded || !third.expanded {
		t.Fatalf("view toggle did not open every disclosure: first=%v second=%v third=%v",
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
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}
	tui.ShowThinking("甲乙丙丁 lots of reasoning that would otherwise wrap")
	record := m.currentReasoningRecord()
	if record == nil {
		t.Fatal("reasoning record was not created")
	}
	record.expanded = true
	m.refreshReasoningRecord(record, rw.StringWidth(reasoningBlockIndent)+1)
	if got := strings.Count(m.transcript[record.transcriptIndex].text, "\n"); got != 0 {
		t.Fatalf("tiny terminal rendered wrapped preview rows: %q", m.transcript[record.transcriptIndex].text)
	}
}

func TestExpandedShortReasoningReservesTwoDetailRows(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.disclosures = disclosuresFolded // the checks below open a folded line by hand
	m.beginTurn("explain")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}
	tui.ShowThinking("short")
	record := m.currentReasoningRecord()
	if record == nil || !m.toggleReasoning(record.id, testThinkingWidth) {
		t.Fatal("short reasoning disclosure did not expand")
	}
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
	m.disclosures = disclosuresFolded // the checks below open a folded line by hand
	m.beginTurn("explain")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}
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

func TestCompletedReasoningKeepsInlineHitbox(t *testing.T) {
	const width = 50
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.disclosures = disclosuresFolded // the checks below open a folded line by hand
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

func TestToggleToolDisclosureGroupAppliesBeforeRefresh(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.disclosures = disclosuresFolded // the checks below open a folded line by hand
	m.beginTurn("work")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}

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
	if !m.toggleDisclosureGroup(activityTools, ids, 0) {
		t.Fatal("group expand returned false")
	}
	for _, id := range ids {
		if !m.toolDisclosures.get(id).expanded {
			t.Fatalf("disclosure %d not expanded after group expand", id)
		}
	}

	// A mixed group still advertises an open control, so its first click must
	// collapse every record rather than re-expand the closed member.
	m.toolDisclosures.get(ids[1]).expanded = false
	m.refreshToolDisclosure(m.toolDisclosures.get(ids[1]))
	if !m.toggleDisclosureGroup(activityTools, ids, 0) {
		t.Fatal("mixed group collapse returned false")
	}
	for _, id := range ids {
		if m.toolDisclosures.get(id).expanded {
			t.Fatalf("disclosure %d still expanded after mixed group collapse", id)
		}
	}
}
