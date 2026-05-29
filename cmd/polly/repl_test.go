package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
	ui "github.com/metaspartan/gotui/v5"
)

func TestSelectConversationMode(t *testing.T) {
	cases := []struct {
		name      string
		config    *Config
		stdinData bool
		want      conversationMode
		wantErr   bool
	}{
		{"prompt set → oneshot", &Config{PromptSet: true}, false, conversationModeOneShot, false},
		{"stdin available → oneshot", &Config{}, true, conversationModeOneShot, false},
		{"no input, no constraints → repl", &Config{}, false, conversationModeREPL, false},
		{"files require prompt", &Config{Files: []string{"x"}}, false, conversationModeOneShot, true},
		{"schema requires prompt", &Config{SchemaPath: "x.json"}, false, conversationModeOneShot, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := selectConversationMode(tc.config, tc.stdinData)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestValidateREPLConfigRejectsBoth(t *testing.T) {
	err := validateREPLConfig(&Config{Files: []string{"a"}, SchemaPath: "s.json"})
	if err == nil || !strings.Contains(err.Error(), "--file") || !strings.Contains(err.Error(), "--schema") {
		t.Fatalf("expected combined message, got %v", err)
	}
}

func TestReplModelSubmitAndHistory(t *testing.T) {
	m := newReplModel()
	m.ed.setText("hello")
	got := m.submitPrompt()
	if got != "hello" {
		t.Fatalf("submit returned %q", got)
	}
	if !m.busy {
		t.Fatal("submit should mark busy")
	}
	if len(m.transcript) != 1 || !strings.Contains(m.transcript[0], "hello") {
		t.Fatalf("expected one transcript entry containing 'hello', got %+v", m.transcript)
	}
	if len(m.history) != 1 || m.history[0] != "hello" {
		t.Fatalf("history not recorded: %+v", m.history)
	}

	m.busy = false
	m.ed.clear()
	got = m.submitPrompt()
	if got != "" {
		t.Fatalf("empty submit returned %q", got)
	}
	if len(m.history) != 1 {
		t.Fatalf("empty submit should not extend history, got %d", len(m.history))
	}
}

func TestReplModelHistoryNavigation(t *testing.T) {
	m := newReplModel()
	m.history = []string{"first", "second"}
	m.ed.setText("draft")

	m.historyUp()
	if m.ed.text() != "second" {
		t.Fatalf("first up should land on 'second', got %q", m.ed.text())
	}
	m.historyUp()
	if m.ed.text() != "first" {
		t.Fatalf("second up should land on 'first', got %q", m.ed.text())
	}
	m.historyUp()
	if m.ed.text() != "first" {
		t.Fatalf("upper bound should stay on 'first', got %q", m.ed.text())
	}
	m.historyDown()
	if m.ed.text() != "second" {
		t.Fatalf("down should return to 'second', got %q", m.ed.text())
	}
	m.historyDown()
	if m.ed.text() != "draft" {
		t.Fatalf("down past last should restore draft, got %q", m.ed.text())
	}
}

func TestReplModelHistoryWorksWhileBusy(t *testing.T) {
	// The prompt stays editable during a turn, so history recall must work too
	// (the user can compose/queue the next message while the model runs).
	m := newReplModel()
	m.history = []string{"one"}
	m.busy = true
	m.historyUp()
	if m.ed.text() != "one" {
		t.Fatalf("history recall while busy = %q, want \"one\"", m.ed.text())
	}
}

func TestReplModelAppendAssistantStreaming(t *testing.T) {
	m := newReplModel()
	m.appendAssistant("Hello ")
	m.appendAssistant("world")
	if len(m.transcript) != 1 {
		t.Fatalf("streaming should accumulate into one entry, got %d", len(m.transcript))
	}
	if m.transcript[0] != "Hello world" {
		t.Fatalf("got %q", m.transcript[0])
	}

	m.appendLine("tool started")
	m.appendAssistant("after-tool")
	if len(m.transcript) != 3 {
		t.Fatalf("expected new assistant entry after non-assistant line, got %d", len(m.transcript))
	}
}

func TestReplModelApprovalFlow(t *testing.T) {
	m := newReplModel()
	reply := make(chan []bool, 1)
	m.approval = &approvalState{
		calls: []messages.ChatMessageToolCall{{Name: "a"}, {Name: "b"}, {Name: "c"}},
		reply: reply,
	}

	m.handleApprovalAnswer('y')
	if m.approval == nil || m.approval.index != 1 {
		t.Fatalf("first yes should advance, got %+v", m.approval)
	}
	m.handleApprovalAnswer('n')
	if m.approval.index != 2 {
		t.Fatalf("no should advance, got %+v", m.approval)
	}
	m.handleApprovalAnswer('y')
	if m.approval != nil {
		t.Fatalf("final answer should clear approval, got %+v", m.approval)
	}
	got := <-reply
	want := []bool{true, false, true}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestReplModelApprovalAcceptAllShortCircuits(t *testing.T) {
	m := newReplModel()
	reply := make(chan []bool, 1)
	m.approval = &approvalState{
		calls: []messages.ChatMessageToolCall{{Name: "a"}, {Name: "b"}},
		reply: reply,
	}
	m.handleApprovalAnswer('a')
	if m.approval != nil {
		t.Fatalf("a should clear approval")
	}
	got := <-reply
	if len(got) != 2 || !got[0] || !got[1] {
		t.Fatalf("a should approve all, got %v", got)
	}
}

func TestLineEditorWordOps(t *testing.T) {
	var e lineEditor
	e.setText("foo bar baz")

	// deleteWordBackward from end removes "baz".
	e.deleteWordBackward()
	if e.text() != "foo bar " {
		t.Fatalf("deleteWordBackward = %q", e.text())
	}

	// wordLeft hops to the start of "bar"; another to start of "foo".
	e.wordLeft()
	if e.cursor != 4 {
		t.Fatalf("wordLeft cursor = %d, want 4", e.cursor)
	}
	e.wordLeft()
	if e.cursor != 0 {
		t.Fatalf("second wordLeft cursor = %d, want 0", e.cursor)
	}

	// deleteWordForward from start removes "foo" (cursor stays put).
	e.deleteWordForward()
	if e.text() != " bar " {
		t.Fatalf("deleteWordForward = %q", e.text())
	}
	if e.cursor != 0 {
		t.Fatalf("deleteWordForward moved cursor to %d", e.cursor)
	}

	// wordRight skips the leading space and lands past "bar".
	e.wordRight()
	if e.cursor != 4 {
		t.Fatalf("wordRight cursor = %d, want 4", e.cursor)
	}
}

func TestLineEditorKillAndInsert(t *testing.T) {
	var e lineEditor
	e.setText("hello world")
	e.home()
	e.right()
	e.right()
	e.killToEnd()
	if e.text() != "he" {
		t.Fatalf("killToEnd = %q", e.text())
	}
	e.killToStart()
	if e.text() != "" || e.cursor != 0 {
		t.Fatalf("killToStart = %q cursor %d", e.text(), e.cursor)
	}
	e.insert('x')
	e.insert('y')
	if e.text() != "xy" || e.cursor != 2 {
		t.Fatalf("insert = %q cursor %d", e.text(), e.cursor)
	}
}

func TestLineEditorVerticalMove(t *testing.T) {
	var e lineEditor
	e.setText("abcde\nfg\nhijkl") // lines of length 5, 2, 5
	// Cursor is at the very end (line 2, col 5).

	// up from the last line clamps to the short middle line's end (col 2)...
	if !e.up() {
		t.Fatal("up from last line should move within the buffer")
	}
	if e.cursor != 8 { // "abcde\n" = 6, "fg" end at 8
		t.Fatalf("up landed at cursor %d, want 8 (end of \"fg\")", e.cursor)
	}
	// ...but the goal column (5) is remembered, so a second up restores col 5
	// on the long first line rather than staying at the clamped col 2.
	if !e.up() {
		t.Fatal("second up should move to the first line")
	}
	if e.cursor != 5 { // col 5 on "abcde"
		t.Fatalf("second up landed at cursor %d, want 5 (goal column held)", e.cursor)
	}
	// up on the first line reports no movement (caller recalls history).
	if e.up() {
		t.Fatal("up on the first line should return false")
	}
	if e.cursor != 5 {
		t.Fatalf("failed up moved cursor to %d", e.cursor)
	}

	// down mirrors: back to the clamped middle line, then col 5 on the last line.
	if !e.down() || e.cursor != 8 {
		t.Fatalf("down to middle line: cursor %d, want 8", e.cursor)
	}
	if !e.down() || e.cursor != 14 { // "abcde\nfg\n" = 9, col 5 -> 14
		t.Fatalf("down to last line: cursor %d, want 14", e.cursor)
	}
	if e.down() {
		t.Fatal("down on the last line should return false")
	}

	// A horizontal move resets the goal column, so the next up uses the new col.
	e.setText("abcde\nfg\nhijkl")
	e.up()       // -> col 2 on "fg" (goal 5)
	e.left()     // -> col 1, goal reset
	if !e.up() { // -> col 1 on "abcde"
		t.Fatal("up after left should move")
	}
	if e.cursor != 1 {
		t.Fatalf("up after left landed at cursor %d, want 1 (goal recomputed)", e.cursor)
	}
}

func TestHandleEventUpDownLineThenHistory(t *testing.T) {
	r := &managedREPL{model: newReplModel()}
	m := r.model
	m.history = []string{"old one", "old two"}
	m.historyIdx = -1
	m.ed.setText("first\nsecond") // cursor at end (line 1)

	up := ui.Event{Type: ui.KeyboardEvent, ID: "<Up>"}

	// First Up moves within the buffer to the first line — history untouched.
	r.handleEvent(up)
	if m.ed.text() != "first\nsecond" {
		t.Fatalf("Up within buffer altered text: %q", m.ed.text())
	}
	if m.historyIdx != -1 {
		t.Fatalf("Up within buffer touched history (idx %d)", m.historyIdx)
	}
	if row, _ := m.inputCursorRowCol(); row != 0 {
		t.Fatalf("cursor row after Up = %d, want 0", row)
	}

	// Now on the first line: another Up recalls the newest history entry.
	r.handleEvent(up)
	if m.ed.text() != "old two" {
		t.Fatalf("Up on first line should recall history, got %q", m.ed.text())
	}
}

func TestToolErrorParts(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		wantCode    string
		wantSummary string
	}{
		{
			// bash-style: exit code + last line of the embedded output.
			name:        "bash with traceback",
			err:         errors.New("command failed: exit status 1 (output: Traceback ...\n  File x\nModuleNotFoundError: No module named 'foo')"),
			wantCode:    "exit 1",
			wantSummary: "ModuleNotFoundError: No module named 'foo'",
		},
		{
			// shell-style (%v, no wrap) still yields the exit code via the string.
			name:        "shell with one-line output",
			err:         errors.New("tool execution failed: exit status 2 (output: ls: nope: No such file or directory)"),
			wantCode:    "exit 2",
			wantSummary: "ls: nope: No such file or directory",
		},
		{
			// A command that failed with no output shows just the code.
			name:        "empty output",
			err:         errors.New("command failed: exit status 3 (output: )"),
			wantCode:    "exit 3",
			wantSummary: "",
		},
		{
			// No exit code and no output wrapper: fall back to the message line.
			name:        "timeout-style message",
			err:         errors.New("tool execution timed out after 30s"),
			wantCode:    "",
			wantSummary: "tool execution timed out after 30s",
		},
	}
	for _, c := range cases {
		code, summary := toolErrorParts(c.err)
		if code != c.wantCode || summary != c.wantSummary {
			t.Errorf("%s: toolErrorParts = (%q, %q), want (%q, %q)", c.name, code, summary, c.wantCode, c.wantSummary)
		}
	}
}

func TestToolErrorPartsTruncates(t *testing.T) {
	long := strings.Repeat("x", toolErrorSummaryMax+50)
	err := fmt.Errorf("command failed: exit status 1 (output: %s)", long)
	code, summary := toolErrorParts(err)
	if code != "exit 1" {
		t.Fatalf("code = %q, want \"exit 1\"", code)
	}
	if !strings.HasSuffix(summary, "…") {
		t.Fatalf("long summary should end with ellipsis: %q", summary)
	}
	if n := len([]rune(summary)); n != toolErrorSummaryMax {
		t.Fatalf("summary len = %d runes, want %d", n, toolErrorSummaryMax)
	}
}

func TestToolExitCodeFromExitError(t *testing.T) {
	// A real subprocess yields a *exec.ExitError; bash wraps it with %w, so
	// errors.As must still recover the code through the wrapper.
	raw := exec.Command("bash", "-c", "exit 7").Run()
	if raw == nil {
		t.Skip("bash unavailable or did not fail")
	}
	wrapped := fmt.Errorf("command failed: %w (output: boom)", raw)
	if code, ok := toolExitCode(wrapped); !ok || code != 7 {
		t.Fatalf("toolExitCode(wrapped) = (%d, %v), want (7, true)", code, ok)
	}
	// Signal-killed processes report ExitCode()<0; we treat that as no code.
	if _, ok := toolExitCode(errors.New("command failed: signal: killed (output: )")); ok {
		t.Fatalf("a non-numeric failure should not report an exit code")
	}
}

func TestToolErrorLineRendering(t *testing.T) {
	err := errors.New("command failed: exit status 1 (output: line one\nfatal: the real error)")
	code, summary := toolErrorParts(err)
	line := toolErrorLine("bash", "1.4s", code, summary)

	if strings.Contains(line, "\n") {
		t.Fatalf("error line should not wrap into multiple rows: %q", line)
	}
	for _, want := range []string{"bash", "exit 1", "fatal: the real error"} {
		if !strings.Contains(line, want) {
			t.Errorf("error line %q missing %q", line, want)
		}
	}
	// The verbose first line of output must not appear — only the last line does.
	if strings.Contains(line, "line one") {
		t.Errorf("error line should not include earlier output: %q", line)
	}
	// The message is dim red; the metadata/exit code stays muted.
	if !strings.Contains(line, "fatal: the real error](fg:err,mod:dim)") {
		t.Errorf("summary should be dim red: %q", line)
	}
	if !strings.Contains(line, "exit 1](fg:muted)") {
		t.Errorf("exit code should be muted: %q", line)
	}
}

func TestRunningToolLine(t *testing.T) {
	line := runningToolLine("bash sleep 30", 15500*time.Millisecond)
	for _, want := range []string{"→", "bash sleep 30", "15.5s"} {
		if !strings.Contains(line, want) {
			t.Errorf("running tool line %q missing %q", line, want)
		}
	}
	// The arrow breathes: it's a themed hue whose modifier comes from the pulse.
	hasPulseMod := false
	for _, mod := range arrowPulse {
		if strings.Contains(line, "→](fg:run,mod:"+mod+")") {
			hasPulseMod = true
			break
		}
	}
	if !hasPulseMod {
		t.Errorf("arrow should use a pulse modifier: %q", line)
	}
}

func TestActiveToolLifecycle(t *testing.T) {
	m := newReplModel()

	// Starting a tool adds one transcript line and tracks it.
	m.appendToolStartLine("id1", "bash sleep 30")
	if len(m.transcript) != 1 || len(m.activeTools) != 1 {
		t.Fatalf("after start: %d transcript, %d active", len(m.transcript), len(m.activeTools))
	}
	if m.activeTools[0].index != 0 {
		t.Fatalf("active tool index = %d, want 0", m.activeTools[0].index)
	}

	// A render frame rewrites the line with live elapsed time.
	m.activeTools[0].started = m.activeTools[0].started.Add(-15500 * time.Millisecond)
	m.refreshActiveTools()
	if !strings.Contains(m.transcript[0], "15.5s") {
		t.Fatalf("refreshed line missing elapsed: %q", m.transcript[0])
	}

	// Finishing it frees the slot and freezes the line in place — still one line.
	idx, ok := m.takeActiveTool("id1")
	if !ok || idx != 0 {
		t.Fatalf("takeActiveTool = (%d, %v), want (0, true)", idx, ok)
	}
	if len(m.activeTools) != 0 {
		t.Fatalf("active tools should be empty, got %d", len(m.activeTools))
	}
	m.transcript[idx] = toolOKLine("bash sleep 30", "30.0s")
	if len(m.transcript) != 1 || !strings.Contains(m.transcript[0], "✓") {
		t.Fatalf("finalized transcript = %v", m.transcript)
	}
}

func TestTakeActiveToolMatchesByIDWithFallback(t *testing.T) {
	m := newReplModel()
	m.appendToolStartLine("a", "bash one") // index 0
	m.appendToolStartLine("b", "bash two") // index 1

	// Out-of-order finish: the second tool's id resolves to its own line.
	if idx, ok := m.takeActiveTool("b"); !ok || idx != 1 {
		t.Fatalf("take(b) = (%d, %v), want (1, true)", idx, ok)
	}
	// An unknown id falls back to the oldest still-running entry (a@0).
	if idx, ok := m.takeActiveTool("missing"); !ok || idx != 0 {
		t.Fatalf("take(missing) = (%d, %v), want (0, true)", idx, ok)
	}
	if _, ok := m.takeActiveTool("anything"); ok {
		t.Fatalf("take on empty should report false")
	}
}

func TestStatusRowShowsSpinnerWhenBusy(t *testing.T) {
	m := newReplModel()
	m.modelName = "gpt-mini"
	m.contextName = "ctx"

	// Idle: no spinner glyph and no "idle" label — an idle bar is just the
	// static fields.
	idle := m.statusRow(120)
	if strings.ContainsAny(idle, string(spinnerFrames)) {
		t.Fatalf("idle status row should carry no spinner, got %q", idle)
	}
	if strings.Contains(idle, "idle") {
		t.Fatalf("idle status row should omit the idle label, got %q", idle)
	}

	// Busy: a spinner frame leads the bar, alongside the state word.
	m.state = turnStateThinking
	m.turnStarted = time.Now()
	busy := m.statusRow(120)
	if !strings.ContainsAny(busy, string(spinnerFrames)) {
		t.Fatalf("busy status row should lead with a spinner frame, got %q", busy)
	}
	if !strings.Contains(busy, "thinking") {
		t.Fatalf("busy status row should include the state word, got %q", busy)
	}

	// The spinner glyph sits at the very start (far left) of the bar.
	cells := ui.ParseStyles(busy, ui.NewStyle(ui.ColorWhite))
	if len(cells) == 0 || !strings.ContainsRune(string(spinnerFrames), cells[0].Rune) {
		t.Fatalf("first cell should be a spinner frame, got bar %q", busy)
	}

	// The live activity (state + elapsed) is grouped right after the spinner,
	// ahead of the static model name.
	if i, j := strings.Index(busy, "thinking"), strings.Index(busy, "gpt-mini"); i < 0 || j < 0 || i > j {
		t.Fatalf("state word should precede the model name, got %q", busy)
	}
}

func TestStatusRowGutterKeepsStaticFixed(t *testing.T) {
	// During a turn the fixed-width gutter keeps the static fields at a constant
	// column across state/elapsed changes; at idle the gutter collapses so the
	// bar isn't left-padded when nothing is happening.
	plain := func(s string) string {
		var b strings.Builder
		for _, c := range ui.ParseStyles(s, ui.NewStyle(ui.ColorWhite)) {
			b.WriteRune(c.Rune)
		}
		return b.String()
	}

	// Display column of "gpt-mini" — measured in runes, since the spinner glyph
	// and middle-dot separators are multibyte (a byte offset would mislead).
	col := func(s string) int {
		p := plain(s)
		i := strings.Index(p, "gpt-mini")
		if i < 0 {
			t.Fatalf("model name missing from bar %q", p)
		}
		return len([]rune(p[:i]))
	}

	m := newReplModel()
	m.modelName = "gpt-mini"
	m.contextName = "ctx"

	idleCol := col(m.statusRow(120))

	busyCol := -1
	for _, st := range []turnState{turnStateWaiting, turnStateThinking, turnStateStreaming, turnStateTool} {
		m.state = st
		m.turnStarted = time.Now()
		c := col(m.statusRow(120))
		if busyCol == -1 {
			busyCol = c
		} else if c != busyCol {
			t.Fatalf("model shifted between states: %d vs %d (state %v)", busyCol, c, st)
		}
	}
	if idleCol >= busyCol {
		t.Fatalf("idle gutter should collapse below the busy gutter: idle %d, busy %d", idleCol, busyCol)
	}
}

func TestCompleteSlash(t *testing.T) {
	cases := []struct {
		in            string
		wantOK        bool
		wantCompleted string
		wantMatches   []string
	}{
		// Unique prefix completes to the full command.
		{"/h", true, "/help", []string{"/help"}},
		{"/e", true, "/exit", []string{"/exit"}},
		{"/q", true, "/quit", []string{"/quit"}},
		// Bare "/" matches everything; common prefix is just "/" (no progress).
		{"/", true, "/", slashCommands},
		// "/c" matches /clear and /context; common prefix extends to "/c".
		{"/c", true, "/c", []string{"/clear", "/context"}},
		{"/cl", true, "/clear", []string{"/clear"}},
		{"/t", true, "/tools", []string{"/tools"}},
		// Already complete stays put but still reports its single match.
		{"/help", true, "/help", []string{"/help"}},
		// No completion when it isn't a bare slash token.
		{"hello", false, "", nil},
		{"/help me", false, "", nil},
		{"/zzz", false, "", nil},
	}
	for _, c := range cases {
		completed, matches, ok := completeSlash(c.in)
		if ok != c.wantOK {
			t.Errorf("completeSlash(%q) ok = %v, want %v", c.in, ok, c.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if completed != c.wantCompleted {
			t.Errorf("completeSlash(%q) completed = %q, want %q", c.in, completed, c.wantCompleted)
		}
		if strings.Join(matches, ",") != strings.Join(c.wantMatches, ",") {
			t.Errorf("completeSlash(%q) matches = %v, want %v", c.in, matches, c.wantMatches)
		}
	}
}

func TestHandleEventTabCompletesAndLists(t *testing.T) {
	r := &managedREPL{model: newReplModel()}

	// Unique prefix: Tab fills in the whole command.
	r.model.ed.setText("/h")
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Tab>"})
	if got := r.model.ed.text(); got != "/help" {
		t.Fatalf("after Tab on /h, input = %q, want /help", got)
	}

	// Ambiguous bare "/": Tab can't extend, so it lists the candidates.
	r.model.ed.setText("/")
	before := len(r.model.transcript)
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Tab>"})
	if got := r.model.ed.text(); got != "/" {
		t.Fatalf("after Tab on /, input = %q, want / unchanged", got)
	}
	if len(r.model.transcript) != before+1 {
		t.Fatalf("expected one notice line listing candidates, got %d new", len(r.model.transcript)-before)
	}

	// Non-slash text: Tab inserts a literal tab (legacy behavior).
	r.model.ed.setText("ab")
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Tab>"})
	if got := r.model.ed.text(); got != "ab\t" {
		t.Fatalf("after Tab on ab, input = %q, want ab\\t", got)
	}
}

func TestBracketedPasteInsertsMultiLine(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	send := func(id string) { r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: id}) }

	send(pasteStartID)
	if !r.model.pasting {
		t.Fatal("PasteStart did not enter paste mode")
	}
	// Within a paste, an Enter is literal text, not a submit.
	send("a")
	send("<Enter>")
	send("b")
	send(pasteEndID)
	if r.model.pasting {
		t.Fatal("PasteEnd did not leave paste mode")
	}
	if r.model.ed.text() != "a\nb" {
		t.Fatalf("pasted text = %q, want \"a\\nb\"", r.model.ed.text())
	}
	select {
	case p := <-r.pending:
		t.Fatalf("paste should not submit, got %q", p)
	default:
	}

	// A real Enter now submits the whole multi-line prompt.
	send("<Enter>")
	select {
	case p := <-r.pending:
		if p != "a\nb" {
			t.Fatalf("submitted %q, want \"a\\nb\"", p)
		}
	default:
		t.Fatal("Enter after paste should submit")
	}
}

func TestCtrlJInsertsNewline(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	send := func(id string) { r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: id}) }
	send("a")
	send("<C-j>")
	send("b")
	if r.model.ed.text() != "a\nb" {
		t.Fatalf("Ctrl-J editor = %q, want \"a\\nb\"", r.model.ed.text())
	}
}

func TestRenderInputMultiLine(t *testing.T) {
	m := newReplModel()
	m.ed.setText("foo\nbar")
	text, rows, curRow, curCol, editable := m.renderInput()
	if !editable {
		t.Fatal("editable should be true")
	}
	if rows != 2 {
		t.Fatalf("rows = %d, want 2", rows)
	}
	// Cursor sits at the end of "bar": row 1, col = prompt(2) + len("bar").
	if curRow != 1 || curCol != 5 {
		t.Fatalf("cursor = (row %d, col %d), want (1, 5)", curRow, curCol)
	}
	if !strings.Contains(text, "foo") || !strings.Contains(text, "bar") {
		t.Fatalf("text = %q", text)
	}
}

func TestRenderInputAnchorsToBottom(t *testing.T) {
	m := newReplModel()
	var lines []string
	for i := 0; i < maxInputRows+3; i++ {
		lines = append(lines, fmt.Sprintf("L%d", i))
	}
	m.ed.setText(strings.Join(lines, "\n"))

	if got := m.inputRows(); got != maxInputRows {
		t.Fatalf("inputRows = %d, want %d", got, maxInputRows)
	}
	text, rows, curRow, _, _ := m.renderInput()
	if rows != maxInputRows {
		t.Fatalf("rows = %d, want %d", rows, maxInputRows)
	}
	// Cursor at the end lands on the last visible row; oldest lines drop off.
	if curRow != maxInputRows-1 {
		t.Fatalf("curRow = %d, want %d", curRow, maxInputRows-1)
	}
	if !strings.Contains(text, fmt.Sprintf("L%d", maxInputRows+2)) {
		t.Fatalf("newest line missing from %q", text)
	}
	if strings.Contains(text, "L0") {
		t.Fatalf("oldest line should have scrolled off: %q", text)
	}
}

func TestRenderInputFollowsCursorUp(t *testing.T) {
	m := newReplModel()
	var lines []string
	for i := 0; i < maxInputRows+3; i++ {
		lines = append(lines, fmt.Sprintf("L%d", i))
	}
	m.ed.setText(strings.Join(lines, "\n"))

	// Walk the cursor up to the very first line.
	for i := 0; i < maxInputRows+2; i++ {
		if !m.ed.up() {
			t.Fatalf("up %d returned false early", i)
		}
	}

	text, rows, curRow, _, _ := m.renderInput()
	if rows != maxInputRows {
		t.Fatalf("rows = %d, want %d", rows, maxInputRows)
	}
	// The window scrolled up to reveal the cursor: L0 is now shown and the
	// cursor sits on the top visible row, while the newest line scrolled off.
	if !strings.Contains(text, "L0") {
		t.Fatalf("cursor's line (L0) not visible: %q", text)
	}
	if curRow != 0 {
		t.Fatalf("curRow = %d, want 0 (cursor on top visible row)", curRow)
	}
	if strings.Contains(text, fmt.Sprintf("L%d", maxInputRows+2)) {
		t.Fatalf("newest line should have scrolled off the top-anchored window: %q", text)
	}
}

func TestReverseSearch(t *testing.T) {
	r := &managedREPL{model: newReplModel()}
	r.model.history = []string{"git status", "go build", "git commit", "go test"}
	send := func(id string) { r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: id}) }

	send("<C-r>")
	if !r.model.searching {
		t.Fatal("Ctrl-R did not enter search")
	}

	// Typing "git" matches the most recent entry containing it.
	send("g")
	send("i")
	send("t")
	if r.model.searchMatch < 0 || r.model.history[r.model.searchMatch] != "git commit" {
		t.Fatalf("first match = %d, want index of \"git commit\"", r.model.searchMatch)
	}

	// Repeated Ctrl-R steps to the older match.
	send("<C-r>")
	if r.model.history[r.model.searchMatch] != "git status" {
		t.Fatalf("stepped match = %q, want \"git status\"", r.model.history[r.model.searchMatch])
	}

	// Enter accepts the match into the editor and leaves search (no submit).
	send("<Enter>")
	if r.model.searching {
		t.Fatal("Enter did not exit search")
	}
	if r.model.ed.text() != "git status" {
		t.Fatalf("accepted text = %q", r.model.ed.text())
	}
}

