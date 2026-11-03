package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	rw "github.com/mattn/go-runewidth"
)

// windowFixture runs one turn of n settled read calls, named file-00.go
// upward, and returns the REPL with the turn ended.
func windowFixture(t *testing.T, n int) (*managedREPL, *replModel) {
	t.Helper()
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
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

func earlierLink(m *replModel) (toolOutputLink, bool) {
	rows := m.transcriptRows(80)
	for _, link := range m.visibleToolOutputLinks(fullViewport(len(rows), 80), 0) {
		if link.action == earlierRowsAction {
			return link, true
		}
	}
	return toolOutputLink{}, false
}

func TestOpenLineWindowsNewestRows(t *testing.T) {
	r, m := windowFixture(t, 8)
	block := activityBlocks(m, 80)[0]
	m.toggleDisclosureGroup(activityTools, block.toolDisclosureIDs, 80)
	rows := windowRows(m)
	if len(rows) != activityRowWindow+1 || strings.TrimSpace(rows[0]) != "⋯ 3 earlier calls" {
		t.Fatalf("eight calls want a link and the newest %d rows: %q", activityRowWindow, rows)
	}
	for i, row := range rows[1:] {
		if want := fmt.Sprintf("file-%02d.go", i+3); !strings.Contains(row, want) {
			t.Fatalf("row %d wants %s: %q", i, want, rows)
		}
	}
	link, ok := earlierLink(m)
	if !ok || link.X != activityRailCols || link.Cols != rw.StringWidth("⋯ 3 earlier calls") {
		t.Fatalf("link wants to sit on the label: %+v %v", link, ok)
	}
	if !r.activateToolOutput(m, link, nil) {
		t.Fatal("link did not activate")
	}
	rows = windowRows(m)
	if len(rows) != 8 || !strings.Contains(rows[0], "file-00.go") {
		t.Fatalf("the reveal wants every row: %q", rows)
	}
	if _, ok := earlierLink(m); ok {
		t.Fatal("revealed line kept its link")
	}
	// Folding forgets the reveal; the window is back on the next open.
	m.toggleDisclosureGroup(activityTools, block.toolDisclosureIDs, 80)
	m.toggleDisclosureGroup(activityTools, block.toolDisclosureIDs, 80)
	if rows := windowRows(m); len(rows) != activityRowWindow+1 {
		t.Fatalf("reopened line wants its window back: %q", rows)
	}
}

func TestShortLineHasNoWindow(t *testing.T) {
	_, m := windowFixture(t, activityRowWindow)
	block := activityBlocks(m, 80)[0]
	m.toggleDisclosureGroup(activityTools, block.toolDisclosureIDs, 80)
	if rows := windowRows(m); len(rows) != activityRowWindow || strings.Contains(rows[0], "earlier") {
		t.Fatalf("a line within the window wants no link: %q", rows)
	}
}

func TestDetailedDisclosuresShowEveryRow(t *testing.T) {
	_, m := windowFixture(t, 9)
	m.toggleAllDisclosures(80) // opened
	if rows := windowRows(m); len(rows) != activityRowWindow+1 {
		t.Fatalf("opened level wants the window: %q", rows)
	}
	m.toggleAllDisclosures(80) // detailed
	if rows := windowRows(m); len(rows) < 9 || strings.Contains(rows[0], "earlier") {
		t.Fatalf("detailed level wants every row: %q", rows)
	}
	// Blocks that arrive at the detailed level inherit the reveal.
	m.beginTurn("more")
	m.appendToolCallStart(messages.ChatMessageToolCall{ID: "late", Name: "read_file"})
	if record := m.toolDisclosures.get(m.turnToolDisclosureID); record == nil || !record.allRows {
		t.Fatal("a new record at the detailed level wants every row")
	}
	m.toggleAllDisclosures(80) // folded
	for _, record := range m.toolDisclosures.all() {
		if record.allRows || record.expanded {
			t.Fatal("folding wants the reveal cleared")
		}
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
	if strings.TrimSpace(rows[0]) != "⋯ 2 earlier calls" {
		t.Fatalf("seven open rows want two hidden: %q", rows)
	}
	visible := 0
	for _, row := range rows[1:] {
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
	m.beginTurn("think")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	for i := range sentences {
		tui.ShowThinking(fmt.Sprintf("Sentence %02d of the reasoning, one row long.\n", i))
	}
	tui.AppendAssistantText("Done.")
	r.endTurn(nil)
	return r, m
}

func thoughtLink(m *replModel) (toolOutputLink, bool) {
	rows := m.transcriptRows(80)
	for _, link := range m.visibleToolOutputLinks(fullViewport(len(rows), 80), 0) {
		if link.action == earlierLinesAction {
			return link, true
		}
	}
	return toolOutputLink{}, false
}

func TestOpenThoughtWindowsNewestLines(t *testing.T) {
	r, m := thoughtFixture(t, 9)
	block := activityBlocks(m, 80)[0]
	m.toggleDisclosureGroup(activityThought, block.reasoningIDs, 80)
	rows := windowRows(m)
	if len(rows) != reasoningPreviewLines+1 || strings.TrimSpace(rows[0]) != "⋯ 4 earlier lines" || !strings.Contains(rows[1], "Sentence 04") {
		t.Fatalf("nine rows want a link and the newest %d: %q", reasoningPreviewLines, rows)
	}
	lineRow := -1
	for y, row := range transcriptRowsText(m.transcriptRows(80)) {
		if strings.Contains(row, "▾ thought") {
			lineRow = y
		}
	}
	link, ok := thoughtLink(m)
	if !ok || link.Y != lineRow+1 || link.X != activityRailCols || link.Cols != rw.StringWidth("⋯ 4 earlier lines") {
		t.Fatalf("link wants to sit on the label under the line (row %d): %+v %v", lineRow, link, ok)
	}
	if !r.activateToolOutput(m, link, nil) {
		t.Fatal("link did not activate")
	}
	rows = windowRows(m)
	if len(rows) != 9 || !strings.Contains(rows[0], "Sentence 00") {
		t.Fatalf("the reveal wants every retained row: %q", rows)
	}
	if _, ok := thoughtLink(m); ok {
		t.Fatal("revealed thought kept its link")
	}
	// Folding forgets the reveal.
	m.toggleDisclosureGroup(activityThought, block.reasoningIDs, 80)
	m.toggleDisclosureGroup(activityThought, block.reasoningIDs, 80)
	if rows := windowRows(m); len(rows) != reasoningPreviewLines+1 {
		t.Fatalf("reopened thought wants its window back: %q", rows)
	}
	// The detailed level reveals; a thought whose tail lost its start says so.
	m.toggleAllDisclosures(80)
	m.toggleAllDisclosures(80)
	m.reasoningRecords.get(block.reasoningIDs[0]).tailCut = true
	m.visual.invalidate()
	if rows := windowRows(m); len(rows) != 10 || strings.TrimSpace(rows[0]) != "… earlier text not kept" {
		t.Fatalf("detailed level wants every row behind the cut notice: %q", rows)
	}
}

func TestShortThoughtHasNoWindow(t *testing.T) {
	_, m := thoughtFixture(t, reasoningPreviewLines)
	block := activityBlocks(m, 80)[0]
	m.toggleDisclosureGroup(activityThought, block.reasoningIDs, 80)
	if rows := windowRows(m); len(rows) != reasoningPreviewLines || strings.Contains(rows[0], "earlier") {
		t.Fatalf("a thought within the window wants no link: %q", rows)
	}
}
