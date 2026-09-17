package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

// toolDataResult builds the tool result message the agent persists for data:
// JSON round-tripped into generic metadata, as llm.Agent stores it.
func toolDataResult(t *testing.T, call messages.ChatMessageToolCall, text string, data any) messages.ChatMessage {
	t.Helper()
	encoded, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	var value any
	if err := json.Unmarshal(encoded, &value); err != nil {
		t.Fatal(err)
	}
	msg := messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: call.ID, ToolName: call.Name, Content: text, Metadata: map[string]any{"tool_data": value}}
	msg.SetToolSucceeded(true)
	return msg
}

func editChanges(path string) tools.FileChanges {
	return tools.FileChanges{Root: "/w", Tracked: true, Changes: []tools.FileChange{
		tools.DiffFileChange(path, []byte("alpha\nbeta\ngamma\n"), []byte("alpha\ndelta\ngamma\nomega\n"), true, true),
	}}
}

func TestFileChangesFromResultDecodesEditAndBashShapes(t *testing.T) {
	call := messages.ChatMessageToolCall{ID: "e", Name: "edit_file"}
	edit := fileChangesFromResult(toolDataResult(t, call, "Edited", editChanges("main.go")))
	if edit == nil || !edit.tracked || edit.root != "/w" || len(edit.changes) != 1 || edit.changes[0].additions != 2 || edit.changes[0].deletions != 1 || edit.changes[0].kind != "modified" || !strings.Contains(edit.changes[0].diff, "+delta") {
		t.Fatalf("edit: %+v", edit)
	}
	bash := messages.ChatMessageToolCall{ID: "b", Name: "bash"}
	cmd := fileChangesFromResult(toolDataResult(t, bash, "", tools.CommandResult{ExitCode: 0, Changes: &tools.FileChanges{Reason: "not a git repository"}}))
	if cmd == nil || cmd.tracked || cmd.reason != "not a git repository" {
		t.Fatalf("bash untracked: %+v", cmd)
	}
	if got := fileChangesFromResult(toolDataResult(t, bash, "", tools.CommandResult{ExitCode: 1})); got != nil {
		t.Fatalf("bash without changes: %+v", got)
	}
	if got := fileChangesFromResult(messages.ChatMessage{Metadata: map[string]any{"tool_data": "text"}}); got != nil {
		t.Fatal("malformed data decoded")
	}
	if got := fileChangesFromResult(messages.ChatMessage{}); got != nil {
		t.Fatal("no metadata decoded")
	}
}

func TestChangeCountText(t *testing.T) {
	cases := []struct {
		changes fileChanges
		want    string
	}{
		{fileChanges{tracked: true, changes: []fileChange{{kind: "modified", additions: 3, deletions: 1}}}, "+3 −1"},
		{fileChanges{tracked: true, changes: []fileChange{{kind: "created", additions: 12}}}, "new +12"},
		{fileChanges{tracked: true, changes: []fileChange{{kind: "deleted", deletions: 4}}}, "deleted −4"},
		{fileChanges{tracked: true, changes: []fileChange{{kind: "modified", binary: true}}}, "binary"},
		{fileChanges{tracked: true, changes: []fileChange{{kind: "created", additions: 1}, {kind: "modified", additions: 2, deletions: 2}}}, "+3 −2"},
		{fileChanges{tracked: true}, ""},
		{fileChanges{reason: "not a git repository", changes: []fileChange{{additions: 1}}}, ""},
	}
	for _, c := range cases {
		if got := c.changes.countText(); got != c.want {
			t.Fatalf("%+v: %q, want %q", c.changes, got, c.want)
		}
	}
	styled := styledChangeCounts("new +3 −1")
	if styled != style.Styled("new", "muted", "")+" "+style.Styled("+3", "ok", "")+" "+style.Styled("−1", "err", "") {
		t.Fatalf("styled: %q", styled)
	}
}

