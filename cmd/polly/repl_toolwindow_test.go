package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
)

// settleToolCall freezes a running row into its outcome, mirroring what
// AppendToolEnd does to the model.
func settleToolCall(m *replModel, id, label string) {
	if idx, ok := m.takeActiveTool(id); ok && idx >= 0 {
		m.transcript[idx] = toolOKLine(label, "0.1s", "")
	}
	m.foldToolWindow()
}

// runToolCall drives one call start-to-finish, the shape of a sequential
// turn where each batch holds a single tool.
func runToolCall(m *replModel, id, label string) {
	m.appendToolStartLine(id, label)
	settleToolCall(m, id, label)
}

// toolRows returns the transcript entries that are tool rows or the rollup,
// so assertions read against the visible activity area rather than indices.
func toolRows(m *replModel) []string {
	var rows []string
	for _, e := range m.transcript {
		if strings.Contains(e, "→") || strings.Contains(e, "✓") ||
			strings.Contains(e, "✗") || strings.Contains(e, "tool calls") {
			rows = append(rows, e)
		}
	}
	return rows
}

func TestToolWindowKeepsLastThreeAndRollsUpTheRest(t *testing.T) {
	m := newReplModel()

	// The first three calls all stay visible; no rollup line exists yet.
	for i := 1; i <= toolWindowSize; i++ {
		runToolCall(m, fmt.Sprintf("id%d", i), fmt.Sprintf("bash cmd%d", i))
	}
	if len(m.transcript) != toolWindowSize || m.toolRollupIndex != -1 {
		t.Fatalf("under the cap: %d entries, rollup at %d", len(m.transcript), m.toolRollupIndex)
	}

	// The fourth turns the oldest row into the rollup: the area stays four
	// lines tall (rollup + three rows) no matter how many more calls run.
	for i := toolWindowSize + 1; i <= 9; i++ {
		runToolCall(m, fmt.Sprintf("id%d", i), fmt.Sprintf("bash cmd%d", i))
	}
	if len(m.transcript) != toolWindowSize+1 {
		t.Fatalf("transcript = %d entries, want %d: %v", len(m.transcript), toolWindowSize+1, m.transcript)
	}
	if m.toolRollupIndex != 0 {
		t.Fatalf("rollup index = %d, want 0", m.toolRollupIndex)
	}
	if !strings.Contains(m.transcript[0], "9 tool calls") {
		t.Fatalf("rollup should count every call this turn: %q", m.transcript[0])
	}
	// The survivors are the three most recently started, in order.
	for offset, want := range []string{"cmd7", "cmd8", "cmd9"} {
		if !strings.Contains(m.transcript[1+offset], want) {
			t.Fatalf("row %d = %q, want %s", offset, m.transcript[1+offset], want)
		}
	}
	if strings.Contains(strings.Join(m.transcript, "\n"), "cmd6") {
		t.Fatal("cmd6 folded away but is still rendered")
	}
}

func TestToolRollupHoldsPositionWhileProseInterleaves(t *testing.T) {
	m := newReplModel()

	runToolCall(m, "a", "bash one")
	m.appendAssistant("thinking about it")
	m.finishAssistantBlock("")
	for _, id := range []string{"b", "c", "d"} {
		runToolCall(m, id, "bash "+id)
	}

	// The rollup replaces the first tool row in place, so prose written after
	// it keeps its original position relative to the activity.
	if m.toolRollupIndex != 0 || !strings.Contains(m.transcript[0], "4 tool calls") {
		t.Fatalf("rollup = %d %q", m.toolRollupIndex, m.transcript[0])
	}
	if !strings.Contains(m.transcript[1], "thinking about it") {
		t.Fatalf("assistant prose moved or was folded: %v", m.transcript)
	}
	if got := len(toolRows(m)); got != toolWindowSize+1 {
		t.Fatalf("tool rows = %d, want %d", got, toolWindowSize+1)
	}
}

