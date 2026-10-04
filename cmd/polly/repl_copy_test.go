package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/gdamore/tcell/v3"
	"github.com/gdamore/tcell/v3/vt"
	ui "github.com/metaspartan/gotui/v5"
)

func TestLastCompletedResponse(t *testing.T) {
	previous := messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "previous answer", StopReason: messages.StopReasonEndTurn}
	for _, tc := range []struct {
		name string
		msg  messages.ChatMessage
		want string
	}{
		{"final", messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "\n**answer** 日本語\n```go\nx := 1\n```\n", Reasoning: "private", StopReason: messages.StopReasonEndTurn}, "\n**answer** 日本語\n```go\nx := 1\n```\n"},
		{"legacy", messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "legacy answer"}, "legacy answer"},
		{"truncated", messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "truncated answer", StopReason: messages.StopReasonMaxTokens}, "truncated answer"},
		{"parts", messages.ChatMessage{Role: messages.MessageRoleAssistant, Parts: []messages.ContentPart{{Type: "text", Text: "one\n"}, {Type: "image_url", ImageURL: "https://example.com/image.png"}, {Type: "text", Text: "two"}}}, "one\ntwo"},
		{"user", messages.ChatMessage{Role: messages.MessageRoleUser, Content: "new prompt"}, previous.Content},
		{"tool", messages.ChatMessage{Role: messages.MessageRoleTool, Content: "tool output"}, previous.Content},
		{"tool call", messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "working", ToolCalls: []messages.ChatMessageToolCall{{ID: "call", Name: "bash"}}, StopReason: messages.StopReasonToolUse}, previous.Content},
		{"malformed tool use", messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "working", StopReason: messages.StopReasonToolUse}, previous.Content},
		{"reasoning", messages.ChatMessage{Role: messages.MessageRoleAssistant, Reasoning: "private", StopReason: messages.StopReasonEndTurn}, previous.Content},
		{"blank", messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: " \n\t", StopReason: messages.StopReasonEndTurn}, previous.Content},
		{"error", messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "partial", StopReason: messages.StopReasonError}, previous.Content},
		{"stream error", messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "partial", Metadata: map[string]any{messages.MetadataKeyIsError: true}}, previous.Content},
		{"filtered", messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "partial", StopReason: messages.StopReasonContentFilter}, previous.Content},
		{"internal", interruptedTurnMarker(errors.New("cancelled")), previous.Content},
		{"phased", messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "final answer", TextBlocks: []messages.AssistantText{{Phase: messages.PhaseCommentary, Text: "working"}, {Phase: messages.PhaseFinalAnswer, Text: "final answer"}}}, "final answer"},
		{"commentary only", messages.ChatMessage{Role: messages.MessageRoleAssistant, TextBlocks: []messages.AssistantText{{Phase: messages.PhaseCommentary, Text: "working"}}, StopReason: messages.StopReasonEndTurn}, previous.Content},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := lastCompletedResponse([]messages.ChatMessage{previous, tc.msg})
			if err != nil || got != tc.want {
				t.Fatalf("lastCompletedResponse = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	if _, err := lastCompletedResponse(messages.User("no answer yet")); err == nil {
		t.Fatal("missing response should report an error")
	}
}

type copyTrackingScreen struct {
	tcell.Screen
	text  string
	calls int
}

func (s *copyTrackingScreen) SetClipboard(data []byte) {
	s.text = string(data)
	s.calls++
}

func installCopyTrackingScreen(t *testing.T) *copyTrackingScreen {
	t.Helper()
	screen := &copyTrackingScreen{Screen: newTestScreen(t, 80, 24)}
	old := ui.DefaultBackend.Screen
	ui.DefaultBackend.Screen = screen
	t.Cleanup(func() { ui.DefaultBackend.Screen = old })
	return screen
}

func TestCopyCommandUsesSavedAnswerWhileBusyAndAfterClear(t *testing.T) {
	screen := installCopyTrackingScreen(t)
	store := testOpenMemoryStore(t, nil)
	session := testAcquireSession(t, store, "copy")
	answer := "**saved answer**\n\n```go\nfmt.Println(\"hello\")\n```\n"
	testAddMessages(t, session, []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "first"},
		{Role: messages.MessageRoleAssistant, Content: answer, Reasoning: "private reasoning", StopReason: messages.StopReasonEndTurn},
		{Role: messages.MessageRoleUser, Content: "next"},
	})
	r := newManagedREPL(&Config{}, "copy", 0, 0)
	r.state = &conversationState{session: session}
	r.model.hydrateHistory(testSessionHistory(t, session), "copy")
	r.model.beginTurn("next")
	r.model.appendAssistant("unfinished streaming text")
	r.model.ed.setText("/copy")
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})
	if screen.text != answer || screen.calls != 1 {
		t.Fatalf("clipboard = %q (%d writes), want saved answer", screen.text, screen.calls)
	}
	if len(r.model.queue) != 0 {
		t.Fatal("/copy should run immediately while busy")
	}
	if got := plainStyledText(r.model.fullTranscript()); !strings.Contains(got, "copied last response to clipboard") {
		t.Fatalf("missing copy confirmation: %q", got)
	}
	r.runCommand("/clear")
	r.runCommand("/copy")
	if screen.text != answer || screen.calls != 2 {
		t.Fatalf("copy after clear = %q (%d writes)", screen.text, screen.calls)
	}
	if got := len(testSessionHistory(t, session)); got != 3 {
		t.Fatalf("/copy changed history: %d messages", got)
	}
	if err := session.Clear(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.runCommand("/copy")
	if screen.calls != 2 || !strings.Contains(plainStyledText(r.model.fullTranscript()), "no completed response to copy") {
		t.Fatal("empty history should report nothing to copy and preserve the clipboard")
	}
}