func TestRenderDiffLinesStylesAndCaps(t *testing.T) {
	diff := "--- a/x\n+++ b/x\n@@ -1,3 +1,3 @@\n a\n-b [c]\n+\tb\n\\ No newline at end of file\n"
	lines := renderDiffLines(diff, 0, 0, false)
	want := []string{
		style.Styled("@@ -1,3 +1,3 @@", "muted", ""),
		style.Styled(" a", "code", ""),
		style.Styled("-b [c]", "err", ""),
		style.Styled("+   b", "ok", ""),
		style.Styled("\\ No newline at end of file", "muted", ""),
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("lines:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	if plain := plainStyledText(lines[2]); plain != "-b [c]" {
		t.Fatalf("brackets: %q", plain)
	}
	var body strings.Builder
	body.WriteString("--- a\n+++ b\n@@ -1 +1 @@\n")
	for i := 0; i < 20; i++ {
		body.WriteString("+line\n")
	}
	capped := renderDiffLines(body.String(), 0, 12, true)
	if len(capped) != 14 || plainStyledText(capped[12]) != "… 9 more lines" || plainStyledText(capped[13]) != "… truncated" {
		t.Fatalf("capped: %d %q %q", len(capped), capped[len(capped)-2], capped[len(capped)-1])
	}
	narrow := renderDiffLines("--- a\n+++ b\n+"+strings.Repeat("x", 40)+"\n", 10, 0, false)
	if plain := plainStyledText(narrow[0]); len([]rune(plain)) != 10 || !strings.HasSuffix(plain, "…") {
		t.Fatalf("width: %q", plain)
	}
	if got := renderDiffLines("", 0, 0, false); len(got) != 0 {
		t.Fatalf("empty: %q", got)
	}
}

func TestEditToolRowShowsCountsAndInlineHunk(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("edit")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	call := messages.ChatMessageToolCall{ID: "e1", Name: "edit_file", Arguments: `{"path":"main.go","old_string":"beta","new_string":"delta"}`}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	tui.AppendToolEnd(call, "Edited main.go: 1 replacement(s).", 1200*time.Millisecond, nil)
	tui.AppendToolResult(call, toolDataResult(t, call, "Edited", editChanges("main.go")))
	record, row := m.toolDisclosureRowForCall(call.ID)
	if row.pres.changes == nil || row.inline.counts != "+2 −1" {
		t.Fatalf("row: %+v", row)
	}
	collapsed := plainStyledText(m.transcript[record.transcriptIndex].text)
	if strings.Contains(collapsed, "+delta") || !strings.Contains(collapsed, "1 tool") {
		t.Fatalf("collapsed disclosure: %q", collapsed)
	}
	m.toggleToolDisclosure(record.id)
	expanded := plainStyledText(m.transcript[record.transcriptIndex].text)
	for _, want := range []string{"✓ edit_file main.go +2 −1 · 1 line · 1.2s", "\n    @@ -1,3 +1,4 @@", "\n    -beta", "\n    +delta", "\n    +omega"} {
		if !strings.Contains(expanded, want) {
			t.Fatalf("expanded missing %q:\n%s", want, expanded)
		}
	}
	if strings.Contains(expanded, "--- a/") {
		t.Fatalf("file headers shown:\n%s", expanded)
	}
	rows := strings.Join(transcriptRowsText(m.transcriptRows(60)), "\n")
	if !strings.Contains(rows, "+delta") || !strings.Contains(rows, "✓ edit main.go +2 −1 · 1 line · 1.2s") {
		t.Fatalf("painted rows lost the hunk or counts:\n%s", rows)
	}
	for _, line := range strings.Split(rows, "\n") {
		if len([]rune(line)) > 60 {
			t.Fatalf("row wider than pane: %q", line)
		}
	}
	// The hunk lands behind the rail, not as a loose indented block.
	for _, block := range activityBlocks(m, 60) {
		if strings.Contains(block.text, "+delta") && !strings.Contains(block.text, style.Rail+style.Styled("+delta", "ok", "")) {
			t.Fatalf("hunk not railed:\n%s", block.text)
		}
	}
}

func TestInlineHunkTruncatesAtPaintWidth(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("edit")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	call := messages.ChatMessageToolCall{ID: "e1", Name: "write_file", Arguments: `{"path":"x.txt"}`}
	long := strings.Repeat("y", 120)
	changes := tools.FileChanges{Root: "/w", Tracked: true, Changes: []tools.FileChange{tools.DiffFileChange("x.txt", nil, []byte(long+"\n"), false, true)}}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	tui.AppendToolEnd(call, "Created x.txt", time.Second, nil)
	tui.AppendToolResult(call, toolDataResult(t, call, "Created", changes))
	record, row := m.toolDisclosureRowForCall(call.ID)
	if row.inline.counts != "new +1" {
		t.Fatalf("counts: %q", row.inline.counts)
	}
	m.toggleToolDisclosure(record.id)
	if !strings.Contains(m.transcript[record.transcriptIndex].text, "+"+long) {
		t.Fatal("canonical text should keep the whole line")
	}
	for _, line := range transcriptRowsText(m.transcriptRows(40)) {
		if len([]rune(line)) > 40 {
			t.Fatalf("row wider than pane: %q", line)
		}
		if strings.Contains(line, "+yyyy") && !strings.HasSuffix(strings.TrimRight(line, " "), "…") {
			t.Fatalf("hunk line not truncated: %q", line)
		}
	}
}

func TestBashMultiFileChangesListFilesWithoutHunk(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("run")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	call := messages.ChatMessageToolCall{ID: "b1", Name: "bash", Arguments: `{"command":"sed -i s/a/b/ *.txt"}`}
	changes := &tools.FileChanges{Root: "/w", Tracked: true, Changes: []tools.FileChange{
		tools.DiffFileChange("a.txt", []byte("a\n"), []byte("b\n"), true, true),
		tools.DiffFileChange("dir/c.txt", nil, []byte("new\nfile\n"), false, true),
	}}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	tui.AppendToolEnd(call, "", time.Second, nil)
	tui.AppendToolResult(call, toolDataResult(t, call, "", tools.CommandResult{Changes: changes}))
	record, row := m.toolDisclosureRowForCall(call.ID)
	if row.inline.counts != "+3 −1" {
		t.Fatalf("aggregate counts: %q", row.inline.counts)
	}
	m.toggleToolDisclosure(record.id)
	expanded := plainStyledText(m.transcript[record.transcriptIndex].text)
	for _, want := range []string{"✓ bash sed -i s/a/b/ *.txt +3 −1", "\n    a.txt +1 −1", "\n    dir/c.txt new +2"} {
		if !strings.Contains(expanded, want) {
			t.Fatalf("expanded missing %q:\n%s", want, expanded)
		}
	}
	if rows := strings.Join(transcriptRowsText(m.transcriptRows(80)), "\n"); !strings.Contains(rows, "$ sed -i s/a/b/ *.txt +3 −1") || !strings.Contains(rows, "dir/c.txt new +2") {
		t.Fatalf("painted rows:\n%s", rows)
	}
	if strings.Contains(expanded, "@@") || strings.Contains(expanded, "+new") {
		t.Fatalf("multi-file change showed a hunk:\n%s", expanded)
	}
	if strings.Contains(strings.Join(m.flattenTranscript(), "\n"), "not tracked") {
		t.Fatal("tracked change produced the untracked notice")
	}
}

func TestBashUntrackedChangesNoticeOnce(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("run")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	for _, id := range []string{"b1", "b2"} {
		call := messages.ChatMessageToolCall{ID: id, Name: "bash", Arguments: `{"command":"true"}`}
		tui.AppendToolStart([]messages.ChatMessageToolCall{call})
		tui.AppendToolEnd(call, "", time.Second, nil)
		tui.AppendToolResult(call, toolDataResult(t, call, "", tools.CommandResult{Changes: &tools.FileChanges{Reason: "not a git repository"}}))
	}
	flat := strings.Join(m.flattenTranscript(), "\n")
	if strings.Count(flat, "Command edits are not tracked here: not a git repository") != 1 {
		t.Fatalf("notice count wrong:\n%s", flat)
	}
	_, row := m.toolDisclosureRowForCall("b2")
	if row.pres.changes != nil || row.inline.counts != "" {
		t.Fatalf("untracked row gained counts: %+v", row)
	}
	// Results for unknown calls are ignored rather than synthesizing rows.
	before := len(m.transcript)
	tui.AppendToolResult(messages.ChatMessageToolCall{ID: "ghost", Name: "edit_file"}, toolDataResult(t, messages.ChatMessageToolCall{ID: "ghost", Name: "edit_file"}, "", editChanges("g.go")))
	if len(m.transcript) != before {
		t.Fatal("unknown call result appended a row")
	}
}

func TestFailedToolResultKeepsNoCounts(t *testing.T) {
	withDisplayTTY(t)
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.beginTurn("edit")
	tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
	call := messages.ChatMessageToolCall{ID: "e1", Name: "edit_file", Arguments: `{"path":"main.go"}`}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	tui.AppendToolEnd(call, "old_string not found", time.Second, errors.New("edit failed"))
	tui.AppendToolResult(call, toolDataResult(t, call, "", editChanges("main.go")))
	_, row := m.toolDisclosureRowForCall(call.ID)
	if row.pres.changes != nil || row.changeText != "" {
		t.Fatalf("failed row took changes: %+v", row)
	}
}

func TestHistoryHydratorRestoresChanges(t *testing.T) {
	m := newReplModel()
	h := historyHydrator{m: m}
	call := messages.ChatMessageToolCall{ID: "one", Name: "edit_file", Arguments: `{"path":"main.go"}`}
	h.assistant(messages.ChatMessage{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{call}})
	h.tool(toolDataResult(t, call, "Edited", editChanges("main.go")))
	if h.toolRows[0].pres.changes == nil || h.toolRows[0].inline.counts != "+2 −1" || !strings.Contains(h.toolRows[0].changeText, "+delta") {
		t.Fatalf("hydrated row: %+v", h.toolRows[0])
	}
	h.flushTools()
	record := h.tools
	m.toggleToolDisclosure(record.id)
	expanded := plainStyledText(m.transcript[record.transcriptIndex].text)
	if !strings.Contains(expanded, "edit_file main.go +2 −1") || !strings.Contains(expanded, "\n    +delta") {
		t.Fatalf("hydrated disclosure:\n%s", expanded)
	}
	if len(m.transcript) != 1 {
		t.Fatalf("hydration emitted extra entries: %q", m.flattenTranscript())
	}
}

func TestInspectorShowsDiffFence(t *testing.T) {
	withDisplayTTY(t)
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root")
	m := r.model
	call := messages.ChatMessageToolCall{ID: "one", Name: "edit_file", Arguments: `{"path":"main.go","old_string":"beta","new_string":"delta"}`}
	tui := &gotuiTurnUI{model: m, config: r.config, repl: r}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	tui.AppendToolEnd(call, "Edited main.go: 1 replacement(s).\n2: delta", time.Second, nil)
	tui.AppendToolResult(call, toolDataResult(t, call, "Edited main.go: 1 replacement(s).\n2: delta", editChanges("main.go")))
	r.inspectCommand("")
	waitInspector(t, r, 140)
	v := openToolDetails(t, r, 140)
	text := inspectorText(v)
	for _, want := range []string{"╭─ diff · main.go +2 −1", "│ -beta", "│ +delta", "╭─ output · +2 −1 · 2 lines\n│ Edited main.go: 1 replacement(s)"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q: %s", want, text)
		}
	}
	item := v.model.toolInspector.items[0]
	var joined strings.Builder
	for _, block := range item.output {
		joined.WriteString(block.text + "\n")
	}
	if !strings.Contains(joined.String(), style.Styled("+delta", "ok", "")) || !strings.Contains(joined.String(), style.Styled("-beta", "err", "")) {
		t.Fatalf("diff lines not colored: %s", joined.String())
	}

	bash := messages.ChatMessageToolCall{ID: "two", Name: "bash", Arguments: `{"command":"true"}`}
	untracked := inspectedTool{call: bash, complete: true, available: true, result: toolDataResult(t, bash, "", tools.CommandResult{Changes: &tools.FileChanges{Reason: "not a git repository"}})}
	out := newReplModel()
	if _, err := appendInspectedToolOutput(context.Background(), out, &untracked); err != nil {
		t.Fatal(err)
	}
	if flat := strings.Join(out.flattenTranscript(), "\n"); !strings.Contains(flat, "Command edits not tracked: not a git repository") || !strings.Contains(flat, "No text output") {
		t.Fatalf("untracked inspector output: %s", flat)
	}
}

