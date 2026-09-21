package main

import (
	"image"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
	ui "github.com/metaspartan/gotui/v5"
)

func TestSessionChangeStatsTotalsTools(t *testing.T) {
	tools := []inspectedTool{
		{pres: toolPresentation{changes: &fileChanges{tracked: true, changes: []fileChange{
			{path: "main.go", additions: 2, deletions: 1},
			{path: "notes.txt", additions: 3},
		}}}},
		// A second call to one of the same files adds to the totals but not
		// to the distinct file count.
		{pres: toolPresentation{changes: &fileChanges{tracked: true, changes: []fileChange{{path: "main.go", additions: 1, deletions: 4}}}}},
		{pres: toolPresentation{}},
		{pres: toolPresentation{changes: &fileChanges{changes: []fileChange{{path: "skipped.go", additions: 9}}}}},
	}
	additions, deletions, files := sessionChangeStats(tools)
	if additions != 6 || deletions != 5 || files != 2 {
		t.Fatalf("totals: +%d −%d, %d files", additions, deletions, files)
	}
	if raw, styled := changeTotalsText(additions, deletions); raw != "+6 −5" || !strings.Contains(styled, "+6") || !strings.Contains(styled, "−5") {
		t.Fatalf("counts: %q / %q", raw, styled)
	}
	if raw, _ := changeTotalsText(0, 0); raw != "" {
		t.Fatalf("no changes should render nothing: %q", raw)
	}
}

func TestSessionChangesFoldRepeatedPaths(t *testing.T) {
	diff := "--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-old\n+new\n"
	tools := []inspectedTool{
		{pres: toolPresentation{changes: &fileChanges{tracked: true, changes: []fileChange{
			{path: "a.go", kind: "created", additions: 2, diff: diff},
			{path: "b.go", kind: "modified", additions: 5, deletions: 1, diff: diff},
		}}}},
		{pres: toolPresentation{changes: &fileChanges{tracked: true, changes: []fileChange{
			{path: "a.go", kind: "modified", additions: 3, deletions: 1, diff: diff},
			{path: "b.go", kind: "deleted", deletions: 4, binary: true},
		}}}},
		{pres: toolPresentation{changes: &fileChanges{changes: []fileChange{{path: "skipped.go", additions: 9}}}}},
	}
	folded := sessionChanges(tools)
	if len(folded) != 2 || folded[0].path != "a.go" || folded[1].path != "b.go" {
		t.Fatalf("folding: %+v", folded)
	}
	a := folded[0]
	if a.kind != "created" || a.additions != 5 || a.deletions != 1 || len(a.bodyLines()) != 6 {
		t.Fatalf("created then modified: %+v (%d lines)", a, len(a.bodyLines()))
	}
	b := folded[1]
	if b.kind != "deleted" || b.additions != 5 || b.deletions != 5 || !b.binary || len(b.bodyLines()) != 0 {
		t.Fatalf("modified then deleted: %+v (%d lines)", b, len(b.bodyLines()))
	}
	if keys := trackedChangeKeys(tools); len(keys) != 2 || keys[0] != "a.go" || keys[1] != "b.go" {
		t.Fatalf("keys: %q", keys)
	}
	if title := changeItemTitle(a); !strings.Contains(title, "a.go") || !strings.Contains(title, "new") || !strings.Contains(title, "+5") {
		t.Fatalf("title: %q", title)
	}
}

func TestChangesInspectorAggregatesOneFile(t *testing.T) {
	withDisplayTTY(t)
	fixture, screen := affordanceTestREPL(t)
	t.Cleanup(func() { _ = fixture.work.close() })
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root")
	r.setupWidgets()
	r.showTab(0)
	screen.SetSize(140, 40)
	m := r.model
	for _, id := range []string{"e1", "e2"} {
		call := messages.ChatMessageToolCall{ID: id, Name: "edit_file", Arguments: `{"path":"main.go"}`}
		m.inspections.setResult(call, toolDataResult(t, call, "Edited main.go", editChanges("main.go")))
	}
	r.openChangesInspector()
	v := waitInspector(t, r, 140)
	items := v.model.changesInspector.items
	if len(items) != 1 || items[0].key != "main.go" {
		t.Fatalf("two edits should fold into one row: %+v", items)
	}
	text := inspectorText(v)
	if !strings.Contains(text, "1 file") || !strings.Contains(text, "+4 −2") {
		t.Fatalf("folded counts: %s", text)
	}
	r.inspectorAction(changesInspectorBlock(items[0].key, "title"))
	text = inspectorText(waitInspector(t, r, 140))
	if strings.Count(text, "+delta") != 2 || strings.Count(text, "@@ -1,3 +1,4 @@") != 2 {
		t.Fatalf("folded row should carry both diffs:\n%s", text)
	}
	r.render()
	if header := plainStyledText(r.inspectorHeaderW.Text); !strings.Contains(header, "Changes · 1 file") {
		t.Fatalf("inspector header: %q", header)
	}
}

