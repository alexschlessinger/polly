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

// An abridged line's calls are one run: it shows the run's newest
// activityRunRows calls, the first marked for the rest; the expanded state
// shows every row, and folding forgets it.
func TestAbridgedLineCutsTheCallRun(t *testing.T) {
	const calls = 8
	_, m := windowFixture(t, calls)
	block := activityBlocks(m, 80)[0]
	m.toggleDisclosureGroup(activityTools, block.toolDisclosureIDs, 80)
	rows := windowRows(m)
	if len(rows) != activityRunRows || strings.Contains(strings.Join(rows, "\n"), "earlier") {
		t.Fatalf("%d calls want the newest %d rows: %q", calls, activityRunRows, rows)
	}
	for i, row := range rows {
		if want := fmt.Sprintf("file-%02d.go", i+calls-activityRunRows); !strings.Contains(row, want) || strings.HasPrefix(row, reasoningCutIndent) != (i == 0) {
			t.Fatalf("row %d wants %s, the first alone marked: %q", i, want, rows)
		}
	}
	expandLines(m, nil)
	if rows = windowRows(m); len(rows) != calls || !strings.Contains(rows[0], "file-00.go") || strings.HasPrefix(rows[0], reasoningCutIndent) {
		t.Fatalf("expanded wants every row: %q", rows)
	}
	// The depth is the view's: folding and reopening the line keeps it.
	m.toggleDisclosureGroup(activityTools, block.toolDisclosureIDs, 80)
	m.toggleDisclosureGroup(activityTools, block.toolDisclosureIDs, 80)
	if rows := windowRows(m); len(rows) != calls {
		t.Fatalf("reopened line wants to stay expanded: %q", rows)
	}
}

func TestShortRunHasNoCut(t *testing.T) {
	_, m := windowFixture(t, activityRunRows)
	block := activityBlocks(m, 80)[0]
	m.toggleDisclosureGroup(activityTools, block.toolDisclosureIDs, 80)
	if rows := windowRows(m); len(rows) != activityRunRows || strings.Contains(rows[0], "earlier") || strings.HasPrefix(rows[0], reasoningCutIndent) {
		t.Fatalf("a run within its rows wants every row and no mark: %q", rows)
	}
}

