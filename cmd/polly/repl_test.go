package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
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

func TestReplModelHistoryFrozenWhileBusy(t *testing.T) {
	m := newReplModel()
	m.history = []string{"one"}
	m.busy = true
	m.historyUp()
	if !m.ed.empty() {
		t.Fatalf("history nav should be a no-op while busy, got %q", m.ed.text())
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

func TestHandleInterruptQuitsWhenIdle(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	if quit := r.handleInterrupt(); !quit {
		t.Fatal("interrupt at an idle prompt should quit")
	}
}

func TestBusyIndicatorShowsStateAndElapsed(t *testing.T) {
	m := newReplModel()
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

	narrow := m.statusRow(30)
	if strings.Contains(narrow, "skills:2") {
		t.Fatalf("narrow bar should drop skills first: %q", narrow)
	}
	if !strings.Contains(narrow, "my-context") || !strings.Contains(narrow, "1.2k") {
		t.Fatalf("narrow bar should keep context and tokens: %q", narrow)
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