func TestReverseSearchCancelKeepsDraft(t *testing.T) {
	r := &managedREPL{model: newReplModel()}
	r.model.history = []string{"alpha", "beta"}
	r.model.ed.setText("draft")

	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-r>"})
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "a"})

	// Ctrl-C cancels the search instead of quitting the REPL.
	if quit := r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-c>"}); quit {
		t.Fatal("Ctrl-C during search should cancel, not quit")
	}
	if r.model.searching {
		t.Fatal("Ctrl-C did not exit search")
	}
	if r.model.ed.text() != "draft" {
		t.Fatalf("cancel altered editor: %q", r.model.ed.text())
	}
}

func TestLoadHistory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hist")
	// Blank lines are skipped; a trailing newline is tolerated.
	if err := os.WriteFile(path, []byte("one\n\ntwo\nthree\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := loadHistory(path); strings.Join(got, ",") != "one,two,three" {
		t.Fatalf("loadHistory = %v", got)
	}
	// A missing file loads nil, not an error.
	if loadHistory(filepath.Join(dir, "nope")) != nil {
		t.Fatal("missing file should load nil")
	}
}

func TestInitHistoryTrimsAndPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hist")
	t.Setenv("POLLY_HISTORY_FILE", path)

	var b strings.Builder
	for i := 0; i < maxPersistedHistory+10; i++ {
		fmt.Fprintf(&b, "cmd%d\n", i)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	r.initHistory()

	if len(r.model.history) != maxPersistedHistory {
		t.Fatalf("loaded %d entries, want %d", len(r.model.history), maxPersistedHistory)
	}
	if last := r.model.history[len(r.model.history)-1]; last != fmt.Sprintf("cmd%d", maxPersistedHistory+9) {
		t.Fatalf("newest entry = %q", last)
	}

	// A new submission persists and survives a reload; the file stays trimmed.
	r.appendHistory("fresh")
	r.closeHistory()

	// loadHistory re-caps at maxPersistedHistory, so the count holds steady and
	// the freshly appended entry is the newest survivor.
	reloaded := loadHistory(path)
	if len(reloaded) != maxPersistedHistory {
		t.Fatalf("persisted %d entries, want %d", len(reloaded), maxPersistedHistory)
	}
	if last := reloaded[len(reloaded)-1]; last != "fresh" {
		t.Fatalf("appended entry not persisted; last = %q", last)
	}
}

