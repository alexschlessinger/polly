package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
	rw "github.com/mattn/go-runewidth"
	ui "github.com/metaspartan/gotui/v5"
)

func TestManagedSandboxWarningsDrainIntoQuietTranscriptOnce(t *testing.T) {
	home, root := broadWarningTestPaths(t)
	warnings := newBroadWritablePathWarner()
	warnings.Warn(sandbox.Config{WritablePaths: []string{home}})

	r := newManagedREPL(&Config{Quiet: true}, "ctx", 0, 0)
	r.state = &conversationState{sandboxWarnings: warnings}
	r.model.mu.Lock()
	appended := r.appendPendingSandboxWarningsLocked()
	r.model.mu.Unlock()
	if !appended {
		t.Fatal("buffered sandbox warning was not appended")
	}

	joined := plainStyledText(strings.Join(r.model.flattenTranscript(), "\n"))
	if strings.Count(joined, "Warning: sandbox writable path") != 1 || !strings.Contains(joined, "whole home directory") {
		t.Fatalf("buffered warning transcript = %q, want one visible home warning in quiet mode", joined)
	}
	if r.model.turnHasOutput {
		t.Fatal("startup sandbox warning marked the assistant turn as having output")
	}
	r.model.mu.Lock()
	appended = r.appendPendingSandboxWarningsLocked()
	r.model.mu.Unlock()
	if appended {
		t.Fatal("empty second drain reported an appended warning")
	}

	warnings.Warn(sandbox.Config{WritablePaths: []string{root}})
	warnings.Warn(sandbox.Config{WritablePaths: []string{home, root}})
	r.model.mu.Lock()
	appended = r.appendPendingSandboxWarningsLocked()
	r.model.mu.Unlock()
	if !appended {
		t.Fatal("late sandbox warning was not appended")
	}

	joined = plainStyledText(strings.Join(r.model.flattenTranscript(), "\n"))
	if strings.Count(joined, "Warning: sandbox writable path") != 2 || !strings.Contains(joined, "filesystem root") {
		t.Fatalf("live warning transcript = %q, want one late root warning with duplicates suppressed", joined)
	}
}

func TestDrainSandboxWarningsToWriterEmptiesQueue(t *testing.T) {
	home, root := broadWarningTestPaths(t)
	warnings := newBroadWritablePathWarner()
	warnings.Warn(sandbox.Config{WritablePaths: []string{home}})
	state := &conversationState{sandboxWarnings: warnings}

	var out bytes.Buffer
	drainSandboxWarningsToWriter(&out, state)
	got := out.String()
	if strings.Count(got, "Warning: sandbox writable path") != 1 || !strings.Contains(got, "whole home directory") {
		t.Fatalf("line warning output = %q, want the buffered home warning", got)
	}

	out.Reset()
	drainSandboxWarningsToWriter(&out, state)
	if out.Len() != 0 {
		t.Fatalf("second drain output = %q, want empty", out.String())
	}

	warnings.Warn(sandbox.Config{WritablePaths: []string{root}})
	drainSandboxWarningsToWriter(&out, state)
	if got := out.String(); strings.Count(got, "Warning: sandbox writable path") != 1 || !strings.Contains(got, "filesystem root") {
		t.Fatalf("late line warning output = %q, want one root warning", got)
	}
}

func broadWarningTestPaths(t *testing.T) (home, root string) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	home = canonicalWarningPath(home)
	root = filepath.VolumeName(home) + string(filepath.Separator)
	root = canonicalWarningPath(root)
	if home == "" || root == "" || home == root {
		t.Skipf("need distinct home and filesystem root, got home=%q root=%q", home, root)
	}
	return home, root
}

func TestAssistantTerminalNewlinesDoNotOwnTurnSpacing(t *testing.T) {
	for _, terminalNewlines := range []string{"", "\n", "\n\n"} {
		t.Run(fmt.Sprintf("terminal-newlines-%d", len(terminalNewlines)), func(t *testing.T) {
			m := newReplModel()
			m.beginTurn("first")
			m.appendAssistant("answer" + terminalNewlines)
			if got := m.flattenTranscript(); len(got) != 2 || got[1] != "answer" {
				t.Fatalf("streaming transcript rows = %#v, terminal newline should stay provisional", got)
			}
			m.finishAssistantBlock("")

			if got := m.flattenTranscript(); len(got) != 2 || got[1] != "answer" {
				t.Fatalf("settled transcript rows = %#v, want prompt + answer with no terminal blank", got)
			}

			m.beginTurn("second")
			got := m.flattenTranscript()
			if len(got) != 4 || got[2] != "" || !strings.Contains(got[3], "second") {
				t.Fatalf("next turn rows = %#v, want exactly one renderer-owned blank row", got)
			}
		})
	}
}

func TestAssistantInternalBlankLinesArePreserved(t *testing.T) {
	m := newReplModel()
	m.appendAssistant("alpha\n\nbeta\n\n")
	m.finishAssistantBlock("")
	if got, want := m.flattenTranscript(), []string{"alpha", "", "beta"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("rows = %#v, want %#v", got, want)
	}
}

func TestFailedTurnLabelsPartialAndMarksQueueNotSent(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("explain")
	m.appendToolStartLine("call-1", "bash sleep 30")
	m.appendAssistant("partial answer\n")
	m.ed.setText("next question")
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})
	r.endTurn(errors.New("provider unavailable"))

	joined := strings.Join(m.flattenTranscript(), "\n")
	plain := plainStyledText(joined)
	for _, want := range []string{"▸ 1 tool", "partial answer", "failed · not saved", "provider unavailable"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("failed transcript %q missing %q", plain, want)
		}
	}
	if strings.Contains(plain, "bash sleep 30") {
		t.Fatalf("failed turn leaked collapsed tool detail: %q", plain)
	}
	record := m.currentToolDisclosure()
	if record == nil || !record.complete || record.expanded || !m.toggleToolDisclosure(record.id) {
		t.Fatalf("failed tool disclosure did not reopen: %#v", record)
	}
	expandedTools := plainStyledText(m.transcript[record.transcriptIndex])
	if !strings.Contains(expandedTools, "failed bash sleep 30") || strings.Contains(expandedTools, "→") {
		t.Fatalf("failed tool expansion = %q", expandedTools)
	}
	if m.busy || m.state != turnStateIdle || m.lastOutcome != turnOutcomeFailed {
		t.Fatalf("failed turn did not settle idle: busy=%v state=%v outcome=%v", m.busy, m.state, m.lastOutcome)
	}
	if len(m.queue) != 0 || !strings.Contains(plain, "> next question\n  (not sent)") {
		t.Fatalf("failed turn left pending input queued: queue=%v transcript=%q", m.queue, joined)
	}
	if m.ed.text() != "explain" || m.restoredDraft == nil {
		t.Fatalf("failed input was not restored: editor=%q draft=%#v", m.ed.text(), m.restoredDraft)
	}
}

