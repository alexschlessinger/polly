package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/alexschlessinger/pollytool/messages"
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

func TestReplModelHistoryNavigation(t *testing.T) {
	m := newReplModel()
	m.hist.entries = []string{"first", "second"}
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
	m.hist.entries = []string{"one"}
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
	if m.transcript[0].text != "Hello world" {
		t.Fatalf("got %q", m.transcript[0].text)
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
	m.hist.entries = []string{"old one", "old two"}
	m.hist.idx = -1
	m.ed.setText("first\nsecond") // cursor at end (line 1)

	up := ui.Event{Type: ui.KeyboardEvent, ID: "<Up>"}

	// First Up moves within the buffer to the first line — history untouched.
	r.handleEvent(up)
	if m.ed.text() != "first\nsecond" {
		t.Fatalf("Up within buffer altered text: %q", m.ed.text())
	}
	if m.hist.idx != -1 {
		t.Fatalf("Up within buffer touched history (idx %d)", m.hist.idx)
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

func TestToolErrorLineRendering(t *testing.T) {
	line := toolErrorLine("bash", "1.4s", "exit 1")

	if strings.Contains(line, "\n") {
		t.Fatalf("error line should not wrap into multiple rows: %q", line)
	}
	for _, want := range []string{"bash", "1.4s", "exit 1"} {
		if !strings.Contains(line, want) {
			t.Errorf("error line %q missing %q", line, want)
		}
	}
	// Display is only ✗, time, command, and exit code — no tool output.
	for _, unwanted := range []string{"line one", "fatal: the real error"} {
		if strings.Contains(line, unwanted) {
			t.Errorf("error line should not include %q: %q", unwanted, line)
		}
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

	// Starting a tool adds one collapsed disclosure and tracks its semantic row.
	m.appendToolStartLine("id1", "bash sleep 30")
	if len(m.transcript) != 1 || len(m.activeTools) != 1 {
		t.Fatalf("after start: %d transcript, %d active", len(m.transcript), len(m.activeTools))
	}
	if m.activeTools[0].row != 0 {
		t.Fatalf("active tool row = %d, want 0", m.activeTools[0].row)
	}
	record := m.currentToolDisclosure()
	if record == nil || record.expanded || len(record.rows) != 1 || !strings.Contains(m.transcript[0].text, "1 tool") {
		t.Fatalf("started disclosure = %#v transcript=%#v", record, m.transcript)
	}

	// A render frame updates the hidden semantic row without leaking its timer.
	m.activeTools[0].started = m.activeTools[0].started.Add(-15500 * time.Millisecond)
	m.refreshActiveTools()
	if strings.Contains(m.transcript[0].text, "15.5s") || !strings.Contains(record.rows[0].line, "15.5s") {
		t.Fatalf("collapsed timer update: row=%q transcript=%q", record.rows[0].line, m.transcript[0].text)
	}
	if !m.toggleToolDisclosure(record.id) || !strings.Contains(m.transcript[0].text, "15.5s") {
		t.Fatalf("expanded disclosure missing elapsed: %q", m.transcript[0].text)
	}

	// Finishing frees the active slot and freezes the same semantic row.
	row, ok := m.takeActiveTool("id1")
	if !ok || row != 0 {
		t.Fatalf("takeActiveTool = (%d, %v), want (0, true)", row, ok)
	}
	if len(m.activeTools) != 0 {
		t.Fatalf("active tools should be empty, got %d", len(m.activeTools))
	}
	record.rows[row].line = toolOKLine("bash sleep 30", "30.0s", "")
	record.rows[row].settled = true
	m.refreshToolDisclosure(record)
	if len(m.transcript) != 1 || !strings.Contains(m.transcript[0].text, "✓") {
		t.Fatalf("finalized transcript = %v", m.transcript)
	}
}

func TestTakeActiveToolMatchesByIDWithFallback(t *testing.T) {
	m := newReplModel()
	m.appendToolStartLine("a", "bash one") // row 0
	m.appendToolStartLine("b", "bash two") // row 1

	// Out-of-order finish: the second tool's id resolves to its own row.
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

func TestStatusRowShowsLiveTimer(t *testing.T) {
	// While a turn runs, the elapsed timer sits at the far left of the status
	// row; the static context fields stay right-aligned and unchanged.
	plain := func(s string) string {
		var b strings.Builder
		for _, c := range ui.ParseStyles(s, ui.NewStyle(ui.ColorWhite)) {
			b.WriteRune(c.Rune)
		}
		return b.String()
	}

	m := newReplModel()
	m.status.modelName = "gpt-mini"
	m.status.contextName = "ctx"

	idle := plain(m.statusRow(120))
	if strings.HasPrefix(strings.TrimSpace(idle), "0.0s") {
		t.Fatalf("idle status row should not show a timer: %q", idle)
	}

	m.busy = true
	m.turnStarted = time.Now()
	for _, st := range []turnState{turnStateWaiting, turnStateThinking, turnStateStreaming, turnStateTool} {
		m.state = st
		row := plain(m.statusRow(120))
		if !strings.HasPrefix(row, "0.0s") {
			t.Fatalf("busy status row for state %v = %q, want timer at left", st, row)
		}
		if !strings.Contains(row, "gpt-mini") || !strings.Contains(row, "ctx") {
			t.Fatalf("busy status row for state %v lost static fields: %q", st, row)
		}
	}
}

func TestStatusRowKeepsStaticFieldsFixed(t *testing.T) {
	// Static context stays right-aligned across idle and active states, so the
	// model/context do not jump horizontally when a completion settles.
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
	m.status.modelName = "gpt-mini"
	m.status.contextName = "ctx"

	idleCol := col(m.statusRow(120))

	m.busy = true
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
	if idleCol != busyCol {
		t.Fatalf("static fields shifted at completion: idle %d, busy %d", idleCol, busyCol)
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
		// "/c" matches /clear, /close, and /context; common prefix extends to "/c".
		{"/c", true, "/c", []string{"/clear", "/close", "/context"}},
		{"/cl", true, "/cl", []string{"/clear", "/close"}},
		{"/cle", true, "/clear", []string{"/clear"}},
		// "/t" matches the tab commands and /tools; "/to" is /tools alone.
		{"/t", true, "/t", []string{"/tab", "/tabs", "/tools"}},
		{"/to", true, "/tools", []string{"/tools"}},
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
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Tab>"})
	if got := r.model.ed.text(); got != "/" {
		t.Fatalf("after Tab on /, input = %q, want / unchanged", got)
	}
	if r.model.slashHints == "" || !strings.Contains(r.model.slashHints, "/help") {
		t.Fatalf("expected transient slash hints, got %q", r.model.slashHints)
	}
	if len(r.model.transcript) != 0 {
		t.Fatalf("slash hints should not append transcript lines, got %v", r.model.transcript)
	}

	// Non-slash text: Tab inserts a literal tab (legacy behavior).
	r.model.ed.setText("ab")
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Tab>"})
	if got := r.model.ed.text(); got != "ab\t" {
		t.Fatalf("after Tab on ab, input = %q, want ab\\t", got)
	}
}

func TestHandleEventSlashListsCommandsOnInsert(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	send := func(id string) { r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: id}) }

	send("/")
	if got := r.model.ed.text(); got != "/" {
		t.Fatalf("after typing /, input = %q, want /", got)
	}
	if r.model.slashHints == "" || !strings.Contains(r.model.slashHints, "/help") {
		t.Fatalf("typing / should set transient command hints, got %q", r.model.slashHints)
	}
	if len(r.model.transcript) != 0 {
		t.Fatalf("typing / should not append transcript lines, got %v", r.model.transcript)
	}
	if got := r.model.visibleTranscript(3); !strings.Contains(got, "/help") {
		t.Fatalf("visible transcript = %q, want transient command list", got)
	}

	r.model.ed.setText("ask")
	send("/")
	if got := r.model.ed.text(); got != "ask/" {
		t.Fatalf("after typing / mid-input, input = %q, want ask/", got)
	}
	if r.model.slashHints != "" {
		t.Fatalf("typing / mid-input should clear slash hints, got %q", r.model.slashHints)
	}
	if len(r.model.transcript) != 0 {
		t.Fatalf("typing / mid-input should not list commands, got %v", r.model.transcript)
	}
}

func TestSlashHintsClearOnBackspaceEnterAndHistory(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	send := func(id string) { r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: id}) }

	send("/")
	send("<Backspace>")
	if got := r.model.ed.text(); got != "" {
		t.Fatalf("after backspacing /, input = %q, want empty", got)
	}
	if r.model.slashHints != "" {
		t.Fatalf("slash hints should clear after backspace, got %q", r.model.slashHints)
	}
	if got := r.model.visibleTranscript(3); got != "" {
		t.Fatalf("visible transcript should drop cleared slash hints, got %q", got)
	}

	send("/")
	send("<Enter>")
	if r.model.slashHints != "" {
		t.Fatalf("slash hints should clear after slash command submit, got %q", r.model.slashHints)
	}

	r.model.ed.clear()
	clearTranscriptForTest(r.model)
	r.model.visual.invalidate()
	r.model.hist.entries = []string{"hello"}
	send("/")
	send("<Up>")
	if got := r.model.ed.text(); got != "hello" {
		t.Fatalf("history recall = %q, want hello", got)
	}
	if r.model.slashHints != "" {
		t.Fatalf("slash hints should clear after history recall, got %q", r.model.slashHints)
	}
}

func TestSlashHintsLiveFilterAndEscape(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	send := func(id string) { r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: id}) }

	// Hints narrow with each typed rune, no Tab required.
	send("/")
	if r.model.slashHints == "" || !strings.Contains(r.model.slashHints, "/help") {
		t.Fatalf("typing / should show all commands, got %q", r.model.slashHints)
	}
	send("t")
	if got := r.model.slashHints; !strings.Contains(got, "/tools") || strings.Contains(got, "/thinking") {
		t.Fatalf("typing /t should narrow hints to /tools, got %q", got)
	}

	// Escape hides the line without touching the input…
	send("<Escape>")
	if got := r.model.ed.text(); got != "/t" {
		t.Fatalf("escape changed input to %q", got)
	}
	if r.model.slashHints != "" {
		t.Fatalf("escape should hide slash hints, got %q", r.model.slashHints)
	}

	// …and the next edit brings it back.
	send("o")
	if got := r.model.slashHints; !strings.Contains(got, "/tools") {
		t.Fatalf("typing after escape should re-show hints, got %q", got)
	}

	// Argument values hint once the command name is complete.
	r.model.ed.setText("/set thinking")
	send(" ")
	if got := r.model.slashHints; !strings.Contains(got, "dynamic") || !strings.Contains(got, "medium") {
		t.Fatalf("/set thinking␣ should hint values, got %q", got)
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
		t.Fatalf("paste should not submit, got %q", p.displayText)
	default:
	}

	// A real Enter now submits the whole multi-line prompt.
	send("<Enter>")
	select {
	case p := <-r.pending:
		if p.displayText != "a\nb" {
			t.Fatalf("submitted %q, want \"a\\nb\"", p.displayText)
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

func TestRenderInputHonorsSmallerRowBudget(t *testing.T) {
	m := newReplModel()
	m.ed.setText("L0\nL1\nL2\nL3")

	text, rows, curRow, _, _ := m.renderInputWithMaxRows(2)
	if rows != 2 {
		t.Fatalf("rows = %d, want 2", rows)
	}
	if curRow != 1 {
		t.Fatalf("curRow = %d, want 1", curRow)
	}
	if !strings.Contains(text, "L3") || strings.Contains(text, "L0") {
		t.Fatalf("input should be capped to the bottom two lines, got %q", text)
	}
}

func TestRenderInputHorizontallyFollowsCursor(t *testing.T) {
	m := newReplModel()
	m.ed.setText("0123456789abcdef")

	text, _, curCol, _ := m.renderInputForTerminal(maxInputRows, 8)
	if !strings.Contains(text, "abcdef") || strings.Contains(text, "0123") {
		t.Fatalf("long input should show the cursor-side tail, got %q", text)
	}
	if curCol != 7 {
		t.Fatalf("curCol = %d, want clamped right edge 7", curCol)
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
	r.model.hist.entries = []string{"git status", "go build", "git commit", "go test"}
	send := func(id string) { r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: id}) }

	send("<C-r>")
	if !r.model.hist.searching {
		t.Fatal("Ctrl-R did not enter search")
	}

	// Typing "git" matches the most recent entry containing it.
	send("g")
	send("i")
	send("t")
	if r.model.hist.match < 0 || r.model.hist.entries[r.model.hist.match] != "git commit" {
		t.Fatalf("first match = %d, want index of \"git commit\"", r.model.hist.match)
	}

	// Repeated Ctrl-R steps to the older match.
	send("<C-r>")
	if r.model.hist.entries[r.model.hist.match] != "git status" {
		t.Fatalf("stepped match = %q, want \"git status\"", r.model.hist.entries[r.model.hist.match])
	}

	// Enter accepts the match into the editor and leaves search (no submit).
	send("<Enter>")
	if r.model.hist.searching {
		t.Fatal("Enter did not exit search")
	}
	if r.model.ed.text() != "git status" {
		t.Fatalf("accepted text = %q", r.model.ed.text())
	}
}

func TestReverseSearchCancelKeepsDraft(t *testing.T) {
	r := &managedREPL{model: newReplModel()}
	r.model.hist.entries = []string{"alpha", "beta"}
	r.model.ed.setText("draft")

	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-r>"})
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "a"})

	// Ctrl-C cancels the search instead of quitting the REPL.
	if quit := r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-c>"}); quit {
		t.Fatal("Ctrl-C during search should cancel, not quit")
	}
	if r.model.hist.searching {
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

func TestPersistentHistoryFiltersAttachmentTokens(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "legacy")
	if err := os.WriteFile(legacy, []byte("safe\ninspect [image #7]\nafter\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := loadHistory(legacy); strings.Join(got, ",") != "safe,after" {
		t.Fatalf("legacy tokenized history was not filtered: %v", got)
	}

	path := filepath.Join(dir, "current")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	r.histFile = file
	r.recordAcceptedInput("inspect [image #1]")
	r.recordAcceptedInput("plain prompt")
	r.closeHistory()

	if got := r.model.hist.entries; len(got) != 2 || got[0] != "inspect [image #1]" {
		t.Fatalf("same-process recall lost tokenized input: %v", got)
	}
	if got := loadHistory(path); len(got) != 1 || got[0] != "plain prompt" {
		t.Fatalf("tokenized input leaked into persistent history: %v", got)
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

	if len(r.model.hist.entries) != maxPersistedHistory {
		t.Fatalf("loaded %d entries, want %d", len(r.model.hist.entries), maxPersistedHistory)
	}
	if last := r.model.hist.entries[len(r.model.hist.entries)-1]; last != fmt.Sprintf("cmd%d", maxPersistedHistory+9) {
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
	store := testOpenMemoryStore(t, nil)
	session := testAcquireSession(t, store, "ctx-test")
	testAddMessage(t, session, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "hi"})
	testAddMessage(t, session, messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "hello"})

	r := newManagedREPL(&Config{}, "ctx-test", 0, 0)
	r.state = &conversationState{session: session, toolRegistry: tools.NewToolRegistry(nil), settings: Settings{MaxHistoryTokens: 5678}}

	// /context reports the session name and message stats.
	if handled, quit := r.runCommand("/context"); !handled || quit {
		t.Fatalf("/context handled=%v quit=%v", handled, quit)
	}
	if joined := strings.Join(transcriptTexts(r.model), "\n"); !strings.Contains(joined, "ctx-test") || !strings.Contains(joined, "messages:") ||
		!strings.Contains(joined, "transcript:") || !strings.Contains(joined, "(durable)") || !strings.Contains(joined, "model budget: 5.6k") {
		t.Fatalf("/context output missing fields: %q", joined)
	}

	// /clear is display-only: durable history remains intact.
	r.runCommand("/clear")
	if got := len(testSessionHistory(t, session)); got != 2 {
		t.Fatalf("/clear changed durable history; got %d messages", got)
	}
	if len(r.model.transcript) != 1 || !strings.Contains(r.model.transcript[0].text, "cleared") {
		t.Fatalf("/clear transcript = %v", r.model.transcript)
	}

	// /reset requires the literal confirmation token, then clears persistence.
	r.runCommand("/reset")
	if got := len(testSessionHistory(t, session)); got != 2 {
		t.Fatalf("unconfirmed /reset changed history; got %d messages", got)
	}
	r.runCommand("/reset confirm")
	if got := len(testSessionHistory(t, session)); got != 0 {
		t.Fatalf("/reset confirm left %d messages", got)
	}

	// /tools on an empty registry reports none.
	clearTranscriptForTest(r.model)
	r.runCommand("/tools")
	if !strings.Contains(strings.Join(transcriptTexts(r.model), "\n"), "no tools loaded") {
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
	joined := strings.Join(transcriptTexts(m), "\n")
	if !strings.Contains(joined, "commands:") || !strings.Contains(joined, "Ctrl-C") {
		t.Fatalf("transcript missing help content: %q", joined)
	}
}

func TestRunREPLLoopShowsHelp(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader("/help\n"))
	var out bytes.Buffer
	err := runREPLLoop(context.Background(), reader, &out, func(prompt string) error {
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

func TestMouseLeftOpensThumbnailUnderCursor(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	var opened []string
	r.openImage = func(path string) error { opened = append(opened, path); return nil }
	r.model.imagePlacements = []terminalImagePlacement{
		{Key: "logo", Embedded: "logo", X: 0, Y: 0, Cols: 10, Rows: 5},
		{Key: "b:image:0", Path: "/tmp/pic.png", X: 4, Y: 6, Cols: 20, Rows: 8},
	}
	click := func(x, y int) {
		r.handleEvent(ui.Event{Type: ui.MouseEvent, ID: "<MouseLeft>", Payload: ui.Mouse{X: x, Y: y}})
	}

	click(3, 6)  // left of the thumbnail
	click(4, 14) // one row below it (Y+Rows is exclusive)
	if len(opened) != 0 {
		t.Fatalf("misses should not open anything, got %v", opened)
	}
	click(2, 2) // splash logo has no backing file
	if len(opened) != 0 {
		t.Fatalf("logo click should not open anything, got %v", opened)
	}
	click(4, 6)   // top-left corner
	click(23, 13) // bottom-right corner
	if len(opened) != 2 || opened[0] != "/tmp/pic.png" || opened[1] != "/tmp/pic.png" {
		t.Fatalf("clicks inside the thumbnail should open it, got %v", opened)
	}
}

func TestEscapeCancelsTurnButNeverQuits(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	canceled := false
	r.turnCancel = func() { canceled = true }
	r.model.busy = true
	r.model.state = turnStateStreaming
	esc := ui.Event{Type: ui.KeyboardEvent, ID: "<Escape>"}

	// Escape during a turn cancels it like Ctrl-C.
	if quit := r.handleEvent(esc); quit {
		t.Fatal("escape should not quit")
	}
	if !canceled {
		t.Fatal("escape should cancel the turn context")
	}
	if !r.model.canceling {
		t.Fatal("escape should mark canceling")
	}

	// A second Escape while winding down is a no-op, never a quit.
	if quit := r.handleEvent(esc); quit {
		t.Fatal("second escape should not quit")
	}

	// Escape at an idle prompt does nothing.
	r.model.busy = false
	r.model.canceling = false
	r.model.ed.setText("draft")
	if quit := r.handleEvent(esc); quit {
		t.Fatal("escape at idle should not quit")
	}
	if got := r.model.ed.text(); got != "draft" {
		t.Fatalf("escape at idle should keep the draft, got %q", got)
	}
}

func TestAbandonCanceledTurnRestoresPromptAndInvalidatesCallbacks(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.busy = true
	m.canceling = true
	m.state = turnStateTool
	m.toolName = "bash"
	m.turnStarted = time.Now()
	m.turnID = 7
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config, turnID: 7}

	r.abandonCanceledTurn()
	if m.busy || m.canceling {
		t.Fatalf("abandoned turn should restore idle prompt, busy=%v canceling=%v", m.busy, m.canceling)
	}
	if m.state != turnStateIdle {
		t.Fatalf("state = %v, want idle", m.state)
	}
	if m.turnID != 8 {
		t.Fatalf("turnID = %d, want stale callbacks invalidated to 8", m.turnID)
	}

	tui.AppendAssistantText("late text")
	tui.AppendWarning("late warning")
	tui.RecordTurnTokens(1, 2)
	if strings.Contains(strings.Join(transcriptTexts(m), "\n"), "late") {
		t.Fatalf("stale turn UI callback mutated transcript: %v", m.transcript)
	}
	if m.lastIn != 0 || m.lastOut != 0 {
		t.Fatalf("stale token callback mutated counts: %d/%d", m.lastIn, m.lastOut)
	}
}

func TestGotuiTurnUIDeniesApprovalWhileCanceling(t *testing.T) {
	r := newManagedREPL(&Config{Confirm: false}, "ctx", 0, 0)
	r.model.turnID = 1
	r.model.canceling = true
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config, turnID: 1}

	got := tui.ApproveToolCalls([]messages.ChatMessageToolCall{{Name: "bash"}})
	if len(got) != 1 || got[0] {
		t.Fatalf("canceling turn should deny tool approval, got %v", got)
	}
}

func TestEnterSlashCommandRecordsHistoryAndPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hist")
	t.Setenv("POLLY_HISTORY_FILE", path)

	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	r.initHistory()

	r.model.ed.setText("/help")
	if quit := r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"}); quit {
		t.Fatal("/help should not quit")
	}
	if got := r.model.ed.text(); got != "" {
		t.Fatalf("editor should clear after command submit, got %q", got)
	}
	if len(r.model.hist.entries) != 1 || r.model.hist.entries[0] != "/help" {
		t.Fatalf("slash command history = %v, want [/help]", r.model.hist.entries)
	}
	r.model.historyUp()
	if got := r.model.ed.text(); got != "/help" {
		t.Fatalf("history recall = %q, want /help", got)
	}

	r.closeHistory()
	if got := loadHistory(path); len(got) != 1 || got[0] != "/help" {
		t.Fatalf("persisted slash command history = %v, want [/help]", got)
	}
}