func TestRunCommandSessionCommands(t *testing.T) {
	store := sessions.NewSyncMapSessionStore(nil)
	session, err := store.Get("ctx-test")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	session.AddMessage(messages.ChatMessage{Role: messages.MessageRoleUser, Content: "hi"})
	session.AddMessage(messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "hello"})

	r := newManagedREPL(&Config{}, "ctx-test", 0, 0)
	r.state = &conversationState{session: session, toolRegistry: tools.NewToolRegistry(nil)}

	// /context reports the session name and message stats.
	if handled, quit := r.runCommand("/context"); !handled || quit {
		t.Fatalf("/context handled=%v quit=%v", handled, quit)
	}
	if joined := strings.Join(r.model.transcript, "\n"); !strings.Contains(joined, "ctx-test") || !strings.Contains(joined, "messages:") {
		t.Fatalf("/context output missing fields: %q", joined)
	}

	// /clear empties both the session history and the transcript, leaving the notice.
	r.runCommand("/clear")
	if got := len(session.GetHistory()); got != 0 {
		t.Fatalf("/clear left %d messages", got)
	}
	if len(r.model.transcript) != 1 || !strings.Contains(r.model.transcript[0], "cleared") {
		t.Fatalf("/clear transcript = %v", r.model.transcript)
	}

	// /tools on an empty registry reports none.
	r.model.transcript = nil
	r.runCommand("/tools")
	if !strings.Contains(strings.Join(r.model.transcript, "\n"), "no tools loaded") {
		t.Fatalf("/tools = %v", r.model.transcript)
	}

	// Unknown command is not handled (caller reports it).
	if handled, _ := r.runCommand("/bogus"); handled {
		t.Fatal("/bogus should be unhandled")
	}

	// /quit signals quit.
	if _, quit := r.runCommand("/quit"); !quit {
		t.Fatal("/quit should signal quit")
	}
}