// A parallel batch is announced in one AppendToolStart and every call in it
// runs at once. Folding waits for the batch so each call still shows its own
// outcome, then contracts to the cap.
func TestParallelBatchStaysWholeUntilItSettles(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.busy = true
	tui := &gotuiTurnUI{repl: r, config: r.config}

	calls := make([]messages.ChatMessageToolCall, 6)
	for i := range calls {
		calls[i] = messages.ChatMessageToolCall{ID: fmt.Sprintf("c%d", i), Name: "bash"}
	}
	tui.AppendToolStart(calls)
	if len(toolRows(m)) != len(calls) {
		t.Fatalf("a running batch must stay whole: %d rows, want %d", len(toolRows(m)), len(calls))
	}

	// Outcomes land on their own rows while siblings are still in flight —
	// including the failures, which must never fold away unseen.
	tui.AppendToolEnd(calls[0], "boom", time.Second, fmt.Errorf("boom"))
	if got := len(toolRows(m)); got != len(calls) {
		t.Fatalf("rows changed while the batch is still running: %d", got)
	}
	if !strings.Contains(strings.Join(m.transcript, "\n"), "✗") {
		t.Fatalf("the failed call should show its outcome: %v", m.transcript)
	}
	for _, c := range calls[1:5] {
		tui.AppendToolEnd(c, "ok", time.Second, nil)
	}

	// The last call settling releases the window: it contracts in one step.
	tui.AppendToolEnd(calls[5], "ok", time.Second, nil)
	if got := len(toolRows(m)); got != toolWindowSize+1 {
		t.Fatalf("settled batch = %d rows, want %d", got, toolWindowSize+1)
	}
	if !strings.Contains(m.transcript[m.toolRollupIndex], "6 tool calls") {
		t.Fatalf("rollup = %q", m.transcript[m.toolRollupIndex])
	}
	if m.runningTools != 0 {
		t.Fatalf("runningTools = %d, want 0", m.runningTools)
	}
}

// The window cannot accumulate across a turn: the agent starts no new call
// while one is in flight, so back-to-back batches each contract to the cap.
func TestWindowContractsBetweenBatches(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.busy = true
	tui := &gotuiTurnUI{repl: r, config: r.config}

	for batch := 0; batch < 3; batch++ {
		calls := make([]messages.ChatMessageToolCall, 5)
		for i := range calls {
			calls[i] = messages.ChatMessageToolCall{ID: fmt.Sprintf("b%d-%d", batch, i), Name: "bash"}
		}
		tui.AppendToolStart(calls)
		for _, c := range calls {
			tui.AppendToolEnd(c, "ok", time.Millisecond, nil)
		}
		if got := len(toolRows(m)); got != toolWindowSize+1 {
			t.Fatalf("after batch %d: %d rows, want %d", batch, got, toolWindowSize+1)
		}
	}
	if !strings.Contains(m.transcript[m.toolRollupIndex], "15 tool calls") {
		t.Fatalf("rollup should total the whole turn: %q", m.transcript[m.toolRollupIndex])
	}
}

// An interrupted turn leaves rows that never got an OnToolEnd; they settle to
// a terminal glyph and then contract like any other batch.
func TestInterruptedBatchContractsAfterSettling(t *testing.T) {
	m := newReplModel()
	for i := 0; i < 6; i++ {
		m.appendToolStartLine(fmt.Sprintf("x%d", i), fmt.Sprintf("bash cmd%d", i))
	}
	if len(toolRows(m)) != 6 {
		t.Fatalf("running batch = %d rows, want 6", len(toolRows(m)))
	}

	m.settleActiveTools("canceled")
	m.activeTools = nil
	m.foldToolWindow()
	if got := len(toolRows(m)); got != toolWindowSize+1 {
		t.Fatalf("canceled batch = %d rows, want %d", got, toolWindowSize+1)
	}
	if !strings.Contains(m.transcript[m.toolRollupIndex], "6 tool calls") {
		t.Fatalf("rollup = %q", m.transcript[m.toolRollupIndex])
	}
}