func TestLineActivityToolRowShowsChangeCounts(t *testing.T) {
	line, _, status := activityTestUI(t, false, &Config{ActivityDetails: true})
	call := messages.ChatMessageToolCall{ID: "e", Name: "edit_file", Arguments: `{"path":"main.go"}`}
	line.AppendToolStart([]messages.ChatMessageToolCall{call})
	line.AppendToolEnd(call, "Edited main.go", time.Second, nil)
	line.AppendToolResult(call, toolDataResult(t, call, "Edited", editChanges("main.go")))
	for _, id := range []string{"b1", "b2"} {
		bash := messages.ChatMessageToolCall{ID: id, Name: "bash", Arguments: `{"command":"true"}`}
		line.AppendToolStart([]messages.ChatMessageToolCall{bash})
		line.AppendToolEnd(bash, "", time.Second, nil)
		line.AppendToolResult(bash, toolDataResult(t, bash, "", tools.CommandResult{Changes: &tools.FileChanges{Reason: "not a git repository"}}))
	}
	line.AppendAssistantText("done")
	line.CompleteTurn(turnCompletion{Elapsed: time.Second})
	line.Stop()
	got := status.String()
	if !strings.Contains(got, "main.go +2 −1") {
		t.Fatalf("counts missing: %s", got)
	}
	if strings.Contains(got, "+delta") || strings.Contains(got, "@@") {
		t.Fatalf("line mode printed a diff body: %s", got)
	}
	if strings.Count(got, "Command edits are not tracked here") != 1 {
		t.Fatalf("notice count: %s", got)
	}
}