func TestFailedPostToolCallLabelsEarlierGeneratedOutput(t *testing.T) {
	r := newManagedREPL(&Config{Quiet: true}, "ctx", 0, 0)
	r.model.beginTurn("do work")
	tui := &gotuiTurnUI{repl: r, config: r.config}
	tui.AppendAssistantText("I will inspect that.")
	tui.AppendToolStart([]messages.ChatMessageToolCall{{ID: "1", Name: "bash"}})
	r.endTurn(errors.New("follow-up completion failed"))
	joined := strings.Join(r.model.flattenTranscript(), "\n")
	if !strings.Contains(joined, "I will inspect that.") || !strings.Contains(joined, "failed · not saved") {
		t.Fatalf("pre-tool output was left looking saved after a later failure: %q", joined)
	}
}

func TestCancelFreezesPartialAndRejectsLateCallbacks(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("question")
	m.turnID = 9
	m.ed.setText("queued")
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})
	tui := &gotuiTurnUI{repl: r, config: r.config, turnID: 9}
	tui.AppendAssistantText("visible partial")

	if quit := r.handleInterrupt(); quit {
		t.Fatal("first cancel should keep the REPL open")
	}
	tui.AppendAssistantText(" late")
	tui.AppendWarning("late warning")
	tui.RecordTurnTokens(99, 88)
	tui.FinishTextTurn()
	r.endTurn(context.Canceled)
	tui.AppendAssistantText(" after settle")
	tui.AppendWarning("after-settle warning")

	joined := strings.Join(m.flattenTranscript(), "\n")
	plain := plainStyledText(joined)
	if !strings.Contains(joined, "visible partial") || !strings.Contains(joined, "canceled · not saved") {
		t.Fatalf("canceled partial was not retained and labeled: %q", joined)
	}
	if strings.Contains(joined, "late") || strings.Contains(joined, "after settle") || strings.Contains(joined, "after-settle") || m.lastIn != 0 || m.lastOut != 0 {
		t.Fatalf("post-cancel callback mutated settled state: transcript=%q tokens=%d/%d", joined, m.lastIn, m.lastOut)
	}
	for _, redundant := range []string{"cancel requested", "input restored to composer", "completed work saved"} {
		if strings.Contains(plain, redundant) {
			t.Fatalf("canceled transcript retained redundant notice %q: %q", redundant, plain)
		}
	}
	if len(m.queue) != 0 || !strings.Contains(plain, "> queued\n  (not sent)") {
		t.Fatalf("cancel left pending input queued: queue=%v transcript=%q", m.queue, joined)
	}
}

func TestCancelRequestLosingCompletionRaceDoesNotClaimUnsaved(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	r.model.beginTurn("question")
	tui := &gotuiTurnUI{repl: r, config: r.config}
	tui.AppendAssistantText("completed answer")
	if quit := r.handleInterrupt(); quit {
		t.Fatal("first cancel request should not quit")
	}
	r.endTurn(nil)

	plain := plainStyledText(strings.Join(r.model.flattenTranscript(), "\n"))
	if strings.Contains(plain, "not saved") {
		t.Fatalf("successful completion retained false unsaved label: %q", plain)
	}
	if r.model.lastOutcome != turnOutcomeDone || r.model.restoredDraft != nil || !r.model.ed.empty() {
		t.Fatalf("race winner did not settle successful: outcome=%v draft=%#v editor=%q", r.model.lastOutcome, r.model.restoredDraft, r.model.ed.text())
	}
}

func TestInterruptedTurnWithPersistedProgressOmitsUnsavedLabel(t *testing.T) {
	for _, tc := range []struct {
		name        string
		err         error
		wantLabel   string
		wantOutcome turnOutcome
	}{
		{
			name:        "failed",
			err:         &turnProgressSavedError{cause: errors.New("provider exploded")},
			wantLabel:   "failed",
			wantOutcome: turnOutcomeFailed,
		},
		{
			name:        "canceled",
			err:         &turnProgressSavedError{cause: context.Canceled},
			wantLabel:   "canceled",
			wantOutcome: turnOutcomeCanceled,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newManagedREPL(&Config{Quiet: true}, "ctx", 0, 0)
			r.model.beginTurn("do work")
			tui := &gotuiTurnUI{repl: r, config: r.config}
			tui.AppendAssistantText("I ran the tools.")
			r.endTurn(tc.err)

			plain := plainStyledText(strings.Join(r.model.flattenTranscript(), "\n"))
			if !strings.Contains(plain, tc.wantLabel) {
				t.Fatalf("settled transcript %q missing %q", plain, tc.wantLabel)
			}
			if strings.Contains(plain, "not saved") {
				t.Fatalf("persisted progress was still labeled unsaved: %q", plain)
			}
			if strings.Contains(plain, "completed work saved") {
				t.Fatalf("persisted progress retained redundant success notice: %q", plain)
			}
			if r.model.lastOutcome != tc.wantOutcome {
				t.Fatalf("outcome = %v, want %v", r.model.lastOutcome, tc.wantOutcome)
			}
		})
	}
}

func TestInterruptedTurnWithPersistedProgressKeepsReasoningSavedLabel(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("explain")
	tui := &gotuiTurnUI{repl: r, config: r.config}
	tui.ShowThinking("reasoning from a persisted iteration")
	record := m.currentReasoningRecord()
	r.endTurn(&turnProgressSavedError{cause: errors.New("provider failed")})
	if record == nil || !record.complete || record.unsaved {
		t.Fatalf("persisted-progress reasoning disclosure = %#v, want saved", record)
	}
	if collapsed := plainStyledText(m.transcript[record.transcriptIndex]); strings.Contains(collapsed, "not saved") {
		t.Fatalf("persisted reasoning was labeled unsaved: %q", collapsed)
	}
}

func TestHydrateHistorySettlesInterruptedTurn(t *testing.T) {
	m := newReplModel()
	m.hydrateHistory([]messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "do the work"},
		{Role: messages.MessageRoleAssistant, Content: "starting the work"},
		interruptedTurnMarker(errors.New("provider exploded")),
	}, "ctx")

	plain := plainStyledText(strings.Join(m.flattenTranscript(), "\n"))
	if !strings.Contains(plain, "turn interrupted · completed work retained") {
		t.Fatalf("interrupted marker was not rendered: %q", plain)
	}
	if strings.Contains(plain, "incomplete") || m.restoredDraft != nil {
		t.Fatalf("interrupted turn was treated as an unsent draft: plain=%q draft=%#v", plain, m.restoredDraft)
	}
}

