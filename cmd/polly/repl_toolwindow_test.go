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
// runs at once. In-flight rows are never folded, so the batch opens at full
// height and then drains one row at a time as its calls settle — no single
// collapse at the end.
func TestParallelBatchDrainsAsItsCallsSettle(t *testing.T) {
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
		t.Fatalf("a fully running batch must stay whole: %d rows, want %d", len(toolRows(m)), len(calls))
	}

	// Each completion releases one row, so the area steps down by one rather
	// than standing at six and dropping to the cap all at once.
	wantRows := []int{6, 5, 4, 4, 4, 4}
	for i, c := range calls {
		tui.AppendToolEnd(c, "ok", time.Second, nil)
		if got := len(toolRows(m)); got != wantRows[i] {
			t.Fatalf("after %d of %d settled: %d rows, want %d", i+1, len(calls), got, wantRows[i])
		}
	}
	if !strings.Contains(m.transcript[m.toolRollupIndex], "6 tool calls") {
		t.Fatalf("rollup = %q", m.transcript[m.toolRollupIndex])
	}
	if m.runningTools != 0 {
		t.Fatalf("runningTools = %d, want 0", m.runningTools)
	}
}

// Calls fold in completion order, which for a parallel batch is not start
// order. The rollup must still end up above every row it stands for instead
// of stranded among the survivors.
func TestRollupStaysAboveSurvivorsWhenCallsFinishOutOfOrder(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.busy = true
	tui := &gotuiTurnUI{repl: r, config: r.config}

	calls := make([]messages.ChatMessageToolCall, 7)
	for i := range calls {
		calls[i] = messages.ChatMessageToolCall{ID: fmt.Sprintf("c%d", i), Name: fmt.Sprintf("tool%d", i)}
	}
	tui.AppendToolStart(calls)

	// The sixth call lands first, so the rollup is planted near the bottom;
	// earlier rows then fold and must pull it up with them.
	order := []int{5, 0, 1, 3, 2, 4, 6}
	for _, i := range order {
		tui.AppendToolEnd(calls[i], "ok", time.Second, nil)
	}

	rows := toolRows(m)
	if len(rows) != toolWindowSize+1 {
		t.Fatalf("settled batch = %d rows, want %d", len(rows), toolWindowSize+1)
	}
	if !strings.Contains(rows[0], "7 tool calls") {
		t.Fatalf("rollup should lead the activity area, got rows %v", rows)
	}
	for _, row := range rows[1:] {
		if strings.Contains(row, "tool calls") {
			t.Fatalf("only one rollup may exist: %v", rows)
		}
	}
	// It must sit above the surviving rows in the transcript too, not merely
	// first among tool rows.
	for _, idx := range m.toolWindow {
		if idx <= m.toolRollupIndex {
			t.Fatalf("row %d is at or above the rollup at %d", idx, m.toolRollupIndex)
		}
	}
}

// A slow call at the head of the batch must not hold its finished siblings on
// screen — folding skips it and takes the oldest settled row instead, so the
// window still reaches the cap while it runs.
func TestSlowLeadCallDoesNotBlockFolding(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.busy = true
	tui := &gotuiTurnUI{repl: r, config: r.config}

	calls := make([]messages.ChatMessageToolCall, 7)
	for i := range calls {
		calls[i] = messages.ChatMessageToolCall{ID: fmt.Sprintf("c%d", i), Name: "bash"}
	}
	tui.AppendToolStart(calls)

	// Everything except the first call finishes.
	for _, c := range calls[1:] {
		tui.AppendToolEnd(c, "ok", time.Second, nil)
	}
	// The area is already down to the cap even though the lead call is still
	// running: it holds one of the three slots rather than blocking the fold.
	if got := len(toolRows(m)); got != toolWindowSize+1 {
		t.Fatalf("with one call still running: %d rows, want %d", got, toolWindowSize+1)
	}
	if !m.toolRowRunning(m.toolWindow[0]) {
		t.Fatalf("the running lead call must keep its row: window %v", m.toolWindow)
	}

	// It is inside the cap by the time it lands, so nothing folds and its own
	// outcome is shown — a slow call is never dropped for finishing late.
	tui.AppendToolEnd(calls[0], "ok", 9*time.Second, nil)
	if got := len(toolRows(m)); got != toolWindowSize+1 {
		t.Fatalf("settled batch = %d rows, want %d", got, toolWindowSize+1)
	}
	if !strings.Contains(strings.Join(m.transcript, "\n"), "9.0s") {
		t.Fatalf("the slow lead call should show its outcome: %v", m.transcript)
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
