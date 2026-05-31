package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	ui "github.com/metaspartan/gotui/v5"
)

// Fix 1: --confirm in a non-interactive context (no TTY) must auto-approve tool
// calls, not read approvals from a non-tty stdin — which EOFs immediately and
// denies everything. The test process has no controlling terminal, so
// isTerminal() is false here.
func TestLineTurnUIAutoApprovesWhenNotInteractive(t *testing.T) {
	tui := newLineTurnUI(&Config{Confirm: true}, bufio.NewReader(strings.NewReader("")))
	approved := tui.ApproveToolCalls([]messages.ChatMessageToolCall{{Name: "bash"}})
	if len(approved) != 1 || !approved[0] {
		t.Fatalf("non-interactive --confirm should auto-approve, got %v", approved)
	}
}

func TestLineTurnUIAutoApprovesWhenStdinIsNotTTY(t *testing.T) {
	old := terminalFD
	terminalFD = func(fd int) bool {
		switch fd {
		case int(os.Stdin.Fd()):
			return false
		case int(os.Stdout.Fd()), int(os.Stderr.Fd()):
			return true
		default:
			return old(fd)
		}
	}
	t.Cleanup(func() { terminalFD = old })

	tui := newLineTurnUI(&Config{Confirm: true}, bufio.NewReader(strings.NewReader("")))
	if tui.approver != nil {
		t.Fatal("piped stdin must not create an approver, even when stdout/stderr are TTYs")
	}
	approved := tui.ApproveToolCalls([]messages.ChatMessageToolCall{{Name: "bash"}})
	if len(approved) != 1 || !approved[0] {
		t.Fatalf("piped stdin should auto-approve instead of denying on EOF, got %v", approved)
	}
}

// Fix 2: a parallel tool batch must keep the status in the "tool" state until
// the LAST tool finishes; the first one to complete must not flip the bar back
// to "waiting" while its siblings are still running.
func TestStatusStaysToolUntilParallelToolsAllFinish(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.busy = true
	tui := &gotuiTurnUI{repl: r, config: r.config}

	calls := []messages.ChatMessageToolCall{{ID: "a", Name: "bash"}, {ID: "b", Name: "grep"}}
	tui.AppendToolStart(calls)
	if m.state != turnStateTool {
		t.Fatalf("state after start = %v, want tool", m.state)
	}

	tui.AppendToolEnd(calls[0], "ok", time.Second, nil)
	if m.state != turnStateTool {
		t.Fatalf("state after 1 of 2 tools finished = %v, want tool (one still running)", m.state)
	}

	tui.AppendToolEnd(calls[1], "ok", time.Second, nil)
	if m.state != turnStateWaiting {
		t.Fatalf("state after all tools finished = %v, want waiting", m.state)
	}
	if m.toolName != "" {
		t.Fatalf("toolName should clear once all tools finished, got %q", m.toolName)
	}
}

// Fix 3: a recoverable per-turn error must be shown and the loop must keep
// going, not drop the user back to the shell on the first hiccup.
func TestRunREPLLoopContinuesAfterTurnError(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader("first\nsecond\n"))
	var out bytes.Buffer
	var seen []string
	err := runREPLLoop(reader, &out, func(prompt string) error {
		seen = append(seen, prompt)
		if prompt == "first" {
			return fmt.Errorf("boom")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("recoverable turn error should not propagate, got %v", err)
	}
	if len(seen) != 2 || seen[0] != "first" || seen[1] != "second" {
		t.Fatalf("loop should process both lines, got %v", seen)
	}
	if !strings.Contains(out.String(), "boom") {
		t.Fatalf("error should be shown to the user, got %q", out.String())
	}
}

// Fix 3: context cancellation ends the loop cleanly (no error, no spin) rather
// than printing the same error forever.
func TestRunREPLLoopStopsWhenContextCanceled(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader("first\nsecond\n"))
	var out bytes.Buffer
	n := 0
	err := runREPLLoop(reader, &out, func(prompt string) error {
		n++
		return context.Canceled
	})
	if err != nil {
		t.Fatalf("context cancellation should end the loop cleanly, got %v", err)
	}
	if n != 1 {
		t.Fatalf("loop should stop after the cancelled turn, ran %d turns", n)
	}
}