func TestApprovalEnterAndEscapeDenyWithoutQuitting(t *testing.T) {
	for _, key := range []string{"<Enter>", "<Escape>"} {
		t.Run(key, func(t *testing.T) {
			r := newManagedREPL(&Config{}, "ctx", 0, 0)
			reply := make(chan []bool, 1)
			r.model.approval = &approvalState{
				calls: []messages.ChatMessageToolCall{{Name: "bash"}},
				reply: reply,
			}
			if quit := r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: key}); quit {
				t.Fatalf("%s should deny, not quit", key)
			}
			if got := <-reply; len(got) != 1 || got[0] {
				t.Fatalf("%s approval result = %v, want denied", key, got)
			}
			select {
			case <-r.quit:
				t.Fatalf("%s unexpectedly requested REPL exit", key)
			default:
			}
		})
	}
}

func TestEmptyToolApprovalDoesNotInstallBlockingPrompt(t *testing.T) {
	r := newManagedREPL(&Config{Confirm: true}, "ctx", 0, 0)
	tui := &gotuiTurnUI{repl: r, config: r.config}
	if got := tui.ApproveToolCalls(nil); got != nil {
		t.Fatalf("empty approval = %v, want nil", got)
	}
	if r.model.approval != nil {
		t.Fatal("empty tool batch installed an approval prompt")
	}
}

func TestApprovalPromptFitsNarrowTerminal(t *testing.T) {
	m := newReplModel()
	m.approval = &approvalState{
		calls: []messages.ChatMessageToolCall{{Name: "custom_tool_with_a_very_long_name"}, {Name: "next"}},
	}
	text, _, _, _, _ := m.renderInputForTerminal(1, 20)
	plain := plainStyledText(text)
	if rw.StringWidth(plain) > 20 {
		t.Fatalf("approval width = %d, want <= 20: %q", rw.StringWidth(plain), plain)
	}
	if !strings.Contains(plain, "[y/N/a/v]") {
		t.Fatalf("narrow approval lost its actions: %q", plain)
	}
}

func TestStatusAndApprovalNeverExceedTerminalWidth(t *testing.T) {
	m := newReplModel()
	m.busy = true
	m.state = turnStateTool
	m.toolName = "custom_tool_with_a_very_long_name"
	m.turnStarted = time.Now()
	m.modelName = "provider/model-with-a-long-name"
	m.contextName = "context-with-a-long-name"
	m.toolCount = 12
	m.skillCount = 8
	m.queue = queuedTextInputs("a long queued message preview that must truncate")
	for width := 1; width <= 80; width++ {
		if got := rw.StringWidth(plainStyledText(m.statusRow(width))); got > width {
			t.Fatalf("status width %d exceeds terminal %d: %q", got, width, plainStyledText(m.statusRow(width)))
		}
	}
	if status := plainStyledText(m.statusRow(80)); strings.Contains(status, "queued") || strings.Contains(status, "paused") {
		t.Fatalf("status still exposes queue state: %q", status)
	}

	m.approval = &approvalState{calls: []messages.ChatMessageToolCall{{Name: "custom_tool_with_a_very_long_name"}}}
	for width := 1; width <= 60; width++ {
		text, _, _, _, _ := m.renderInputForTerminal(1, width)
		if got := rw.StringWidth(plainStyledText(text)); got > width {
			t.Fatalf("approval width %d exceeds terminal %d: %q", got, width, plainStyledText(text))
		}
	}
}

func TestBusyQuietModeAcceptsPaste(t *testing.T) {
	r := newManagedREPL(&Config{Quiet: true}, "ctx", 0, 0)
	r.model.busy = true
	for _, event := range []ui.Event{
		{Type: ui.KeyboardEvent, ID: pasteStartID},
		{Type: ui.KeyboardEvent, ID: "a"},
		{Type: ui.KeyboardEvent, ID: "<Enter>"},
		{Type: ui.KeyboardEvent, ID: "b"},
		{Type: ui.KeyboardEvent, ID: pasteEndID},
	} {
		r.handleEvent(event)
	}
	if got := r.model.ed.text(); got != "a\nb" {
		t.Fatalf("busy paste = %q, want multi-line draft", got)
	}
	if _, _, _, _, editable := r.model.renderInput(); !editable {
		t.Fatal("quiet busy composer should stay editable")
	}
}

func TestBusyReadOnlyCommandsRunImmediately(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	r.model.busy = true

	// Read-only inspection runs right away instead of queueing.
	r.model.ed.setText("/help")
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})
	if len(r.model.queue) != 0 {
		t.Fatalf("busy /help was queued instead of executed: %v", r.model.queue)
	}
	if joined := strings.Join(r.model.flattenTranscript(), "\n"); !strings.Contains(joined, "commands:") {
		t.Fatalf("busy /help output missing: %q", joined)
	}

	// Mutating commands still queue behind the running turn.
	r.model.ed.setText("/reset confirm")
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})
	if got := r.model.queue; len(got) != 1 || got[0].text != "/reset confirm" {
		t.Fatalf("busy /reset should queue, got %v", got)
	}
}

func TestQueueSubmissionAppearsInTranscript(t *testing.T) {
	for _, quiet := range []bool{false, true} {
		t.Run(fmt.Sprintf("quiet=%v", quiet), func(t *testing.T) {
			r := newManagedREPL(&Config{Quiet: quiet}, "ctx", 0, 0)
			r.model.busy = true
			r.model.ed.setText("next message")
			r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})
			joined := plainStyledText(r.model.fullTranscript())
			if !strings.Contains(joined, "> next message\n  (queued)") {
				t.Fatalf("queued transcript entry missing: %q", joined)
			}
			if status := plainStyledText(r.model.statusRow(80)); strings.Contains(status, "queued") {
				t.Fatalf("queue leaked into status bar: %q", status)
			}
		})
	}
}

func TestQueueTranscriptEntryDoesNotSplitAssistantStream(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	r.model.beginTurn("question")
	tui := &gotuiTurnUI{repl: r, config: r.config}
	tui.AppendAssistantText("hel")
	r.model.ed.setText("next")
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})
	tui.AppendAssistantText("lo")
	tui.FinishTextTurn()
	got := r.model.flattenTranscript()
	count := 0
	for _, line := range got {
		if line == "hello" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("queued entry split assistant output: %#v", got)
	}
	if joined := plainStyledText(strings.Join(got, "\n")); !strings.Contains(joined, "> next\n  (queued)") {
		t.Fatalf("queued entry missing beside assistant stream: %#v", got)
	}
}