func changesInspectorFixture(t *testing.T) *managedREPL {
	t.Helper()
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root")
	r.setupWidgets()
	r.showTab(0)
	m := r.model
	modify := messages.ChatMessageToolCall{ID: "e1", Name: "edit_file", Arguments: `{"path":"main.go"}`}
	create := messages.ChatMessageToolCall{ID: "w1", Name: "write_file", Arguments: `{"path":"notes.txt"}`}
	m.inspections.setResult(modify, toolDataResult(t, modify, "Edited main.go", editChanges("main.go")))
	created := tools.FileChanges{Root: "/w", Tracked: true, Changes: []tools.FileChange{
		tools.DiffFileChange("notes.txt", "", "first\nsecond\n", false, true),
	}}
	m.inspections.setResult(create, toolDataResult(t, create, "Created notes.txt", created))
	binary := messages.ChatMessageToolCall{ID: "b1", Name: "bash", Arguments: `{"command":"convert logo.png"}`}
	binaryChanges := tools.FileChanges{Root: "/w", Tracked: true, Changes: []tools.FileChange{
		tools.DiffFileChange("logo.png", "\x00\x01", "\x00\x02", true, true),
	}}
	m.inspections.setResult(binary, toolDataResult(t, binary, "", tools.CommandResult{ExitCode: 0, Changes: &binaryChanges}))
	return r
}