func TestAppendHelpPopulatesTranscript(t *testing.T) {
	m := newReplModel()
	m.appendHelp()
	joined := strings.Join(m.transcript, "\n")
	if !strings.Contains(joined, "commands:") || !strings.Contains(joined, "Ctrl-C") {
		t.Fatalf("transcript missing help content: %q", joined)
	}
}

func TestRunREPLLoopShowsHelp(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader("/help\n"))
	var out bytes.Buffer
	err := runREPLLoop(reader, &out, func(prompt string) error {
		t.Fatalf("runTurn should not be called for /help")
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), "commands:") || !strings.Contains(out.String(), "/exit") {
		t.Fatalf("help output missing: %q", out.String())
	}
}

func TestHandleInterruptCancelsThenQuits(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	canceled := false
	r.turnCancel = func() { canceled = true }
	reply := make(chan []bool, 1)
	r.model.busy = true
	r.model.state = turnStateTool
	r.model.toolName = "bash"
	r.model.approval = &approvalState{
		calls: []messages.ChatMessageToolCall{{Name: "bash"}},
		reply: reply,
	}

	// First Ctrl-C: cancel the turn, deny the pending approval, stay open.
	if quit := r.handleInterrupt(); quit {
		t.Fatal("first interrupt should not quit")
	}
	if !canceled {
		t.Fatal("first interrupt should cancel the turn context")
	}
	if !r.model.canceling {
		t.Fatal("first interrupt should mark canceling")
	}
	if r.model.approval != nil {
		t.Fatal("first interrupt should clear the pending approval")
	}
	if got := <-reply; len(got) != 1 || got[0] {
		t.Fatalf("pending approval should be denied, got %v", got)
	}
	if r.model.busyLabel() != "canceling" {
		t.Fatalf("busy label should read 'canceling', got %q", r.model.busyLabel())
	}

	// Second Ctrl-C while still winding down: force quit.
	if quit := r.handleInterrupt(); !quit {
		t.Fatal("second interrupt should quit")
	}
}