func TestQueuedTranscriptEntryActivatesWithoutDuplicate(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	r.model.beginTurn("current")
	r.model.ed.setText("next")
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})
	r.endTurn(nil)

	started := make(chan string, 1)
	done := r.startNextQueued(context.Background(), func(_ context.Context, prompt string, _ TurnUI) error {
		started <- prompt
		return nil
	})
	if done == nil || <-started != "next" {
		t.Fatal("queued turn did not start")
	}
	joined := plainStyledText(r.model.fullTranscript())
	if strings.Count(joined, "> next") != 1 || strings.Contains(joined, "(queued)") {
		t.Fatalf("queued entry was duplicated or left marked after activation: %q", joined)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestClearDisplayMidTurnDoesNotInventNoResponse(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	r.model.beginTurn("question")
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-l>"})
	tui := &gotuiTurnUI{repl: r, config: r.config}
	tui.AppendAssistantText("answer after clear")
	r.endTurn(nil)

	plain := plainStyledText(strings.Join(r.model.flattenTranscript(), "\n"))
	if !strings.Contains(plain, "answer after clear") || strings.Contains(plain, "(no response)") {
		t.Fatalf("mid-turn clear completion = %q", plain)
	}
}

func TestClearDisplayPreservesParallelToolState(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	r.model.beginTurn("run both")
	tui := &gotuiTurnUI{repl: r, config: r.config}
	calls := []messages.ChatMessageToolCall{{ID: "1", Name: "one"}, {ID: "2", Name: "two"}}
	tui.AppendToolStart(calls)
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-l>"})
	tui.AppendToolEnd(calls[0], "ok", time.Millisecond, nil)
	if r.model.runningTools != 1 || r.model.state != turnStateTool {
		t.Fatalf("first completion after clear settled siblings: running=%d state=%v", r.model.runningTools, r.model.state)
	}
	tui.AppendToolEnd(calls[1], "ok", time.Millisecond, nil)
	if r.model.runningTools != 0 || r.model.state != turnStateWaiting {
		t.Fatalf("parallel tools did not settle after final completion: running=%d state=%v", r.model.runningTools, r.model.state)
	}
	record := r.model.currentToolDisclosure()
	if record == nil || len(r.model.toolDisclosures) != 1 || len(record.rows) != 2 {
		t.Fatalf("post-clear tool disclosure = %#v records=%d", record, len(r.model.toolDisclosures))
	}
}

func TestDeleteAndControlDDeleteForward(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	r.model.ed.setText("abc")
	r.model.ed.home()
	r.model.ed.right()
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Delete>"})
	if got := r.model.ed.text(); got != "ac" {
		t.Fatalf("Delete = %q, want ac", got)
	}
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-d>"})
	if got := r.model.ed.text(); got != "a" {
		t.Fatalf("Ctrl-D on non-empty input = %q, want a", got)
	}
}

func TestHomeAndEndAreLogicalLineLocal(t *testing.T) {
	var e lineEditor
	e.setText("one\ntwo\nthree")
	e.home()
	if e.cursor != len([]rune("one\ntwo\n")) {
		t.Fatalf("Home moved to %d, want start of last line", e.cursor)
	}
	e.up()
	e.end()
	if e.cursor != len([]rune("one\ntwo")) {
		t.Fatalf("End moved to %d, want end of middle line", e.cursor)
	}
}

func TestHydrateHistoryShowsFiveRecentTurnsAndCollapsesTools(t *testing.T) {
	var history []messages.ChatMessage
	for i := 0; i < 7; i++ {
		history = append(history, messages.ChatMessage{Role: messages.MessageRoleUser, Content: fmt.Sprintf("unique-question-%d", i)})
		if i == 5 {
			history = append(history,
				messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "checking", ToolCalls: []messages.ChatMessageToolCall{
					{ID: "a", Name: "bash"}, {ID: "b", Name: "read_file"},
				}},
				messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: "a", Content: "raw secret shell output"},
				messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: "b", Content: "raw secret file output"},
				messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "tool work done"},
			)
		} else {
			history = append(history, messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: fmt.Sprintf("unique-answer-%d", i)})
		}
	}

	m := newReplModel()
	m.hydrateHistory(history, "project-x")
	joined := strings.Join(m.flattenTranscript(), "\n")
	for _, absent := range []string{"unique-question-0", "unique-question-1", "raw secret shell output", "raw secret file output"} {
		if strings.Contains(joined, absent) {
			t.Fatalf("hydrated transcript leaked excluded/raw content %q: %q", absent, joined)
		}
	}
	for _, present := range []string{"showing last 5 of 7 turns", "unique-question-2", "unique-question-6", "2 tools", "tool work done"} {
		if !strings.Contains(joined, present) {
			t.Fatalf("hydrated transcript missing %q: %q", present, joined)
		}
	}
	if len(m.toolDisclosures) != 1 {
		t.Fatalf("hydrated tool disclosures = %d, want 1", len(m.toolDisclosures))
	}
	var tools *toolDisclosureRecord
	for _, record := range m.toolDisclosures {
		tools = record
	}
	if !m.toggleToolDisclosure(tools.id) {
		t.Fatal("hydrated tool disclosure was not expandable")
	}
	if plain := plainStyledText(m.transcript[tools.transcriptIndex]); !strings.Contains(plain, "· bash") ||
		!strings.Contains(plain, "· read_file") || strings.Contains(plain, "✓") {
		t.Fatalf("legacy tool outcomes should hydrate neutrally, got %q", plain)
	}
}

func TestHydrateHistoryShowsDurableToolFailure(t *testing.T) {
	toolResult := messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: "1", ToolName: "bash", Content: "Error: exit 1"}
	toolResult.SetError(errors.New("exit 1"))
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "run"},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "1", Name: "bash"}}},
		toolResult,
		{Role: messages.MessageRoleAssistant, Content: "done"},
	}
	m := newReplModel()
	m.hydrateHistory(history, "ctx")
	var tools *toolDisclosureRecord
	for _, record := range m.toolDisclosures {
		tools = record
	}
	if tools == nil || !m.toggleToolDisclosure(tools.id) {
		t.Fatal("durable failed tool disclosure was not expandable")
	}
	if plain := plainStyledText(m.transcript[tools.transcriptIndex]); !strings.Contains(plain, "✗ bash") {
		t.Fatalf("durable failed tool was not restored as failed: %q", plain)
	}
}

