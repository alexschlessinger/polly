package main

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
)

// A coordinator's answer stays provisional while its agents work. When their
// results arrive it answers again, and only hidden coordination messages sit
// between the two answers. Each is a message of its own.

// answerRows are the rows a view shows from first on.
func answerRows(t *testing.T, m *replModel, first string) []string {
	t.Helper()
	rows := strings.Split(shown(m, 80), "\n")
	at := slices.Index(rows, first)
	if at < 0 {
		t.Fatalf("no row reads %q:\n%s", first, strings.Join(rows, "\n"))
	}
	return rows[at:]
}

// The live view shows a turn's second answer as a block of its own, the way
// the same turn reads when the session is opened again.
func TestSecondAnswerOfATurnStartsItsOwnBlock(t *testing.T) {
	withDisplayTTY(t)
	const first, second = "The agent is running.", "The agent finished."
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("delegate")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	tui.ModelAttemptStarted(0, 0)
	tui.AppendAssistantText(first)
	// The agent's result arrives as hidden input and the model is asked again.
	tui.ModelAttemptStarted(1, 0)
	tui.AppendAssistantText(second)
	tui.FinishTextTurn()
	live := answerRows(t, m, first)

	reloaded := newReplModel()
	reloaded.hydrateHistory([]messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "delegate"},
		{Role: messages.MessageRoleAssistant, Content: first},
		{Role: messages.MessageRoleUser, Content: "<peer_messages>the agent's result</peer_messages>", Metadata: map[string]any{messages.MetadataKeyAgentSynthetic: true}},
		{Role: messages.MessageRoleAssistant, Content: second},
	}, "ctx")
	saved := answerRows(t, reloaded, first)

	want := []string{first, second}
	if !slices.Equal(live, want) || !slices.Equal(saved, want) {
		t.Fatalf("live answers %q, reloaded %q, want both %q", live, saved, want)
	}
}

// A second answer that opens with a thought has the thought's row between
// the two answers, in the order the turn made them.
func TestSecondAnswerFollowsItsThought(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("delegate")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	tui.ModelAttemptStarted(0, 0)
	tui.AppendAssistantText("The agent is running.")
	tui.ModelAttemptStarted(1, 0)
	tui.ShowThinking("read the result")
	tui.AppendAssistantText("The agent finished.")
	tui.FinishTextTurn()
	rows := answerRows(t, m, "The agent is running.")
	if len(rows) != 3 || !strings.HasPrefix(rows[1], thoughtGlyph+" ") || rows[2] != "The agent finished." {
		t.Fatalf("rows = %q, want the first answer, the thought, the second answer", rows)
	}
}

// The line frontend starts a turn's second answer on its own line, as it
// does text that follows a tool call: after a blank line on a terminal, on
// the next line when piped.
func TestLineSecondAnswerStartsOnItsOwnLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		tty  bool
		want string
	}{
		{"terminal", true, "The agent is running.\n\nThe agent finished."},
		{"piped", false, "The agent is running.\nThe agent finished."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, status bytes.Buffer
			ui := &lineTurnUI{config: &Config{}, interactive: tc.tty, stdoutTTY: tc.tty, writer: &out, errWriter: &status}
			ui.ModelAttemptStarted(0, 0)
			ui.AppendAssistantText("The agent ")
			ui.AppendAssistantText("is running.")
			ui.ModelAttemptStarted(1, 0)
			ui.AppendAssistantText("The agent finished.")
			if out.String() != tc.want {
				t.Fatalf("output %q, want %q", out.String(), tc.want)
			}
		})
	}
}

// A request that follows a tool call, a thought or nothing at all adds no
// line: only an answer already on the line is ended.
func TestLineRequestWithoutAnAnswerBeforeItAddsNoLine(t *testing.T) {
	var out, status bytes.Buffer
	ui := &lineTurnUI{config: &Config{}, interactive: true, stdoutTTY: true, writer: &out, errWriter: &status}
	ui.ModelAttemptStarted(0, 0)
	ui.ShowThinking("plan")
	ui.ModelAttemptStarted(1, 0)
	ui.AppendAssistantText("Only answer.")
	if out.String() != "Only answer." {
		t.Fatalf("output %q, want the answer alone", out.String())
	}
}

// The streaming line renderer commits the first answer before the second
// begins, so the two never share a paragraph.
func TestLineStreamSecondAnswerIsItsOwnParagraph(t *testing.T) {
	columns, height := 70, 12
	ui, out := lineStreamTestUI(t, false, true, &columns, &height)
	ui.ModelAttemptStarted(0, 0)
	ui.AppendAssistantText("The agent is running.")
	paintLineStream(ui)
	ui.ModelAttemptStarted(1, 0)
	ui.AppendAssistantText("The agent finished.")
	paintLineStream(ui)
	ui.FinishTextTurn()
	got := lineTestScreen(out.String())
	first, second := strings.Index(got, "The agent is running."), strings.Index(got, "The agent finished.")
	if first < 0 || second < first || strings.Contains(got, "running.The agent") || !strings.Contains(got[first:second], "\n") {
		t.Fatalf("answers share a paragraph or are missing:\n%s", got)
	}
}