func TestFullDepthShowsEveryRow(t *testing.T) {
	_, m := windowFixture(t, 9)
	m.toggleAllDisclosures(80) // out of the fixture's collapsed depth: abridged, the line open
	if rows := windowRows(m); m.depth != depthAbridged || len(rows) != activityRunRows {
		t.Fatalf("abridged depth wants the cut run: %q", rows)
	}
	m.toggleAllDisclosures(80) // full
	if rows := windowRows(m); m.depth != depthFull || len(rows) != 9 || strings.Contains(rows[0], "earlier") || strings.HasPrefix(rows[0], reasoningCutIndent) {
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
	m.toggleAllDisclosures(80) // calls alone: the cut run again
	if rows := windowRows(m); m.depth != depthCalls || len(rows) != activityRunRows {
		t.Fatalf("calls depth wants the cut run: %q", rows)
	}
	m.toggleAllDisclosures(80) // collapsed: the line folds
	if m.depth != depthCollapsed || activityBlocks(m, 80)[0].activityToolDetail != "" {
		t.Fatalf("collapsed depth wants the line folded: %q", windowRows(m))
	}
	m.toggleAllDisclosures(80) // abridged: the line opens again
	if rows := windowRows(m); m.depth != depthAbridged || len(rows) != activityRunRows {
		t.Fatalf("depth after four presses = %v, want abridged with the line open: %q", m.depth, rows)
	}
}

// Rows a click opened under a call belong to its entry and take none of
// the area's rows: a log of exactly one entry more than the area shows its
// newest activityTailRows entries, each call whole with its open output,
// and the call that leaves the top takes its output along.
func TestTailKeepsOpenRowsWhole(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("inspect")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	// Runs of two calls, a thought row and four calls: seven entries and
	// two blank rows between the runs, one row more than the area holds.
	var names []string
	for step, size := range []struct{ thoughts, calls int }{{0, 2}, {1, 0}, {0, 4}} {
		tui.ModelAttemptStarted(step, 0)
		for i := range size.thoughts {
			tui.ShowThinking(fmt.Sprintf("Thought %d\n", i))
		}
		for i := range size.calls {
			c := readCall(fmt.Sprintf("s%d-%d", step, i))
			names = append(names, c.ID+".go")
			tui.AppendToolStart([]messages.ChatMessageToolCall{c})
			tui.AppendToolEnd(c, "ok", time.Millisecond, nil)
		}
	}
	tui.AppendAssistantText("Done.")
	r.endTurn(nil)
	openAllOutputs(m)
	rows := windowRows(m)
	var kept []string
	for i, row := range rows {
		if !strings.Contains(row, "✓ read ") {
			continue
		}
		if i+2 >= len(rows) || strings.TrimSpace(rows[i+1]) != "output" || strings.TrimSpace(rows[i+2]) != "ok" {
			t.Fatalf("call %q wants its open output under it: %q", row, rows)
		}
		kept = append(kept, row)
	}
	if len(kept) != len(names)-1 || strings.Contains(strings.Join(rows, "\n"), names[0]) {
		t.Fatalf("the oldest call wants to leave with its output, %d calls kept: %q", len(kept), rows)
	}
	if !strings.HasPrefix(rows[0], reasoningCutIndent) || !strings.Contains(rows[0], names[1]) {
		t.Fatalf("the top call wants the mark for its hidden run mate: %q", rows)
	}
	if thoughts := strings.Count(strings.Join(rows, "\n"), "Thought "); thoughts != 1 {
		t.Fatalf("the thought run wants its row: %q", rows)
	}
	// Each open call's output and each change of kind leave one blank row,
	// never two, and none at either end.
	if strings.TrimSpace(rows[0]) == "" || strings.TrimSpace(rows[len(rows)-1]) == "" {
		t.Fatalf("the window wants no blank row at either end: %q", rows)
	}
	for i := 1; i < len(rows); i++ {
		if strings.TrimSpace(rows[i]) == "" && strings.TrimSpace(rows[i-1]) == "" {
			t.Fatalf("rows %d and %d are both blank: %q", i-1, i, rows)
		}
	}
	for i, row := range rows {
		if strings.Contains(row, "Thought ") && (i == 0 || strings.TrimSpace(rows[i-1]) != "" || i+1 >= len(rows) || strings.TrimSpace(rows[i+1]) != "") {
			t.Fatalf("the thought wants a blank row on either side: %q", rows)
		}
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

// An abridged thought longer than its run's rows shows its newest
// activityRunRows rows, the top one marked as cut; the expanded state
// shows every retained row.
func TestAbridgedThoughtShowsNewestRows(t *testing.T) {
	const sentences = activityRunRows + 5
	_, m := thoughtFixture(t, sentences)
	block := activityBlocks(m, 80)[0]
	m.toggleDisclosureGroup(activityThought, block.reasoningIDs, 80)
	rows := windowRows(m)
	hidden := sentences - activityRunRows
	if len(rows) != activityRunRows || !strings.HasPrefix(rows[0], reasoningCutIndent+fmt.Sprintf("Sentence %02d", hidden)) || strings.Contains(strings.Join(rows, "\n"), "earlier") {
		t.Fatalf("%d rows want the newest %d, the top marked as cut: %q", sentences, activityRunRows, rows)
	}
	for _, row := range rows[1:] {
		if strings.HasPrefix(row, reasoningCutIndent) {
			t.Fatalf("only the top row carries the cut mark: %q", rows)
		}
	}
	expandLines(m, block.reasoningIDs)
	if rows = windowRows(m); len(rows) != sentences || !strings.Contains(rows[0], "Sentence 00") {
		t.Fatalf("expanded wants every retained row: %q", rows)
	}
	// A whole part stays whole across a fold and reopen.
	m.toggleDisclosureGroup(activityThought, block.reasoningIDs, 80)
	m.toggleDisclosureGroup(activityThought, block.reasoningIDs, 80)
	if rows := windowRows(m); len(rows) != sentences {
		t.Fatalf("reopened thought wants to stay whole: %q", rows)
	}
	// A thought whose tail lost its start says so when whole.
	m.reasoningRecords.get(block.reasoningIDs[0]).tailCut = true
	m.visual.invalidate()
	if rows := windowRows(m); len(rows) != sentences+1 || strings.TrimSpace(rows[0]) != "… earlier text not kept" {
		t.Fatalf("expanded level wants every row behind the cut notice: %q", rows)
	}
}

func TestShortThoughtHasNoCut(t *testing.T) {
	_, m := thoughtFixture(t, activityRunRows)
	block := activityBlocks(m, 80)[0]
	m.toggleDisclosureGroup(activityThought, block.reasoningIDs, 80)
	if rows := windowRows(m); len(rows) != activityRunRows || strings.Contains(rows[0], "earlier") || strings.HasPrefix(rows[0], reasoningCutIndent) {
		t.Fatalf("a thought within its run's rows wants every row and no mark: %q", rows)
	}
}