func TestEnterWhileBusyQueues(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	send := func(id string) { r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: id}) }

	r.model.beginTurn("current")
	r.model.ed.setText("queued one")
	send("<Enter>")

	if r.model.ed.text() != "" {
		t.Fatalf("editor should clear after queueing, got %q", r.model.ed.text())
	}
	if len(r.model.queue) != 1 || r.model.queue[0].text != "queued one" || r.model.queue[0].turn == nil {
		t.Fatalf("queue = %v, want [\"queued one\"]", r.model.queue)
	}
	if len(r.model.hist.entries) != 1 || r.model.hist.entries[0] != "queued one" {
		t.Fatalf("history should record the queued prompt, got %v", r.model.hist.entries)
	}
	// Queueing must not start a turn.
	select {
	case p := <-r.pending:
		t.Fatalf("queueing should not submit, got %q", p.displayText)
	default:
	}

	// A canceled turn leaves pending entries visibly unsent and clears the
	// internal queue. The prompt remains recoverable from input history.
	r.handleInterrupt()
	r.endTurn(context.Canceled)
	if len(r.model.queue) != 0 {
		t.Fatalf("canceled turn should clear pending queue, got %v", r.model.queue)
	}
	if got := plainStyledText(r.model.fullTranscript()); !strings.Contains(got, "> queued one\n  (not sent)") {
		t.Fatalf("canceled queued entry was not marked unsent: %q", got)
	}
}

