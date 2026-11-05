package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
)

// windowFixture runs one turn of n settled read calls, named file-00.go
// upward, and returns the REPL with the turn ended.
func windowFixture(t *testing.T, n int) (*managedREPL, *replModel) {
	t.Helper()
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.depth = depthCollapsed // the tests open the folded line by hand
	m.beginTurn("inspect")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	var calls []messages.ChatMessageToolCall
	for i := range n {
		calls = append(calls, messages.ChatMessageToolCall{ID: fmt.Sprint(i), Name: "read_file", Arguments: fmt.Sprintf(`{"path":"file-%02d.go"}`, i)})
	}
	tui.AppendToolStart(calls)
	for _, call := range calls {
		tui.AppendToolEnd(call, "ok", time.Millisecond, nil)
	}
	tui.AppendAssistantText("Done.")
	r.endTurn(nil)
	return r, m
}

// windowRows returns the rows under the first activity line.
func windowRows(m *replModel) []string {
	lines := strings.Split(plainStyledText(activityBlocks(m, 80)[0].text), "\n")
	return lines[1:]
}

// expandLines is the expanded depth's effect on the rows and the given
// thoughts, without the outputs the depth also opens: every call row, every
// thought row.
func expandLines(m *replModel, reasoningIDs []int64) {
	m.depth = depthFull
	for _, id := range reasoningIDs {
		record := m.reasoningRecords.get(id)
		record.ensureSegments()
		for i := range record.segments {
			record.segments[i].full = true
		}
	}
	m.visual.invalidate()
}

// openAllOutputs opens every ordinary call's output by hand, as clicks
// would.
func openAllOutputs(m *replModel) {
	for _, record := range m.toolDisclosures.all() {
		for i := range record.rows {
			if !record.rows[i].isAgent() {
				record.rows[i].outputExpanded = true
			}
		}
		m.refreshToolDisclosure(record)
	}
	m.visual.invalidate()
}

// openAllLines opens every activity line by hand.
func openAllLines(m *replModel) {
	for id, record := range m.toolDisclosures.all() {
		if !record.expanded {
			m.toggleToolDisclosure(id)
		}
	}
	for id, record := range m.reasoningRecords.all() {
		if !record.expanded {
			m.toggleReasoning(id, 0)
		}
	}
	m.visual.invalidate()
}

// foldEveryLine folds every activity line by hand.
func foldEveryLine(m *replModel) {
	for id, record := range m.toolDisclosures.all() {
		if record.expanded {
			m.toggleToolDisclosure(id)
		}
	}
	for id, record := range m.reasoningRecords.all() {
		if record.expanded {
			m.toggleReasoning(id, 0)
		}
	}
	m.visual.invalidate()
}

// An abridged line shows the newest rows with no mark for the rest; the
// expanded state shows every row, and folding forgets it.
func TestAbridgedLineWindowsNewestRows(t *testing.T) {
	_, m := windowFixture(t, 8)
	block := activityBlocks(m, 80)[0]
	m.toggleDisclosureGroup(activityTools, block.toolDisclosureIDs, 80)
	rows := windowRows(m)
	if len(rows) != activityRowWindow || strings.Contains(strings.Join(rows, "\n"), "earlier") {
		t.Fatalf("eight calls want the newest %d rows and no mark: %q", activityRowWindow, rows)
	}
	for i, row := range rows {
		if want := fmt.Sprintf("file-%02d.go", i+8-activityRowWindow); !strings.Contains(row, want) {
			t.Fatalf("row %d wants %s: %q", i, want, rows)
		}
	}
	expandLines(m, nil)
	if rows = windowRows(m); len(rows) != 8 || !strings.Contains(rows[0], "file-00.go") {
		t.Fatalf("expanded wants every row: %q", rows)
	}
	// The depth is the view's: folding and reopening the line keeps it.
	m.toggleDisclosureGroup(activityTools, block.toolDisclosureIDs, 80)
	m.toggleDisclosureGroup(activityTools, block.toolDisclosureIDs, 80)
	if rows := windowRows(m); len(rows) != 8 {
		t.Fatalf("reopened line wants to stay expanded: %q", rows)
	}
}

func TestShortLineHasNoWindow(t *testing.T) {
	_, m := windowFixture(t, activityRowWindow)
	block := activityBlocks(m, 80)[0]
	m.toggleDisclosureGroup(activityTools, block.toolDisclosureIDs, 80)
	if rows := windowRows(m); len(rows) != activityRowWindow || strings.Contains(rows[0], "earlier") {
		t.Fatalf("a line within the window wants every row and no mark: %q", rows)
	}
}