func TestHydrateHistoryAggregatesToolBatchesPerRealUserTurn(t *testing.T) {
	succeeded := func(id, name string) messages.ChatMessage {
		msg := messages.ChatMessage{
			Role:       messages.MessageRoleTool,
			ToolCallID: id,
			ToolName:   name,
			Content:    "raw result for " + name,
		}
		msg.SetToolSucceeded(true)
		return msg
	}
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "first real turn"},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "a", Name: "read_alpha"}}},
		succeeded("a", "read_alpha"),
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "b", Name: "search_beta"}}},
		succeeded("b", "search_beta"),
		{Role: messages.MessageRoleUser, Content: "synthetic continuation", Metadata: map[string]any{messages.MetadataKeyAgentSynthetic: true}},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "c", Name: "fetch_gamma"}}},
		succeeded("c", "fetch_gamma"),
		{Role: messages.MessageRoleAssistant, Content: "first turn done"},
		{Role: messages.MessageRoleUser, Content: "second real turn"},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "d", Name: "write_delta"}}},
		succeeded("d", "write_delta"),
		{Role: messages.MessageRoleAssistant, Content: "second turn done"},
	}

	m := newReplModel()
	m.hydrateHistory(history, "ctx")
	var records []*toolDisclosureRecord
	for index := range m.transcript {
		if id := m.toolDisclosureAt[index]; id != 0 {
			records = append(records, m.toolDisclosures[id])
		}
	}
	if len(records) != 2 {
		t.Fatalf("hydrated tool disclosures = %d, want one per real user turn: %#v", len(records), records)
	}
	wants := [][]string{{"read_alpha", "search_beta", "fetch_gamma"}, {"write_delta"}}
	for i, record := range records {
		if record == nil || !record.complete || record.expanded || len(record.rows) != len(wants[i]) {
			t.Fatalf("hydrated disclosure %d = %#v, want %d completed collapsed rows", i, record, len(wants[i]))
		}
		for row, want := range wants[i] {
			if got := record.rows[row].label; got != want {
				t.Fatalf("hydrated disclosure %d row %d = %q, want %q", i, row, got, want)
			}
		}
		if !m.toggleToolDisclosure(record.id) {
			t.Fatalf("hydrated disclosure %d did not expand", i)
		}
		expanded := plainStyledText(m.transcript[record.transcriptIndex])
		for _, want := range wants[i] {
			if got := strings.Count(expanded, want); got != 1 {
				t.Fatalf("hydrated disclosure %d contains %q %d times: %q", i, want, got, expanded)
			}
		}
	}
	firstExpanded := plainStyledText(m.transcript[records[0].transcriptIndex])
	if strings.Contains(firstExpanded, "write_delta") {
		t.Fatalf("next real user turn leaked into first disclosure: %q", firstExpanded)
	}
	if joined := plainStyledText(strings.Join(m.transcript, "\n")); strings.Contains(joined, "raw result for") || strings.Contains(joined, "synthetic continuation") {
		t.Fatalf("hydration leaked raw or synthetic content: %q", joined)
	}
}

func TestHydrateHistoryBuildsOneCompletedReasoningDisclosurePerTurn(t *testing.T) {
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "first"},
		{Role: messages.MessageRoleAssistant, Reasoning: "inspect the inputs", ToolCalls: []messages.ChatMessageToolCall{{ID: "1", Name: "read_file"}}},
		{Role: messages.MessageRoleTool, ToolCallID: "1", ToolName: "read_file", Content: "ok"},
		{Role: messages.MessageRoleUser, Content: "synthetic continuation", Metadata: map[string]any{messages.MetadataKeyAgentSynthetic: true}},
		{Role: messages.MessageRoleAssistant, Reasoning: "compare the result", Content: "first answer"},
		{Role: messages.MessageRoleUser, Content: "second"},
		{Role: messages.MessageRoleAssistant, Reasoning: "answer directly", Content: "second answer"},
	}

	m := newReplModel()
	m.hydrateHistory(history, "ctx")
	if len(m.reasoningOrder) != 2 {
		t.Fatalf("hydrated reasoning disclosures = %d, want one per real user turn", len(m.reasoningOrder))
	}
	first := m.reasoningRecords[m.reasoningOrder[0]]
	second := m.reasoningRecords[m.reasoningOrder[1]]
	for i, record := range []*reasoningRecord{first, second} {
		if record == nil || !record.complete || record.active || record.unsaved || record.expanded {
			t.Fatalf("hydrated record %d = %#v, want completed and collapsed", i, record)
		}
	}
	collapsed := plainStyledText(strings.Join(m.flattenTranscript(), "\n"))
	for _, hidden := range []string{"inspect the inputs", "compare the result", "answer directly"} {
		if strings.Contains(collapsed, hidden) {
			t.Fatalf("collapsed hydration leaked reasoning %q: %q", hidden, collapsed)
		}
	}
	if !strings.Contains(string(first.tail), "inspect the inputs\ncompare the result") {
		t.Fatalf("first turn did not aggregate tool-separated segments: %q", string(first.tail))
	}
	if !m.toggleReasoning(first.id, 80) {
		t.Fatal("completed hydrated disclosure was not toggleable")
	}
	expanded := plainStyledText(m.transcript[first.transcriptIndex])
	if !strings.Contains(expanded, "inspect the inputs") || !strings.Contains(expanded, "compare the result") {
		t.Fatalf("expanded hydrated disclosure = %q", expanded)
	}
	if !m.toggleLatestReasoning(80) || !second.expanded {
		t.Fatal("idle Ctrl-O target should be the newest completed turn")
	}
}

func TestCompletedReasoningDisclosureSurvivesDiskReload(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "reasoning.db")
	store := testOpenDiskStore(t, dbPath, nil)
	session := testAcquireSession(t, store, "reasoning-reload")
	testAddMessages(t, session, []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "explain"},
		{Role: messages.MessageRoleAssistant, Reasoning: "durable chain of thought", Content: "answer"},
	})
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopenedStore := testOpenDiskStore(t, dbPath, nil)
	reopened := testAcquireSession(t, reopenedStore, "reasoning-reload")
	m := newReplModel()
	m.hydrateHistory(testSessionHistory(t, reopened), "reasoning-reload")
	if len(m.reasoningOrder) != 1 {
		t.Fatalf("reloaded reasoning disclosures = %d, want 1", len(m.reasoningOrder))
	}
	record := m.reasoningRecords[m.reasoningOrder[0]]
	if record == nil || !record.complete || record.unsaved || record.expanded {
		t.Fatalf("reloaded reasoning record = %#v", record)
	}
	if !m.toggleReasoning(record.id, 80) || !strings.Contains(plainStyledText(m.transcript[record.transcriptIndex]), "durable chain of thought") {
		t.Fatalf("reloaded completed disclosure did not expand: %q", m.transcript[record.transcriptIndex])
	}
}