func TestCopyCommandUsesCurrentConversationAndReadOnlyViews(t *testing.T) {
	screen := installCopyTrackingScreen(t)
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "first", "second")
	for _, tab := range r.tabs {
		testAddMessage(t, tab.state.session, messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: tab.name + " answer", StopReason: messages.StopReasonEndTurn})
	}
	r.runCommand("/copy")
	if screen.text != "second answer" {
		t.Fatalf("current tab clipboard = %q", screen.text)
	}
	r.showTab(0)
	r.runCommand("/copy")
	if screen.text != "first answer" {
		t.Fatalf("switched tab clipboard = %q", screen.text)
	}

	// A saved agent conversation has no executable session attached.
	view, err := store.(sessions.ViewStore).ReadView(context.Background(), sessions.ViewTarget{Name: "second"}, "")
	if err != nil {
		t.Fatal(err)
	}
	tab := &replTab{name: "second", childView: view, viewTarget: sessions.ViewTarget{ID: view.ID}, model: prepareChildDisplay(view, r.config, 80), state: r.childViewState(store, view)}
	r.tabs = append(r.tabs, tab)
	r.showTab(len(r.tabs) - 1)
	r.model.ed.setText("/copy")
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})
	if screen.text != "second answer" || tab.state.session != nil || tab.viewOpening {
		t.Fatalf("read-only copy = %q, session=%v, opening=%v", screen.text, tab.state.session, tab.viewOpening)
	}
}

func TestCopyCommandReportsUsageAndFailures(t *testing.T) {
	calls := 0
	ctx := &replCommandContext{copyResponse: func() error {
		calls++
		return fmt.Errorf("clipboard unavailable")
	}}
	for _, line := range []string{"/copy extra", "/copy last"} {
		replies := dispatchDefaultCommandForTest(t, line, ctx)
		if got := strings.Join(replies, "\n"); got != "usage: /copy" || calls != 0 {
			t.Fatalf("%s = %q, calls=%d", line, got, calls)
		}
	}
	replies := dispatchDefaultCommandForTest(t, "/copy", ctx)
	if got := strings.Join(replies, "\n"); got != "copy failed: clipboard unavailable" || calls != 1 {
		t.Fatalf("failed copy = %q, calls=%d", got, calls)
	}
	replies = dispatchDefaultCommandForTest(t, "/copy", &replCommandContext{})
	if got := strings.Join(replies, "\n"); got != "clipboard copying requires a terminal" {
		t.Fatalf("unavailable copy = %q", got)
	}

	var output bytes.Buffer
	writerCtx := newWriterReplCommandContext(&Config{}, nil, &output)
	if _, _, err := defaultReplCommands.dispatch("/copy", writerCtx); err != nil {
		t.Fatalf("redirected copy should not end the REPL: %v", err)
	}
	if got := output.String(); strings.Contains(got, "\x1b") || !strings.Contains(got, "requires a terminal") {
		t.Fatalf("redirected output = %q", got)
	}
}

func TestCopyCommandHelpAndCompletion(t *testing.T) {
	if !defaultReplCommands.busySafeCommand("/copy") || !startupSafeCommand("/copy") || !childViewLocalCommand("/copy") {
		t.Fatal("/copy should be a local, busy-safe command")
	}
	if got := strings.Join(defaultReplCommands.helpFor("/copy"), "\n"); !strings.Contains(got, "usage: /copy") || !strings.Contains(got, "clipboard") {
		t.Fatalf("copy help = %q", got)
	}
	if got := strings.Join(defaultReplCommands.helpLines(), "\n"); !strings.Contains(got, "/copy") {
		t.Fatalf("command list missing /copy: %q", got)
	}
	if completed, _, ok := completeSlash("/cop"); !ok || completed != "/copy" {
		t.Fatalf("copy completion = %q, %v", completed, ok)
	}
}

func TestCopyCommandWritesTerminalClipboard(t *testing.T) {
	term := vt.NewMockTerm(vt.MockOptSize{X: 80, Y: 24})
	screen, err := tcell.NewTerminfoScreenFromTty(term, tcell.OptTerm("xterm-256color"))
	if err != nil {
		t.Fatal(err)
	}
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(screen.Fini)
	go func() {
		for range screen.EventQ() {
		}
	}()
	old := ui.DefaultBackend.Screen
	ui.DefaultBackend.Screen = screen
	t.Cleanup(func() { ui.DefaultBackend.Screen = old })

	store := testOpenMemoryStore(t, nil)
	session := testAcquireSession(t, store, "terminal-copy")
	answer := "**answer** 日本語\n```go\nx := 1\n```\n"
	testAddMessage(t, session, messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: answer, StopReason: messages.StopReasonEndTurn})
	r := newManagedREPL(&Config{}, "terminal-copy", 0, 0)
	r.state = &conversationState{session: session}
	r.runCommand("/copy")
	if err := term.Drain(); err != nil {
		t.Fatal(err)
	}
	if got := string(term.Backend().GetClipboard()); got != answer {
		t.Fatalf("terminal clipboard = %q, want %q", got, answer)
	}
}

func TestWriteClipboardText(t *testing.T) {
	text := "**answer** 日本語\n```go\nx := 1\n```\n"
	var output bytes.Buffer
	if err := writeClipboardText(&output, text); err != nil {
		t.Fatal(err)
	}
	want := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + "\x1b\\"
	if got := output.String(); got != want {
		t.Fatalf("clipboard escape = %q, want %q", got, want)
	}
}