func TestStatusRowShowsSessionDiffAndOpensChanges(t *testing.T) {
	withDisplayTTY(t)
	fixture, screen := affordanceTestREPL(t)
	t.Cleanup(func() { _ = fixture.work.close() })
	r := changesInspectorFixture(t)
	screen.SetSize(140, 40)
	m := r.model

	r.render()
	status := plainStyledText(m.statusRow(140))
	if !strings.Contains(status, "+4 −1") {
		t.Fatalf("status row missing the session diff: %q", status)
	}
	if m.status.changesField.Cols == 0 {
		t.Fatalf("status row recorded no diff field: %q", status)
	}

	_, height := screen.Size()
	r.handleEvent(mouseEvent("<MouseLeft>", image.Pt(m.status.changesField.X, height-1)))
	w := r.workspace()
	if !w.inspector.open || w.inspector.target.kind != changesViewKind {
		t.Fatalf("the diff field did not open the Changes inspector: open=%v kind=%d", w.inspector.open, w.inspector.target.kind)
	}
	waitInspector(t, r, 140)
	w.inspector.focused = true
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
	v := waitInspector(t, r, 140)
	text := inspectorText(v)
	for _, want := range []string{"3 files", "+4 −1", "main.go", "-beta", "+delta", "notes.txt", "+first", "logo.png", "binary"} {
		if !strings.Contains(text, want) {
			t.Fatalf("changes inspector missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "--- a/") || strings.Contains(text, "+++ b/") {
		t.Fatalf("file headers should be folded into the diff title:\n%s", text)
	}
	r.render()
	if header := plainStyledText(r.inspectorHeaderW.Text); !strings.Contains(header, "Changes · 3 files") {
		t.Fatalf("inspector header: %q", header)
	}
	for _, width := range []int{48, 140} {
		for _, row := range v.view.Rows(v.model, width) {
			if style.CellsWidth(row) > width {
				t.Fatalf("width %d overflow: %q", width, plainCells(row))
			}
		}
	}
}

func TestStatusRowDropsDiffBeforeContext(t *testing.T) {
	m := newReplModel()
	m.status.contextName = "ctx"
	m.status.contextUsed, m.status.contextLimit = 1234, 156000
	m.inspections.tools = []inspectedTool{{pres: toolPresentation{changes: &fileChanges{tracked: true, changes: []fileChange{{path: "a.go", additions: 5, deletions: 2}}}}}}

	wide := plainStyledText(m.statusRow(30))
	if !strings.Contains(wide, "+5 −2") || !strings.Contains(wide, "1.2k/156k") {
		t.Fatalf("wide row should keep the diff and context: %q", wide)
	}
	narrow := plainStyledText(m.statusRow(24))
	if strings.Contains(narrow, "+5 −2") {
		t.Fatalf("narrow row should drop the diff field first: %q", narrow)
	}
	if !strings.Contains(narrow, "1.2k/156k") || !strings.Contains(narrow, "ctx") {
		t.Fatalf("narrow row dropped too much: %q", narrow)
	}
	if m.status.changesField.Cols != 0 {
		t.Fatalf("dropped diff field kept a click target: %+v", m.status.changesField)
	}
}

func TestChangesInspectorRefreshesOnNewChange(t *testing.T) {
	withDisplayTTY(t)
	fixture, screen := affordanceTestREPL(t)
	t.Cleanup(func() { _ = fixture.work.close() })
	r := changesInspectorFixture(t)
	screen.SetSize(140, 40)
	r.openChangesInspector()
	v := waitInspector(t, r, 140)
	if !strings.Contains(inspectorText(v), "3 files") {
		t.Fatalf("initial list: %q", inspectorText(v))
	}
	m := r.model
	call := messages.ChatMessageToolCall{ID: "e2", Name: "edit_file", Arguments: `{"path":"extra.go"}`}
	m.inspections.setResult(call, toolDataResult(t, call, "Edited extra.go", editChanges("extra.go")))
	v = waitInspector(t, r, 140)
	text := inspectorText(v)
	if !strings.Contains(text, "4 files") || !strings.Contains(text, "extra.go") {
		t.Fatalf("changes inspector did not pick up the new diff: %q", text)
	}
}

func TestInspectCommandChanges(t *testing.T) {
	withDisplayTTY(t)
	fixture, screen := affordanceTestREPL(t)
	t.Cleanup(func() { _ = fixture.work.close() })
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root")
	r.setupWidgets()
	r.showTab(0)
	screen.SetSize(120, 30)
	r.inspectCommand("changes")
	if r.workspace().inspector.open {
		t.Fatal("an empty session opened the Changes inspector")
	}
	if text := plainStyledText(strings.Join(transcriptTexts(r.model), "\n")); !strings.Contains(text, "No file changes to inspect") {
		t.Fatalf("missing empty notice: %q", text)
	}

	call := messages.ChatMessageToolCall{ID: "e1", Name: "edit_file", Arguments: `{"path":"main.go"}`}
	r.model.inspections.setResult(call, toolDataResult(t, call, "Edited main.go", editChanges("main.go")))
	r.inspectCommand("changes")
	w := r.workspace()
	if !w.inspector.open || w.inspector.target.kind != changesViewKind {
		t.Fatalf("/inspect changes did not open the Changes inspector: open=%v kind=%d", w.inspector.open, w.inspector.target.kind)
	}
	v := waitInspector(t, r, 120)
	if text := inspectorText(v); !strings.Contains(text, "1 file") || !strings.Contains(text, "+2 −1") {
		t.Fatalf("changes inspector: %q", text)
	}
}

func TestChangesInspectorCollapsesItems(t *testing.T) {
	withDisplayTTY(t)
	fixture, screen := affordanceTestREPL(t)
	t.Cleanup(func() { _ = fixture.work.close() })
	r := changesInspectorFixture(t)
	screen.SetSize(140, 40)
	r.openChangesInspector()
	v := waitInspector(t, r, 140)
	items := v.model.changesInspector.items
	if len(items) != 3 || items[0].expanded || items[1].expanded {
		t.Fatalf("changes should open folded: %+v", items)
	}
	if text := inspectorText(v); strings.Contains(text, "-beta") || !strings.Contains(text, "main.go") || !strings.Contains(text, "+2 −1") {
		t.Fatalf("folded list: %s", text)
	}

	// A title click opens one diff and leaves the others alone.
	r.inspectorAction(changesInspectorBlock(items[0].key, "title"))
	v = waitInspector(t, r, 140)
	text := inspectorText(v)
	if !strings.Contains(text, "-beta") || strings.Contains(text, "+first") {
		t.Fatalf("opening one diff: %s", text)
	}

	// Ctrl-O opens what is closed, then closes everything.
	r.workspace().inspector.focused = true
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
	if text = inspectorText(waitInspector(t, r, 140)); !strings.Contains(text, "-beta") || !strings.Contains(text, "+first") {
		t.Fatalf("Ctrl-O did not open every diff: %s", text)
	}

	// The open is sticky: a change that arrives later opens too.
	call := messages.ChatMessageToolCall{ID: "e2", Name: "edit_file", Arguments: `{"path":"extra.go"}`}
	r.model.inspections.setResult(call, toolDataResult(t, call, "Edited extra.go", editChanges("extra.go")))
	v = waitInspector(t, r, 140)
	items = v.model.changesInspector.items
	if len(items) != 4 || !items[3].expanded {
		t.Fatalf("a later change ignored the sticky open: %+v", items)
	}

	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"})
	text = inspectorText(waitInspector(t, r, 140))
	if strings.Contains(text, "-beta") || strings.Contains(text, "+first") || !strings.Contains(text, "notes.txt") {
		t.Fatalf("second Ctrl-O did not close every diff: %s", text)
	}
}