func TestCompletedToolDisclosureSurvivesDiskReloadWithoutRawResults(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "tools.db")
	store := testOpenDiskStore(t, dbPath, nil)
	session := testAcquireSession(t, store, "tools-reload")
	toolResult := messages.ChatMessage{
		Role:       messages.MessageRoleTool,
		ToolCallID: "1",
		ToolName:   "bash",
		Content:    "RAW SECRET TOOL BODY",
	}
	toolResult.SetToolSucceeded(true)
	testAddMessages(t, session, []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "run"},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "1", Name: "bash"}}},
		toolResult,
		{Role: messages.MessageRoleAssistant, Content: "done"},
	})
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopenedStore := testOpenDiskStore(t, dbPath, nil)
	reopened := testAcquireSession(t, reopenedStore, "tools-reload")
	m := newReplModel()
	m.hydrateHistory(testSessionHistory(t, reopened), "tools-reload")
	if len(m.toolDisclosures) != 1 {
		t.Fatalf("reloaded tool disclosures = %d, want 1", len(m.toolDisclosures))
	}
	var record *toolDisclosureRecord
	for _, candidate := range m.toolDisclosures {
		record = candidate
	}
	collapsed := plainStyledText(strings.Join(m.transcript, "\n"))
	if record == nil || !record.complete || record.expanded || !strings.Contains(collapsed, "▸ 1 tool") || strings.Contains(collapsed, "RAW SECRET") {
		t.Fatalf("reloaded collapsed tool disclosure = %#v transcript=%q", record, collapsed)
	}
	if !m.toggleToolDisclosure(record.id) {
		t.Fatal("reloaded tool disclosure did not expand")
	}
	expanded := plainStyledText(m.transcript[record.transcriptIndex])
	if !strings.Contains(expanded, "✓ bash") || strings.Contains(expanded, "RAW SECRET") {
		t.Fatalf("reloaded tool detail = %q", expanded)
	}
}

func TestHydrateHistoryRestoresTrailingUserToComposer(t *testing.T) {
	m := newReplModel()
	m.hydrateHistory([]messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "unanswered"}}, "ctx")
	if m.ed.text() != "unanswered" || m.restoredDraft == nil {
		t.Fatalf("restored draft = %#v, editor=%q", m.restoredDraft, m.ed.text())
	}
	if joined := strings.Join(m.flattenTranscript(), "\n"); !strings.Contains(joined, "incomplete · restored to composer") {
		t.Fatalf("incomplete resume marker missing: %q", joined)
	}
}

func TestHydrateHistoryDoesNotExposeOrRestoreAttachedTextBodies(t *testing.T) {
	history := []messages.ChatMessage{{
		Role: messages.MessageRoleUser,
		Parts: []messages.ContentPart{
			{Type: "text", Text: "inspect this"},
			{Type: "text", Text: "TOP SECRET FILE BODY", FileName: "notes.txt"},
		},
	}}
	m := newReplModel()
	m.hydrateHistory(history, "ctx")
	plain := plainStyledText(strings.Join(m.flattenTranscript(), "\n"))
	if !strings.Contains(plain, "inspect this") || !strings.Contains(plain, "attached: notes.txt") {
		t.Fatalf("attachment summary missing prompt or filename: %q", plain)
	}
	if strings.Contains(plain, "TOP SECRET FILE BODY") {
		t.Fatalf("attachment body leaked into resumed transcript: %q", plain)
	}
	if m.restoredDraft != nil || !m.ed.empty() || strings.Contains(plain, "restored to composer") {
		t.Fatalf("parts-based message should not restore a lossy draft: draft=%#v transcript=%q", m.restoredDraft, plain)
	}
}

func TestHydrateHistoryOnlyRestoresPortablePersistedImages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "valid.png")
	writeImageFixture(t, path, 2, 2)
	validPart, err := prepareImageForUpload(path)
	if err != nil {
		t.Fatal(err)
	}
	validMessage := messages.ChatMessage{
		Role: messages.MessageRoleUser,
		Parts: []messages.ContentPart{
			{Type: "text", Text: "inspect"},
			*validPart,
		},
	}
	tooMany := validMessage
	tooMany.Parts = []messages.ContentPart{{Type: "text", Text: "inspect"}}
	for i := 0; i <= maxPromptAttachments; i++ {
		tooMany.Parts = append(tooMany.Parts, *validPart)
	}

	for _, tc := range []struct {
		name       string
		msg        messages.ChatMessage
		restorable bool
	}{
		{name: "portable PNG", msg: validMessage, restorable: true},
		{name: "invalid base64", msg: messages.ChatMessage{Role: messages.MessageRoleUser, Parts: []messages.ContentPart{{Type: "image_base64", ImageData: "%%%", MimeType: "image/png"}}}},
		{name: "GIF", msg: messages.ChatMessage{Role: messages.MessageRoleUser, Parts: []messages.ContentPart{{Type: "image_base64", ImageData: "R0lGODlhAQABAIAAAAAAAP///ywAAAAAAQABAAACAUwAOw==", MimeType: "image/gif"}}}, restorable: true},
		{name: "SVG", msg: messages.ChatMessage{Role: messages.MessageRoleUser, Parts: []messages.ContentPart{{Type: "image_base64", ImageData: base64.StdEncoding.EncodeToString([]byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)), MimeType: "image/svg+xml"}}}},
		{name: "MIME mismatch", msg: messages.ChatMessage{Role: messages.MessageRoleUser, Parts: []messages.ContentPart{{Type: "image_base64", ImageData: validPart.ImageData, MimeType: "image/jpeg"}}}, restorable: true},
		{name: "17 images", msg: tooMany},
		{name: "over aggregate budget", msg: messages.ChatMessage{Role: messages.MessageRoleUser, Parts: []messages.ContentPart{{Type: "image_base64", ImageData: strings.Repeat("A", maxEncodedImageHistoryBytes+1), MimeType: "image/png"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newReplModel()
			m.hydrateHistory([]messages.ChatMessage{tc.msg}, "ctx")
			plain := plainStyledText(strings.Join(m.flattenTranscript(), "\n"))
			if got := m.restoredDraft != nil; got != tc.restorable {
				t.Fatalf("restorable=%v, want %v; transcript=%q", got, tc.restorable, plain)
			}
			if got := strings.Contains(plain, "restored to composer"); got != tc.restorable {
				t.Fatalf("restore marker=%v, want %v; transcript=%q", got, tc.restorable, plain)
			}
		})
	}
}