func TestEnterWhileBusyQueues(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	send := func(id string) { r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: id}) }

	r.model.busy = true
	r.model.ed.setText("queued one")
	send("<Enter>")

	if r.model.ed.text() != "" {
		t.Fatalf("editor should clear after queueing, got %q", r.model.ed.text())
	}
	if len(r.model.queue) != 1 || r.model.queue[0] != "queued one" {
		t.Fatalf("queue = %v, want [\"queued one\"]", r.model.queue)
	}
	if len(r.model.history) != 1 || r.model.history[0] != "queued one" {
		t.Fatalf("history should record the queued prompt, got %v", r.model.history)
	}
	// Queueing must not start a turn.
	select {
	case p := <-r.pending:
		t.Fatalf("queueing should not submit, got %q", p)
	default:
	}

	// Ctrl-C while busy clears the queue ("stop means stop").
	r.handleInterrupt()
	if len(r.model.queue) != 0 {
		t.Fatalf("interrupt should clear the queue, got %v", r.model.queue)
	}
}

func TestStartNextQueuedRunsPromptAfterCommand(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	r.model.queue = []string{"/help", "hello"}

	ran := make(chan string, 1)
	runTurn := func(ctx context.Context, prompt string, _ TurnUI) error {
		ran <- prompt
		return nil
	}

	done := r.startNextQueued(context.Background(), runTurn)
	if done == nil {
		t.Fatal("expected a turn to start for the queued prompt")
	}
	// The queued "/help" runs inline; the queued prompt then starts a turn.
	select {
	case p := <-ran:
		if p != "hello" {
			t.Fatalf("started turn for %q, want \"hello\"", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued prompt turn did not start")
	}
	if !r.model.busy {
		t.Fatal("beginTurn should mark the model busy")
	}
	if len(r.model.queue) != 0 {
		t.Fatalf("queue should be drained, got %v", r.model.queue)
	}
}

func TestStartNextQueuedEmptyReturnsNil(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	runTurn := func(context.Context, string, TurnUI) error { return nil }
	if done := r.startNextQueued(context.Background(), runTurn); done != nil {
		t.Fatal("empty queue should not start a turn")
	}
}

func TestHandleInterruptQuitsWhenIdle(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	if quit := r.handleInterrupt(); !quit {
		t.Fatal("interrupt at an idle prompt should quit")
	}
}

func TestBusyIndicatorShowsStateAndElapsed(t *testing.T) {
	// In quiet mode there is no status bar, so the inline busy row carries the
	// spinner + state + elapsed time.
	m := newReplModel()
	m.quiet = true
	m.busy = true
	m.state = turnStateTool
	m.toolName = "bash"
	m.turnStarted = time.Now().Add(-3 * time.Second)

	got := m.inputDisplay()
	if !strings.Contains(got, "running bash") {
		t.Fatalf("expected state label 'running bash', got %q", got)
	}
	if !strings.Contains(got, "3.0s") {
		t.Fatalf("expected elapsed '3.0s', got %q", got)
	}

	m.state = turnStateThinking
	if label := m.busyLabel(); label != "thinking" {
		t.Fatalf("thinking state label = %q", label)
	}
}

func TestBusyPromptStaysEditable(t *testing.T) {
	// Outside quiet mode the prompt stays editable while a turn runs (the
	// status-line spinner shows progress instead of overlaying the input).
	m := newReplModel()
	m.busy = true
	m.state = turnStateThinking
	m.turnStarted = time.Now()
	m.ed.setText("next message")

	text, _, _, _, editable := m.renderInput()
	if !editable {
		t.Fatal("prompt should stay editable while busy (non-quiet)")
	}
	if !strings.Contains(text, "next message") {
		t.Fatalf("busy input should show the editable draft, got %q", text)
	}
}

func TestStyleEscapeNeutralizesMarkup(t *testing.T) {
	// Lone/balanced brackets are left untouched (gotui renders them literally).
	if got := styleEscape("hello [world]"); got != "hello [world]" {
		t.Fatalf("balanced brackets must be untouched, got %q", got)
	}
	// The one style trigger, "](", is broken with a zero-width space — and no
	// backslashes are ever introduced (gotui renders them verbatim).
	got := styleEscape("see [text](url)")
	if strings.Contains(got, `\`) {
		t.Fatalf("must not introduce backslashes, got %q", got)
	}
	if got != "see [text]\u200b(url)" {
		t.Fatalf("expected ZWSP between ] and (, got %q", got)
	}
}

func TestApprovalPromptRendersLiteralBrackets(t *testing.T) {
	m := newReplModel()
	m.approval = &approvalState{calls: []messages.ChatMessageToolCall{{Name: "bash"}}}

	cells := ui.ParseStyles(m.inputDisplay(), ui.NewStyle(ui.ColorWhite))
	var b strings.Builder
	for _, c := range cells {
		b.WriteRune(c.Rune)
	}
	out := b.String()

	if !strings.Contains(out, "[y/n/a]") {
		t.Fatalf("approval prompt should render literal [y/n/a], got %q", out)
	}
	if strings.Contains(out, `\`) {
		t.Fatalf("approval prompt should not contain backslashes, got %q", out)
	}
}

func TestScrollBackHoldsAnchor(t *testing.T) {
	m := newReplModel()
	for i := 0; i < 20; i++ {
		m.appendLine("line " + string(rune('a'+i)))
	}
	if !m.followBottom {
		t.Fatal("expected followBottom true initially")
	}

	// Viewport = 5 lines. Scrolling up should pin the anchor and disable
	// follow.
	m.scrollBy(-3, 5)
	if m.followBottom {
		t.Fatal("scrollBy(-3) should disable followBottom")
	}
	wantAnchor := 20 - 5 - 3 // total - viewport - delta
	if m.scrollAnchor != wantAnchor {
		t.Fatalf("scrollAnchor = %d, want %d", m.scrollAnchor, wantAnchor)
	}

	// New content arrives while user is scrolled up — anchor must not jump.
	m.appendLine("new line")
	if m.scrollAnchor != wantAnchor {
		t.Fatalf("appendLine moved anchor: got %d", m.scrollAnchor)
	}

	// Scrolling back down re-engages follow.
	m.scrollBy(100, 5)
	if !m.followBottom {
		t.Fatalf("scrolling past bottom should re-engage follow")
	}
}

func TestVisibleTranscriptFollowsBottom(t *testing.T) {
	m := newReplModel()
	for i := 0; i < 10; i++ {
		m.appendLine(fmt.Sprintf("line %d", i))
	}
	got := m.visibleTranscript(3)
	want := "line 7\nline 8\nline 9"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestStatusRowDropsLowPriorityFields(t *testing.T) {
	m := newReplModel()
	m.modelName = "gpt-extra-long-name"
	m.contextName = "my-context"
	m.toolCount = 4
	m.skillCount = 2
	m.lastIn = 1234
	m.lastOut = 567

	wide := m.statusRow(200)
	if !strings.Contains(wide, "skills:2") || !strings.Contains(wide, "tools:4") {
		t.Fatalf("wide bar should include all fields: %q", wide)
	}
	// Tokens moved to the post-turn summary line; the bar never shows them.
	if strings.Contains(wide, "1.2k") || strings.Contains(wide, "→") {
		t.Fatalf("status bar should not carry token counts: %q", wide)
	}

	narrow := m.statusRow(20)
	if strings.Contains(narrow, "skills:2") {
		t.Fatalf("narrow bar should drop higher-priority fields before context: %q", narrow)
	}
	if !strings.Contains(narrow, "my-context") {
		t.Fatalf("narrow bar should keep the context (drop:0) field: %q", narrow)
	}
}

func TestTurnSummaryLine(t *testing.T) {
	line := turnSummaryLine(15500*time.Millisecond, 1234, 567)
	for _, want := range []string{"15.5s", "1234 in", "567 out"} {
		if !strings.Contains(line, want) {
			t.Errorf("turn summary %q missing %q", line, want)
		}
	}
	// The whole line is muted metadata, not an accent.
	if !strings.Contains(line, "](fg:muted)") {
		t.Errorf("turn summary should be muted: %q", line)
	}
}

func TestHumanizeTokens(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{0, "0"},
		{999, "999"},
		{1234, "1.2k"},
		{99999, "99.9k"},
		{100000, "100k"},
		{1_500_000, "1.5M"},
	}
	for _, tc := range cases {
		if got := humanizeTokens(tc.n); got != tc.want {
			t.Errorf("humanizeTokens(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestFormatElapsed(t *testing.T) {
	if got := formatElapsed(2300 * time.Millisecond); got != "2.3s" {
		t.Errorf("got %q", got)
	}
	if got := formatElapsed(65 * time.Second); got != "1m05s" {
		t.Errorf("got %q", got)
	}
}

func TestRunREPLLoopExitOnEOF(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader("hi\n"))
	var out bytes.Buffer
	calls := 0
	err := runREPLLoop(reader, &out, func(prompt string) error {
		calls++
		if prompt != "hi" {
			return errors.New("unexpected prompt: " + prompt)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected one turn call, got %d", calls)
	}
	if !strings.Contains(out.String(), "> ") {
		t.Fatalf("prompt prefix not written: %q", out.String())
	}
}

func TestRunREPLLoopHandlesExitCommand(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader("/exit\n"))
	err := runREPLLoop(reader, &bytes.Buffer{}, func(prompt string) error {
		t.Fatalf("runTurn should not be called for /exit")
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunREPLLoopSkipsBlankLines(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader("\n   \nask\n"))
	calls := 0
	err := runREPLLoop(reader, &bytes.Buffer{}, func(prompt string) error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected one turn, got %d", calls)
	}
}
