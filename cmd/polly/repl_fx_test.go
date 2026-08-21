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