func TestHydrateHistoryRestoresThroughNormalizedEarlierImage(t *testing.T) {
	history := []messages.ChatMessage{
		{
			Role: messages.MessageRoleUser,
			Parts: []messages.ContentPart{{
				Type: "image_base64", ImageData: "R0lGODlhAQABAIAAAAAAAP///ywAAAAAAQABAAACAUwAOw==", MimeType: "image/gif",
			}},
		},
		{Role: messages.MessageRoleAssistant, Content: "old response"},
		{Role: messages.MessageRoleUser, Content: "restore this text"},
	}
	m := newReplModel()
	m.hydrateHistory(history, "poisoned")
	plain := plainStyledText(strings.Join(m.flattenTranscript(), "\n"))
	if m.restoredDraft == nil || m.ed.text() != "restore this text" || !strings.Contains(plain, "restored to composer") {
		t.Fatalf("normalized earlier image suppressed draft restoration: %q", plain)
	}
}

func TestHydrateHistoryCompactsContextImportedFiles(t *testing.T) {
	for _, tc := range []struct {
		msg  messages.ChatMessage
		want string
	}{
		{msg: messages.ChatMessage{
			Role:    messages.MessageRoleUser,
			Content: "=== legacy.txt ===\nLEGACY SECRET BODY",
		}, want: "attached: legacy.txt"},
		{msg: messages.ChatMessage{
			Role: messages.MessageRoleUser,
			Parts: []messages.ContentPart{{
				Type: "text", Text: "CURRENT SECRET BODY", FileName: "current.txt",
			}},
			Metadata: map[string]any{messages.MetadataKeyContextImport: true},
		}, want: "attached: current.txt"},
		{msg: messages.ChatMessage{
			Role:     messages.MessageRoleUser,
			Content:  "IMPORTED STDIN SECRET BODY",
			Metadata: map[string]any{messages.MetadataKeyContextImport: true},
		}, want: "context added"},
	} {
		m := newReplModel()
		m.hydrateHistory([]messages.ChatMessage{tc.msg}, "ctx")
		plain := plainStyledText(strings.Join(m.flattenTranscript(), "\n"))
		if !strings.Contains(plain, tc.want) || strings.Contains(plain, "SECRET BODY") {
			t.Fatalf("context import was not compacted: %q", plain)
		}
		if strings.Contains(plain, "incomplete") || m.restoredDraft != nil || !m.ed.empty() {
			t.Fatalf("context import was treated as a failed turn: %q draft=%#v", plain, m.restoredDraft)
		}
	}
}

func TestHydrateHistoryTreatsDeniedTurnMarkerAsCompleted(t *testing.T) {
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "run it"},
		{
			Role: messages.MessageRoleInternal,
			Metadata: map[string]any{
				messages.MetadataKeyTurnStatus: messages.TurnStatusToolDenied,
			},
		},
	}
	m := newReplModel()
	m.hydrateHistory(history, "ctx")
	plain := plainStyledText(strings.Join(m.flattenTranscript(), "\n"))
	if !strings.Contains(plain, "tool request denied") || strings.Contains(plain, "incomplete") || m.restoredDraft != nil || !m.ed.empty() {
		t.Fatalf("denied completion hydration = %q draft=%#v", plain, m.restoredDraft)
	}
}

func TestQuietToolBoundaryDoesNotConcatenateAssistantIterations(t *testing.T) {
	r := newManagedREPL(&Config{Quiet: true}, "ctx", 0, 0)
	r.model.beginTurn("run it")
	tui := &gotuiTurnUI{repl: r, config: r.config}
	tui.AppendAssistantText("before")
	tui.AppendToolStart([]messages.ChatMessageToolCall{{ID: "1", Name: "bash"}})
	tui.AppendAssistantText("after")
	tui.FinishTextTurn()

	got := r.model.flattenTranscript()
	if len(got) != 3 || got[1] != "before" || got[2] != "after" {
		t.Fatalf("quiet tool boundary = %#v, want separate assistant blocks", got)
	}
}

func TestVisualRowScrollHandlesWrappedParagraphs(t *testing.T) {
	m := newReplModel()
	m.appendLine(strings.Repeat("word ", 20))
	m.appendLine("tail")
	total := len(transcriptVisualRows(m.fullTranscript(), ui.NewStyle(ui.ColorClear), 10))
	if total <= 3 {
		t.Fatalf("fixture did not wrap: %d rows", total)
	}
	m.scrollByWidth(-2, 3, 10)
	if m.followBottom {
		t.Fatal("scrolling up in wrapped content should disengage follow-bottom")
	}
	anchor := m.scrollAnchor
	m.appendLine("new output")
	if m.scrollAnchor != anchor {
		t.Fatalf("new output moved held visual anchor: %d -> %d", anchor, m.scrollAnchor)
	}
	m.scrollByWidth(1000, 3, 10)
	if !m.followBottom {
		t.Fatal("scrolling to the last visual row should re-engage follow-bottom")
	}
}

func TestTranscriptVisualCacheReusesUnchangedBlocksAndTracksHints(t *testing.T) {
	m := newReplModel()
	m.appendLine("a stable earlier transcript block")
	m.appendToolStartLine("1", "bash sleep 30")
	rows1 := m.transcriptRows(80)
	if len(rows1) != 2 || len(m.visualBlocks[0].rows[0]) == 0 {
		t.Fatalf("cache fixture rows = %#v", rows1)
	}
	staticCell := &m.visualBlocks[0].rows[0][0]
	cacheStart := &rows1[0]

	m.activeTools[0].started = time.Now().Add(-2 * time.Second)
	m.refreshActiveTools()
	if !m.visualCacheValid {
		t.Fatal("collapsed live timer invalidated the visual cache")
	}
	rows2 := m.transcriptRows(80)
	if &m.visualBlocks[0].rows[0][0] != staticCell {
		t.Fatal("unchanged transcript block was reparsed")
	}
	if &rows2[0] != cacheStart {
		t.Fatal("same-row-count tool update rebuilt the full visual row index")
	}
	// Collapsed inline header hides the call detail but stays visible.
	if shown := strings.Join(transcriptRowsText(rows2), "\n"); strings.Contains(shown, "bash sleep 30") || !strings.Contains(shown, "1 tool") {
		t.Fatalf("collapsed inline tool block wrong: %q", shown)
	}
	record := m.currentToolDisclosure()
	if record == nil || !m.toggleToolDisclosure(record.id) {
		t.Fatalf("cached tool disclosure did not expand: %#v", record)
	}
	if m.visualCacheValid {
		t.Fatal("inline disclosure expansion did not invalidate the visual cache")
	}
	expandedRows := m.transcriptRows(80)
	if shown := strings.Join(transcriptRowsText(expandedRows), "\n"); !strings.Contains(shown, "bash sleep 30") {
		t.Fatalf("expanded inline disclosure missing detail: %q", shown)
	}

	m.setSlashHintLine("/help  /history")
	withHints := len(m.transcriptRows(80))
	if withHints <= len(expandedRows) {
		t.Fatal("slash hints did not invalidate the visual row cache")
	}
	m.setSlashHintLine("")
	if got := len(m.transcriptRows(80)); got != len(expandedRows) {
		t.Fatalf("clearing slash hints left stale cached rows: %d, want %d", got, len(expandedRows))
	}
}