// Fix 4: empty/whitespace structured output is an error (e.g. all tool calls
// denied), not a silently-printed blank line.
func TestOutputStructuredEmptyContentErrors(t *testing.T) {
	if err := outputStructured("", nil); err == nil {
		t.Fatal("empty content should error, not print a blank line")
	}
	if err := outputStructured("  \n\t ", nil); err == nil {
		t.Fatal("whitespace-only content should error")
	}
}

func TestOutputStructuredValidJSONReturnsNil(t *testing.T) {
	// Capture stdout so the success path stays quiet during the test run.
	old := os.Stdout
	rp, wp, _ := os.Pipe()
	os.Stdout = wp
	err := outputStructured(`{"a":1}`, nil)
	wp.Close()
	os.Stdout = old
	_, _ = io.Copy(io.Discard, rp)
	if err != nil {
		t.Fatalf("valid JSON content should not error, got %v", err)
	}
}

// Fix 5: flattenTranscript is cached, but every mutation kind must show through
// — appends, multi-line expansion, streamed text, and in-place tool refreshes.
func TestFlattenTranscriptReflectsMutations(t *testing.T) {
	m := newReplModel()

	m.appendLine("one")
	if got := m.flattenTranscript(); len(got) != 1 || got[0] != "one" {
		t.Fatalf("after first append: %v", got)
	}

	m.appendLine("two\nthree")
	got := m.flattenTranscript()
	if len(got) != 3 || got[0] != "one" || got[1] != "two" || got[2] != "three" {
		t.Fatalf("after multi-line append: %v", got)
	}

	m.appendAssistant("strea")
	m.appendAssistant("ming")
	got = m.flattenTranscript()
	if got[len(got)-1] != "streaming" {
		t.Fatalf("streamed text not reflected through cache: %v", got)
	}

	m.appendToolStartLine("id", "bash")
	m.activeTools[0].started = m.activeTools[0].started.Add(-2 * time.Second)
	m.refreshActiveTools()
	got = m.flattenTranscript()
	if !strings.Contains(got[len(got)-1], "2.0s") {
		t.Fatalf("in-place tool refresh not reflected through cache: %q", got[len(got)-1])
	}
}

// Fix 5: streaming many small chunks into one assistant entry concatenates
// correctly (the Builder-backed accumulation must not drop or reorder text).
func TestAppendAssistantAccumulatesManyChunks(t *testing.T) {
	m := newReplModel()
	var want strings.Builder
	for i := 0; i < 500; i++ {
		chunk := fmt.Sprintf("c%d ", i)
		m.appendAssistant(chunk)
		want.WriteString(chunk)
	}
	if len(m.transcript) != 1 {
		t.Fatalf("streaming should stay in one entry, got %d", len(m.transcript))
	}
	if m.transcript[0] != want.String() {
		t.Fatalf("accumulated text mismatch:\n got %q\nwant %q", m.transcript[0], want.String())
	}
}

// Fix 8: denial is recognized through one named predicate that owns the
// llm.ToolDeniedContent sentinel contract, rather than the bare string compare
// previously duplicated across both tool-end renderers.
func TestToolWasDenied(t *testing.T) {
	if !toolWasDenied(llm.ToolDeniedContent) {
		t.Fatal("the denial sentinel must be recognized as denied")
	}
	if toolWasDenied("real tool output") {
		t.Fatal("ordinary tool output must not be treated as denied")
	}
	if toolWasDenied("") {
		t.Fatal("empty output must not be treated as denied")
	}
}