func TestEnterWhileBusyQueuesSlashCommandInHistory(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	send := func(id string) { r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: id}) }

	// A mutating command queues behind the running turn (busy-safe read-only
	// commands run immediately instead; see TestBusyReadOnlyCommandsRunImmediately).
	r.model.busy = true
	r.model.ed.setText("/reset confirm")
	send("<Enter>")

	if r.model.ed.text() != "" {
		t.Fatalf("editor should clear after queueing, got %q", r.model.ed.text())
	}
	if len(r.model.queue) != 1 || r.model.queue[0].text != "/reset confirm" || r.model.queue[0].turn != nil {
		t.Fatalf("queue = %v, want [/reset confirm]", r.model.queue)
	}
	if len(r.model.hist.entries) != 1 || r.model.hist.entries[0] != "/reset confirm" {
		t.Fatalf("history should record the queued slash command, got %v", r.model.hist.entries)
	}
	select {
	case p := <-r.pending:
		t.Fatalf("queueing slash command should not submit, got %q", p.displayText)
	default:
	}
}

func TestStartNextQueuedRunsPromptAfterCommand(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	r.model.queue = queuedTextInputs("/help", "hello")

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

func TestQuietBusyPromptStaysEditable(t *testing.T) {
	m := newReplModel()
	m.quiet = true
	m.busy = true
	m.state = turnStateTool
	m.toolName = "bash"
	m.turnStarted = time.Now().Add(-3 * time.Second)
	m.ed.setText("compose next")

	got, _, _, _, editable := m.renderInput()
	if !editable || !strings.Contains(got, "compose next") {
		t.Fatalf("quiet busy composer should remain visible and editable, got %q editable=%v", got, editable)
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

func TestAppendAssistantRendersStreamedLink(t *testing.T) {
	m := newReplModel()
	m.appendAssistant("see [text]")

	// The bracket may still become a link \u2014 held back, only settled text shows.
	if got := plainStyledText(m.transcript[0].text); got != "see" {
		t.Fatalf("partial link should be held back, got %q", got)
	}

	m.appendAssistant("(url)")
	if got := plainStyledText(m.transcript[0].text); got != "see text (url)" {
		t.Fatalf("completed link render = %q, want %q", got, "see text (url)")
	}
	if !strings.Contains(m.transcript[0].text, "fg:accent") {
		t.Fatalf("link text should carry the accent color: %q", m.transcript[0].text)
	}
}

func plainStyledText(s string) string {
	var rendered strings.Builder
	for _, c := range parseStyledCells(s, ui.NewStyle(ui.ColorWhite)) {
		if c.Rune != '\u200b' {
			rendered.WriteRune(c.Rune)
		}
	}
	return rendered.String()
}

func TestAppendAssistantRendersStreamingCodeFenceWithLanguage(t *testing.T) {
	m := newReplModel()
	m.appendAssistant("before\n```go\n")
	m.appendAssistant("totalIn  int  // cumulative input tokens this session\n")
	m.appendAssistant("totalOut int  // cumulative output tokens this session\n```\nafter")

	got := plainStyledText(m.transcript[0].text)
	want := "before\n\n╭─ go\n│ totalIn  int  // cumulative input tokens this session\n│ totalOut int  // cumulative output tokens this session\n\nafter"
	if got != want {
		t.Fatalf("rendered code fence = %q, want %q", got, want)
	}
}

func TestAppendAssistantRendersBareCodeFenceAcrossChunks(t *testing.T) {
	m := newReplModel()
	m.appendAssistant("```")
	m.appendAssistant("\n")
	m.appendAssistant("fmt.Println([text]")
	m.appendAssistant("(url))\n``")
	m.appendAssistant("`\n")

	got := plainStyledText(m.transcript[0].text)
	want := "│ fmt.Println([text](url))"
	if got != want {
		t.Fatalf("rendered bare code fence = %q, want %q", got, want)
	}
}

func TestFinishAssistantStreamFlushesPendingFenceLine(t *testing.T) {
	m := newReplModel()
	m.appendAssistant("```go\nx\n```")
	m.finishAssistantStream()

	got := plainStyledText(m.transcript[0].text)
	want := "╭─ go\n│ x"
	if got != want {
		t.Fatalf("finished code fence = %q, want %q", got, want)
	}
}

func TestTranscriptParagraphPinsWrappedOverflowToBottom(t *testing.T) {
	p := newTranscriptParagraph()
	noBorder(&p.Block)
	p.Text = "abcdefghijkl"
	p.SetRect(0, 0, 4, 2)

	buf := ui.NewBuffer(image.Rect(0, 0, 4, 2))
	p.Draw(buf)

	line := func(y int) string {
		var b strings.Builder
		for x := 0; x < 4; x++ {
			b.WriteRune(buf.GetCell(image.Pt(x, y)).Rune)
		}
		return b.String()
	}
	if got := line(0) + "\n" + line(1); got != "efgh\nijkl" {
		t.Fatalf("bottom-pinned wrapped rows = %q, want last two rows", got)
	}

	p.PinBottom = false
	buf = ui.NewBuffer(image.Rect(0, 0, 4, 2))
	p.Draw(buf)
	if got := line(0) + "\n" + line(1); got != "abcd\nefgh" {
		t.Fatalf("top-clipped wrapped rows = %q, want first two rows", got)
	}
}

func TestTranscriptParagraphDrawsMultiRowOverlayWithoutReflow(t *testing.T) {
	p := newTranscriptParagraph()
	noBorder(&p.Block)
	p.Rows = [][]ui.Cell{
		{{Rune: 'a'}}, {{Rune: 'b'}}, {{Rune: 'c'}}, {{Rune: 'd'}},
	}
	p.UseRows = true
	p.PinBottom = false
	p.OverlayBottom = [][]ui.Cell{{{Rune: 'x'}}, {{Rune: 'y'}}}
	p.SetRect(0, 0, 2, 4)

	buf := ui.NewBuffer(image.Rect(0, 0, 2, 4))
	p.Draw(buf)
	var got strings.Builder
	for y := 0; y < 4; y++ {
		got.WriteRune(buf.GetCell(image.Pt(0, y)).Rune)
	}
	if got.String() != "abxy" {
		t.Fatalf("overlay moved transcript rows instead of covering the bottom: %q", got.String())
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

	if !strings.Contains(out, "[y]es [N]o [a]ll") {
		t.Fatalf("approval prompt should render explicit actions, got %q", out)
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
	m.status.modelName = "gpt-extra-long-name"
	m.status.contextName = "my-context"
	m.status.toolCount = 4
	m.status.skillCount = 2
	m.lastIn = 1234
	m.lastOut = 567

	wide := m.statusRow(200)
	if !strings.Contains(wide, "skills:2") {
		t.Fatalf("wide bar should include all fields: %q", wide)
	}
	if strings.Contains(wide, "tools:4") {
		t.Fatalf("tool count is not shown in the bar: %q", wide)
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

func TestCompletedTurnMetricsMoveFromStatusToDock(t *testing.T) {
	m := newReplModel()
	m.status.contextName = "ctx"
	m.startTurnDock()
	m.lastOutcome = turnOutcomeDone
	m.lastElapsed = 15500 * time.Millisecond
	m.lastIn = 1234
	m.lastOut = 567
	m.settleTurnDock()
	status := plainStyledText(m.statusRow(200))
	if strings.Contains(status, "done") || strings.Contains(status, "15.5s") || strings.Contains(status, "tok") {
		t.Fatalf("status bar retained per-turn metrics: %q", status)
	}
	dock, _ := m.turnDockRow(200)
	plain := plainStyledText(dock)
	for _, want := range []string{"✓", "15.5s", "1.2k in / 567 out"} {
		if !strings.Contains(plain, want) {
			t.Errorf("completed dock %q missing %q", plain, want)
		}
	}
	if len(m.transcript) != 0 {
		t.Fatalf("settling the dock must not add transcript rows: %v", m.transcript)
	}
}

func TestTurnTokensStayPerTurnInAttachedTrailers(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model

	m.beginTurn("first")
	first := &gotuiTurnUI{repl: r, model: r.model, config: r.config, turnID: m.turnID}
	first.RecordTurnTokens(1000, 250)
	first.RecordTurnTokens(1200, 300)
	r.endTurn(nil)
	firstTrailer := m.turnTrailers[m.turnTrailerSeq]
	if got := plainStyledText(m.transcript[firstTrailer.transcriptIndex].text); !strings.Contains(got, "1.2k in / 300 out") {
		t.Fatalf("first completed trailer = %q", got)
	}

	m.beginTurn("second")
	second := &gotuiTurnUI{repl: r, model: r.model, config: r.config, turnID: m.turnID}
	second.RecordTurnTokens(800, 200)
	r.endTurn(nil)

	secondTrailer := m.turnTrailers[m.turnTrailerSeq]
	if got := plainStyledText(m.transcript[secondTrailer.transcriptIndex].text); !strings.Contains(got, "800 in / 200 out") || strings.Contains(got, "2.0k") {
		t.Fatalf("second completed trailer should show only this turn, got %q", got)
	}
	history := plainStyledText(strings.Join(transcriptTexts(m), "\n"))
	if !strings.Contains(history, "1.2k in / 300 out") {
		t.Fatalf("first attached trailer missing from transcript history: %q", history)
	}
	if m.lastIn != 800 || m.lastOut != 200 {
		t.Fatalf("last turn tokens = %d/%d, want 800/200", m.lastIn, m.lastOut)
	}
}

func TestTruncatePreservesUTF8(t *testing.T) {
	got := truncate(strings.Repeat("é", 20), 10)
	if !utf8.ValidString(got) {
		t.Fatalf("truncate split a UTF-8 sequence: %q", got)
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("truncated string should carry an ellipsis tail, got %q", got)
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
	err := runREPLLoop(context.Background(), reader, &out, func(prompt string) error {
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
	err := runREPLLoop(context.Background(), reader, &bytes.Buffer{}, func(prompt string) error {
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
	err := runREPLLoop(context.Background(), reader, &bytes.Buffer{}, func(prompt string) error {
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