func TestTranscriptBlockCacheMatchesJoinedRenderer(t *testing.T) {
	m := newReplModel()
	for _, entry := range []string{
		styled("muted one\nmuted two\n", "muted", ""),
		"",
		styled("> ", "accent", "bold") + "a prompt that wraps across rows",
	} {
		m.appendTranscriptEntry(entry)
	}
	m.invalidateVisual()
	m.setSlashHintLine("/help  /tools")
	for _, width := range []int{4, 12, 40} {
		got := m.transcriptRows(width)
		want := transcriptVisualRows(m.fullTranscript(), ui.NewStyle(ui.ColorClear), width)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("width %d cached rows diverged from joined renderer:\n got=%#v\nwant=%#v", width, got, want)
		}
	}
}

func TestRestoredDraftResubmitsExactUserAfterDiscardingPendingInput(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	r.model.beginTurn("same prompt")
	r.model.ed.setText("later")
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})
	r.endTurn(errors.New("provider failed"))
	if r.model.ed.text() != "same prompt" || len(r.model.queue) != 0 {
		t.Fatalf("failure did not restore input and discard queue: editor=%q queue=%v", r.model.ed.text(), r.model.queue)
	}
	if joined := plainStyledText(r.model.fullTranscript()); !strings.Contains(joined, "> later\n  (not sent)") {
		t.Fatalf("discarded pending input was not marked unsent: %q", joined)
	}

	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})
	turn, ok := r.takePending()
	if !ok || turn.displayText != "same prompt" || !r.model.busy || !r.model.restoreDraftNext {
		t.Fatalf("restored submit state: turn=%#v busy=%v reuse=%v", turn, r.model.busy, r.model.restoreDraftNext)
	}

	reuseSeen := make(chan bool, 1)
	done := r.startManagedTurn(context.Background(), turn, func(_ context.Context, _ string, turnUI TurnUI) error {
		reuseSeen <- turnUI.(*gotuiTurnUI).reuseUser
		return nil
	})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !<-reuseSeen {
		t.Fatal("unchanged restored draft did not reuse its persisted user")
	}
	r.endTurn(nil)
	if len(r.model.queue) != 0 {
		t.Fatalf("successful resubmission recreated discarded queue: %v", r.model.queue)
	}
}

func TestEditedRestoredDraftIsOrdinaryNewTurn(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	r.model.beginTurn("original")
	r.endTurn(errors.New("provider failed"))
	r.model.ed.setText("edited")
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})
	turn, ok := r.takePending()
	if !ok || turn.displayText != "edited" || r.model.restoreDraftNext {
		t.Fatalf("edited draft was not a normal turn: turn=%#v reuse=%v", turn, r.model.restoreDraftNext)
	}
}

func TestCancellationRestoresInputWithoutOverwritingNewerDraft(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	r.model.beginTurn("canceled prompt")
	r.model.ed.setText("newer draft")
	r.endTurn(context.Canceled)
	if got := r.model.ed.text(); got != "newer draft" {
		t.Fatalf("cancellation overwrote newer draft: %q", got)
	}
	if r.model.restoredDraft == nil || r.model.restoredDraft.displayText != "canceled prompt" {
		t.Fatalf("canceled input was not retained for history recall: %#v", r.model.restoredDraft)
	}
	if plain := plainStyledText(r.model.fullTranscript()); !strings.Contains(plain, "input available with ↑ · current draft preserved") {
		t.Fatalf("preserved-draft notice missing: %q", plain)
	}
}

type failingClearSession struct {
	sessions.Session
}

func (failingClearSession) Clear(context.Context) error { return errors.New("clear failed") }

func TestQueuedResetIsAConsistentBarrier(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	session := testAcquireSession(t, store, "queued-reset")
	if err := session.AddMessage(context.Background(), messages.ChatMessage{Role: messages.MessageRoleUser, Content: "old"}); err != nil {
		t.Fatal(err)
	}

	t.Run("success preserves and starts following input", func(t *testing.T) {
		r := newManagedREPL(&Config{}, "ctx", 0, 0)
		r.state = &conversationState{session: session}
		r.model.queue = queuedTextInputs("/reset confirm", "fresh")
		started := make(chan string, 1)
		done := r.startNextQueued(context.Background(), func(_ context.Context, prompt string, _ TurnUI) error {
			started <- prompt
			return nil
		})
		if done == nil || <-started != "fresh" {
			t.Fatal("successful queued reset dropped or failed to start following input")
		}
		if got := len(testSessionHistory(t, session)); got != 0 {
			t.Fatalf("reset left %d durable messages", got)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})

	t.Run("failure marks following input not sent", func(t *testing.T) {
		r := newManagedREPL(&Config{}, "ctx", 0, 0)
		r.state = &conversationState{session: failingClearSession{Session: session}}
		r.model.queue = queuedTextInputs("/reset confirm", "must wait")
		for i := range r.model.queue {
			r.model.appendQueuedInput(&r.model.queue[i])
		}
		if done := r.startNextQueued(context.Background(), func(context.Context, string, TurnUI) error {
			t.Fatal("following prompt ran after reset failure")
			return nil
		}); done != nil {
			t.Fatal("reset failure unexpectedly started a turn")
		}
		if len(r.model.queue) != 0 {
			t.Fatalf("reset failure left queue=%v", r.model.queue)
		}
		if joined := plainStyledText(r.model.fullTranscript()); !strings.Contains(joined, "> must wait\n  (not sent)") {
			t.Fatalf("reset failure did not mark following input unsent: %q", joined)
		}
	})
}