// Fix 8: the gotui renderer keeps reporting a denied tool as "denied" (behavior
// preserved across the predicate extraction).
func TestToolEndRendersDenialLine(t *testing.T) {
	old := terminalFD
	terminalFD = func(int) bool { return true }
	t.Cleanup(func() { terminalFD = old })

	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	r.model.busy = true
	tui := &gotuiTurnUI{repl: r, config: r.config}

	call := messages.ChatMessageToolCall{ID: "a", Name: "bash"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	tui.AppendToolEnd(call, llm.ToolDeniedContent, 0, nil)

	last := r.model.transcript[len(r.model.transcript)-1]
	if !strings.Contains(last, "denied") {
		t.Fatalf("a denied tool should render a denied line, got %q", last)
	}
}

// Fix 9: tool-result durations render through formatElapsed for consistency
// with the status bar. A 90s tool must read "1m30s", not the hand-rolled
// "%.1fs" that printed "90.0s" in the tool line while the bar showed "1m30s".
func TestToolEndDurationUsesFormatElapsed(t *testing.T) {
	old := terminalFD
	terminalFD = func(int) bool { return true }
	t.Cleanup(func() { terminalFD = old })

	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.busy = true
	tui := &gotuiTurnUI{repl: r, config: r.config}

	call := messages.ChatMessageToolCall{ID: "a", Name: "bash"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	tui.AppendToolEnd(call, "ok", 90*time.Second, nil)

	last := m.transcript[len(m.transcript)-1]
	if !strings.Contains(last, "1m30s") {
		t.Fatalf("ok tool line should use formatElapsed (1m30s), got %q", last)
	}
	if strings.Contains(last, "90.0s") {
		t.Fatalf("ok tool line should not hand-roll %%.1fs (90.0s), got %q", last)
	}
}

func TestToolEndErrorDurationUsesFormatElapsed(t *testing.T) {
	old := terminalFD
	terminalFD = func(int) bool { return true }
	t.Cleanup(func() { terminalFD = old })

	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	r.model.busy = true
	tui := &gotuiTurnUI{repl: r, config: r.config}

	call := messages.ChatMessageToolCall{ID: "a", Name: "bash"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	tui.AppendToolEnd(call, "boom", 90*time.Second, fmt.Errorf("boom"))

	last := r.model.transcript[len(r.model.transcript)-1]
	if !strings.Contains(last, "1m30s") {
		t.Fatalf("failed tool line should use formatElapsed (1m30s), got %q", last)
	}
}

func TestLineTurnUIToolEndDurationUsesFormatElapsed(t *testing.T) {
	old := terminalFD
	terminalFD = func(int) bool { return true }
	t.Cleanup(func() { terminalFD = old })

	var errOut bytes.Buffer
	tui := newLineTurnUI(&Config{}, nil)
	tui.errWriter = &errOut

	tui.AppendToolEnd(messages.ChatMessageToolCall{Name: "bash"}, "ok", 90*time.Second, nil)
	if !strings.Contains(errOut.String(), "1m30s") {
		t.Fatalf("line UI tool result should use formatElapsed, got %q", errOut.String())
	}
}

// Fix 6: the periodic render tick only repaints while a turn is live (spinner /
// breathing tool arrow / elapsed timers animate); at idle nothing changes so
// the tick is a no-op.
func TestNeedsTickOnlyWhileBusy(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	if r.needsTick() {
		t.Fatal("idle REPL should not request animation ticks")
	}
	r.model.busy = true
	if !r.needsTick() {
		t.Fatal("busy REPL should request animation ticks")
	}
}

// Fix 7: bracketed-paste content is coalesced into a single repaint — no redraw
// per pasted rune, one redraw when the paste closes.
func TestWantsRenderForEventCoalescesPaste(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)

	if r.wantsRenderForEvent(ui.Event{Type: ui.KeyboardEvent, ID: pasteStartID}) {
		t.Fatal("paste start should not trigger a repaint")
	}

	r.model.pasting = true
	if r.wantsRenderForEvent(ui.Event{Type: ui.KeyboardEvent, ID: "a"}) {
		t.Fatal("paste content should not trigger a per-rune repaint")
	}

	r.model.pasting = false // handleEvent clears this on the closing marker
	if !r.wantsRenderForEvent(ui.Event{Type: ui.KeyboardEvent, ID: pasteEndID}) {
		t.Fatal("paste end should trigger a single repaint")
	}
	if !r.wantsRenderForEvent(ui.Event{Type: ui.KeyboardEvent, ID: "x"}) {
		t.Fatal("a normal keystroke should always repaint")
	}
}