func TestFullDepthShowsEveryRow(t *testing.T) {
	_, m := windowFixture(t, 9)
	m.toggleAllDisclosures(80) // out of the fixture's collapsed depth: abridged, the line open
	if rows := windowRows(m); m.depth != depthAbridged || len(rows) != activityRowWindow {
		t.Fatalf("abridged depth wants the window: %q", rows)
	}
	m.toggleAllDisclosures(80) // full
	if rows := windowRows(m); m.depth != depthFull || len(rows) != 9 || strings.Contains(rows[0], "earlier") {
		t.Fatalf("full depth wants every row: %q", rows)
	}
	// No depth opens an output: that is a click on the row.
	for _, record := range m.toolDisclosures.all() {
		for _, row := range record.rows {
			if row.outputExpanded {
				t.Fatal("the full depth opened an output")
			}
		}
	}
	m.toggleAllDisclosures(80) // calls alone: the window again
	if rows := windowRows(m); m.depth != depthCalls || len(rows) != activityRowWindow {
		t.Fatalf("calls depth wants the window: %q", rows)
	}
	m.toggleAllDisclosures(80) // collapsed: the line folds
	if m.depth != depthCollapsed || activityBlocks(m, 80)[0].activityToolDetail != "" {
		t.Fatalf("collapsed depth wants the line folded: %q", windowRows(m))
	}
	m.toggleAllDisclosures(80) // abridged: the line opens again
	if rows := windowRows(m); m.depth != depthAbridged || len(rows) != activityRowWindow {
		t.Fatalf("depth after four presses = %v, want abridged with the line open: %q", m.depth, rows)
	}
}

func TestWindowKeepsOpenRowsWhole(t *testing.T) {
	_, m := windowFixture(t, 7)
	block := activityBlocks(m, 80)[0]
	record := m.toolDisclosures.get(block.toolDisclosureIDs[0])
	for i := range record.rows {
		record.rows[i].outputExpanded = true
	}
	m.toggleDisclosureGroup(activityTools, block.toolDisclosureIDs, 80)
	rows := windowRows(m)
	visible := 0
	for _, row := range rows {
		if strings.Contains(row, "✓ read file-") {
			visible++
		}
	}
	if visible != activityRowWindow || strings.Contains(strings.Join(rows, "\n"), "file-01.go") {
		t.Fatalf("hidden rows take their detail with them: %q", rows)
	}
}

// thoughtFixture runs one turn whose thought wraps to many rows.
func thoughtFixture(t *testing.T, sentences int) (*managedREPL, *replModel) {
	t.Helper()
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.depth = depthCollapsed // the checks below open a folded line by hand
	m.beginTurn("think")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	for i := range sentences {
		tui.ShowThinking(fmt.Sprintf("Sentence %02d of the reasoning, one row long.\n", i))
	}
	tui.AppendAssistantText("Done.")
	r.endTurn(nil)
	return r, m
}

func TestAbridgedThoughtShowsNewestLines(t *testing.T) {
	_, m := thoughtFixture(t, 9)
	block := activityBlocks(m, 80)[0]
	m.toggleDisclosureGroup(activityThought, block.reasoningIDs, 80)
	rows := windowRows(m)
	hidden := 9 - reasoningPreviewLines
	if len(rows) != reasoningPreviewLines || !strings.Contains(rows[0], fmt.Sprintf("Sentence %02d", hidden)) || strings.Contains(strings.Join(rows, "\n"), "earlier") {
		t.Fatalf("nine rows want the newest %d and no mark: %q", reasoningPreviewLines, rows)
	}
	expandLines(m, block.reasoningIDs)
	if rows = windowRows(m); len(rows) != 9 || !strings.Contains(rows[0], "Sentence 00") {
		t.Fatalf("expanded wants every retained row: %q", rows)
	}
	// A whole part stays whole across a fold and reopen.
	m.toggleDisclosureGroup(activityThought, block.reasoningIDs, 80)
	m.toggleDisclosureGroup(activityThought, block.reasoningIDs, 80)
	if rows := windowRows(m); len(rows) != 9 {
		t.Fatalf("reopened thought wants to stay whole: %q", rows)
	}
	// A thought whose tail lost its start says so when whole.
	m.reasoningRecords.get(block.reasoningIDs[0]).tailCut = true
	m.visual.invalidate()
	if rows := windowRows(m); len(rows) != 10 || strings.TrimSpace(rows[0]) != "… earlier text not kept" {
		t.Fatalf("expanded level wants every row behind the cut notice: %q", rows)
	}
}

func TestShortThoughtHasNoWindow(t *testing.T) {
	_, m := thoughtFixture(t, reasoningPreviewLines)
	block := activityBlocks(m, 80)[0]
	m.toggleDisclosureGroup(activityThought, block.reasoningIDs, 80)
	if rows := windowRows(m); len(rows) != reasoningPreviewLines || strings.Contains(rows[0], "earlier") {
		t.Fatalf("a thought within the window wants every row and no mark: %q", rows)
	}
}
