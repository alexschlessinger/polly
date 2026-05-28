package main

import (
	"bufio"
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
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
	m := newReplModel(&Config{Settings: Settings{Model: "test/gpt"}}, "ctx", 0, 0)
	m.input = []rune("hello")
	m.cursor = 5
	got := m.submitPrompt()
	if got != "hello" {
		t.Fatalf("submit returned %q", got)
	}
	if !m.busy {
		t.Fatal("submit should mark busy")
	}
	if len(m.transcript) != 1 || m.transcript[0].kind != transcriptUser {
		t.Fatalf("expected one user transcript entry, got %+v", m.transcript)
	}
	if len(m.history) != 1 || m.history[0] != "hello" {
		t.Fatalf("history not recorded: %+v", m.history)
	}

	// Empty submit doesn't push to history.
	m.busy = false
	m.input = nil
	got = m.submitPrompt()
	if got != "" {
		t.Fatalf("empty submit returned %q", got)
	}
	if len(m.history) != 1 {
		t.Fatalf("empty submit should not extend history, got %d", len(m.history))
	}
}

func TestReplModelHistoryNavigation(t *testing.T) {
	m := newReplModel(&Config{}, "", 0, 0)
	m.history = []string{"first", "second"}
	m.input = []rune("draft")
	m.cursor = 5

	m.historyUp()
	if string(m.input) != "second" {
		t.Fatalf("first up should land on 'second', got %q", string(m.input))
	}
	m.historyUp()
	if string(m.input) != "first" {
		t.Fatalf("second up should land on 'first', got %q", string(m.input))
	}
	m.historyUp() // pinned at top
	if string(m.input) != "first" {
		t.Fatalf("upper bound should stay on 'first', got %q", string(m.input))
	}
	m.historyDown()
	if string(m.input) != "second" {
		t.Fatalf("down should return to 'second', got %q", string(m.input))
	}
	m.historyDown() // restores draft
	if string(m.input) != "draft" {
		t.Fatalf("down past last should restore draft, got %q", string(m.input))
	}
}

func TestReplModelHistoryFrozenWhileBusy(t *testing.T) {
	m := newReplModel(&Config{}, "", 0, 0)
	m.history = []string{"one"}
	m.busy = true
	m.historyUp()
	if len(m.input) != 0 {
		t.Fatalf("history nav should be a no-op while busy, got %q", string(m.input))
	}
}

func TestReplModelAppendAssistantStreaming(t *testing.T) {
	m := newReplModel(&Config{}, "", 0, 0)
	m.appendAssistantText("Hello ")
	m.appendAssistantText("world")
	if len(m.transcript) != 1 {
		t.Fatalf("streaming should accumulate into one entry, got %d", len(m.transcript))
	}
	if m.transcript[0].text != "Hello world" {
		t.Fatalf("got %q", m.transcript[0].text)
	}

	// A non-assistant entry should reset the streaming target.
	m.appendEntry(transcriptEntry{kind: transcriptToolStart, text: "bash"})
	m.appendAssistantText("after-tool")
	if len(m.transcript) != 3 {
		t.Fatalf("expected new assistant entry after tool, got %d", len(m.transcript))
	}
}

func TestReplModelAppendToolEndShapes(t *testing.T) {
	m := newReplModel(&Config{}, "", 0, 0)
	call := messages.ChatMessageToolCall{Name: "bash", Arguments: `{"command":"ls"}`}

	m.appendToolEnd(call, "files", 1500*time.Millisecond, nil)
	last := m.transcript[len(m.transcript)-1]
	if last.kind != transcriptToolOK || last.duration != "1.5s" {
		t.Fatalf("expected toolOK with 1.5s, got %+v", last)
	}

	m.appendToolEnd(call, "", time.Second, errors.New("boom"))
	last = m.transcript[len(m.transcript)-1]
	if last.kind != transcriptToolErr || last.errText != "boom" {
		t.Fatalf("expected toolErr with err, got %+v", last)
	}

	m.appendToolEnd(call, llm.ToolDeniedContent, 0, nil)
	last = m.transcript[len(m.transcript)-1]
	if last.kind != transcriptToolDenied {
		t.Fatalf("expected toolDenied, got %+v", last)
	}
}

func TestReplModelApprovalFlow(t *testing.T) {
	m := newReplModel(&Config{}, "", 0, 0)
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
	m := newReplModel(&Config{}, "", 0, 0)
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

func TestStatusRowDropsLowPriorityFields(t *testing.T) {
	m := newReplModel(&Config{Settings: Settings{Model: "openai/gpt-extra-long-name"}}, "my-context", 4, 2)
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

func TestRenderEntryRowsToolFormatting(t *testing.T) {
	rows := renderEntryRows(transcriptEntry{kind: transcriptToolOK, text: "bash ls", duration: "0.5s"})
	if len(rows) != 1 || !strings.Contains(rows[0], "0.5s bash ls") {
		t.Fatalf("toolOK row malformed: %v", rows)
	}
	rows = renderEntryRows(transcriptEntry{kind: transcriptToolErr, text: "bash ls", duration: "0.5s", errText: "exit 1"})
	if len(rows) != 1 || !strings.Contains(rows[0], "exit 1") {
		t.Fatalf("toolErr row malformed: %v", rows)
	}
}

func TestStyleEscapeRejectsMarkup(t *testing.T) {
	got := styleEscape("hello [world] (fg:red)")
	if !strings.Contains(got, `\[world\]`) {
		t.Fatalf("expected brackets escaped, got %q", got)
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