func TestFoldingKeepsImagesWithTheirSurvivingRows(t *testing.T) {
	withDisplayTTY(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "shot.png")
	writeImageFixture(t, path, 4, 4)

	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.imageBaseDir = dir
	m.busy = true
	tui := &gotuiTurnUI{repl: r, config: r.config}

	// An image lands on the third call, which later shifts down as older
	// rows are deleted. Its sidecar must ride along with the row.
	for i := 0; i < 2; i++ {
		call := messages.ChatMessageToolCall{ID: fmt.Sprintf("s%d", i), Name: "bash"}
		tui.AppendToolStart([]messages.ChatMessageToolCall{call})
		tui.AppendToolEnd(call, "ok", time.Millisecond, nil)
	}
	shot := messages.ChatMessageToolCall{ID: "shot", Name: "screenshot"}
	tui.AppendToolStart([]messages.ChatMessageToolCall{shot})
	tui.AppendToolEnd(shot, path, time.Millisecond, nil)

	for i := 0; i < 3; i++ {
		call := messages.ChatMessageToolCall{ID: fmt.Sprintf("t%d", i), Name: "bash"}
		tui.AppendToolStart([]messages.ChatMessageToolCall{call})
		tui.AppendToolEnd(call, "ok", time.Millisecond, nil)
	}

	// Six calls: the screenshot row is now folded away, and no stale sidecar
	// may point at a row that no longer holds an image.
	for index, images := range m.transcriptImages {
		if len(images) == 0 {
			continue
		}
		if index >= len(m.transcript) || !strings.Contains(m.transcript[index], "image: shot.png") {
			t.Fatalf("sidecar at %d does not match its row %q", index, m.transcript[index])
		}
	}
}

func TestFoldingHoldsTheViewportStillWhenScrolledUp(t *testing.T) {
	m := newReplModel()

	// Two calls, then a long block of prose the user has scrolled back to
	// read, then more calls — so the next fold deletes a row above them.
	runToolCall(m, "a", "bash a")
	runToolCall(m, "b", "bash b")
	var prose []string
	for i := 0; i < 40; i++ {
		prose = append(prose, fmt.Sprintf("context line %d", i))
	}
	m.appendLine(strings.Join(prose, "\n"))
	runToolCall(m, "c", "bash c")
	runToolCall(m, "d", "bash d") // folds a into the rollup in place

	m.followBottom = false
	m.scrollAnchor = 20
	beforeTop := m.flattenTranscript()[m.scrollAnchor]

	// Deleting b, above the viewport, would otherwise slide every following
	// row up one under the reader's eyes.
	runToolCall(m, "e", "bash e")
	if m.scrollAnchor != 19 {
		t.Fatalf("scrollAnchor = %d, want 19 after one row folded above it", m.scrollAnchor)
	}
	if got := m.flattenTranscript()[m.scrollAnchor]; got != beforeTop {
		t.Fatalf("top visible row moved: %q -> %q", beforeTop, got)
	}

	// While following the bottom there is nothing to correct: the render
	// already trims to the tail.
	m.followBottom = true
	m.scrollAnchor = 19
	runToolCall(m, "f", "bash f")
	if m.scrollAnchor != 19 {
		t.Fatalf("scrollAnchor = %d, want it untouched while following", m.scrollAnchor)
	}
}

func TestToolWindowResetsPerTurn(t *testing.T) {
	m := newReplModel()
	for _, id := range []string{"a", "b", "c", "d"} {
		runToolCall(m, id, "bash "+id)
	}
	if m.turnToolCalls != 4 {
		t.Fatalf("turnToolCalls = %d, want 4", m.turnToolCalls)
	}

	m.beginManagedTurnState(textManagedTurn("next"))
	if m.turnToolCalls != 0 || m.toolRollupIndex != -1 || len(m.toolWindow) != 0 {
		t.Fatalf("new turn kept stale window state: %d calls, rollup %d, window %v",
			m.turnToolCalls, m.toolRollupIndex, m.toolWindow)
	}

	// The previous turn's rollup stays in scrollback untouched; the new turn
	// builds its own once it passes the cap.
	for _, id := range []string{"e", "f", "g", "h"} {
		runToolCall(m, id, "bash "+id)
	}
	rollups := 0
	for _, e := range m.transcript {
		if strings.Contains(e, "tool calls") {
			rollups++
		}
	}
	if rollups != 2 {
		t.Fatalf("rollups = %d, want one per turn", rollups)
	}
	if !strings.Contains(m.transcript[m.toolRollupIndex], "4 tool calls") {
		t.Fatalf("new rollup counts only its own turn: %q", m.transcript[m.toolRollupIndex])
	}
}
