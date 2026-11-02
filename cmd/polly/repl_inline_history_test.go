package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
	ui "github.com/metaspartan/gotui/v5"
)

func inlineTestKey(r *managedREPL, key string) {
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: key})
	r.render()
}

func chooseInlineTestAction(t *testing.T, r *managedREPL, key string) {
	t.Helper()
	for range len(r.inlineKeyboardActions()) + 1 {
		inlineTestKey(r, "<S-Tab>")
		if action, ok := r.selectedInlineAction(); ok && action.key == key {
			return
		}
	}
	t.Fatalf("inline action %q not reachable: %+v", key, r.inlineKeyboardActions())
}

func TestInlineKeyboardReachesWholeToolHistoryAndRecordedDiff(t *testing.T) {
	withDisplayTTY(t)
	for _, replay := range []bool{false, true} {
		t.Run(fmt.Sprint(replay), func(t *testing.T) {
			r, screen := affordanceTestREPL(t)
			t.Cleanup(func() { _ = r.work.close() })
			screen.SetSize(90, 45)
			m := r.model
			m.ed.setText("keep draft")
			var calls []messages.ChatMessageToolCall
			var results []messages.ChatMessage
			var diff strings.Builder
			diff.WriteString("--- a/main.go\n+++ b/main.go\n@@ -1 +1,25 @@\n-old\n")
			for i := range 25 {
				fmt.Fprintf(&diff, "+recorded-%02d\n", i)
			}
			long := strings.Repeat("界", 150) + "end-of-long-line"
			diff.WriteString("+" + long + "\n")
			for i := range 7 {
				call := messages.ChatMessageToolCall{ID: fmt.Sprint(i), Name: "lookup", Arguments: fmt.Sprintf(`{"item":%d}`, i)}
				result := messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: call.ID, Content: fmt.Sprintf("output-%d", i)}
				if i == 0 {
					call.Name, call.Arguments = "edit_file", `{"path":"main.go"}`
					result = toolDataResult(t, call, "edited", tools.FileChanges{Tracked: true, Changes: []tools.FileChange{{Path: "main.go", Diff: diff.String(), Additions: 26, Deletions: 1}}})
				}
				calls, results = append(calls, call), append(results, result)
			}
			if replay {
				history := []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "inspect"}, {Role: messages.MessageRoleAssistant, ToolCalls: calls}}
				m.hydrateHistory(append(history, results...), "ctx")
			} else {
				m.beginTurn("inspect")
				tui := &gotuiTurnUI{repl: r, model: m, config: r.config, turnID: m.turnID}
				tui.AppendToolStart(calls)
				for i, call := range calls {
					tui.AppendToolEnd(call, results[i].Content, time.Second, nil)
					tui.AppendToolResult(call, results[i])
				}
			}
			r.render()
			inlineTestKey(r, "<C-o>")
			links := append([]toolOutputLink(nil), m.toolOutputLinks...)
			if len(links) != len(calls) {
				t.Fatalf("%d recorded calls only produced %d links", len(calls), len(links))
			}
			// The formerly hidden first row opens by keyboard and remains
			// selectable by stable identity after a width change.
			chooseInlineTestAction(t, r, links[0].keyboardKey())
			screen.SetSize(48, 45)
			r.render()
			if action, ok := r.selectedInlineAction(); !ok || action.key != links[0].keyboardKey() {
				t.Fatal("resize lost keyboard identity")
			}
			inlineTestKey(r, "<Enter>")
			painted := strings.Join(transcriptRowsText(m.transcriptRows(48)), "\n")
			for i := range 25 {
				if !strings.Contains(painted, fmt.Sprintf("+recorded-%02d", i)) {
					t.Fatalf("recorded diff line %d disappeared: %s", i, painted)
				}
			}
			if strings.Count(painted, "界") != 150 || !strings.Contains(painted, "end-of-long-line") || !strings.Contains(painted, "-old") {
				t.Fatalf("long-line or deletion content lost: %s", painted)
			}
			colored := 0
			for _, cells := range m.transcriptRows(48) {
				for _, cell := range cells {
					if cell.Rune == '界' {
						colored++
						if cell.Style.Fg != ui.StyleParserColorMap["syn-add"] {
							t.Fatal("wrapped addition lost its diff color")
						}
					}
				}
			}
			if colored != 150 {
				t.Fatal("wrapped diff color check missed content")
			}
			if m.ed.text() != "keep draft" || r.workspace().inspector.open {
				t.Fatal("inline action submitted draft or opened a panel")
			}
			// A local enclosure toggle retains the manual row choice; global
			// Ctrl-O instead applies the next mode and resets row choices.
			record, row := m.toolOutputRow(links[0])
			if record == nil || row == nil || !row.outputExpanded {
				t.Fatal("fixture did not retain the manually expanded diff")
			}
			if !m.toggleToolDisclosure(record.id) || record.expanded || !row.outputExpanded || m.disclosures != disclosuresOpen {
				t.Fatal("local enclosure close changed the row choice or global mode")
			}
			r.render()
			if strings.Contains(strings.Join(transcriptRowsText(m.transcriptRows(48)), "\n"), "+recorded-") {
				t.Fatal("collapsed enclosure leaked diff")
			}
			if !m.toggleToolDisclosure(record.id) || !record.expanded || !row.outputExpanded || len(record.rows) != len(calls) || m.disclosures != disclosuresOpen {
				t.Fatal("local enclosure reopen lost calls, row choice, or global mode")
			}
			r.render()
			if !strings.Contains(strings.Join(transcriptRowsText(m.transcriptRows(48)), "\n"), "+recorded-24") {
				t.Fatal("reopening lost full diff")
			}
		})
	}
}

func TestInlineOutputPageRefreshKeepsHeldViewportAndRejectsOldPage(t *testing.T) {
	m := newReplModel()
	source := newInlineToolOutput(messages.ChatMessageToolCall{}, messages.ChatMessage{Content: strings.Repeat("body\n", 510)})
	record := m.appendCompletedToolDisclosure([]toolDisclosureRow{{sectionKey: "tool", line: "  done", output: source, outputExpanded: true, settled: true}})
	m.toggleToolDisclosure(record.id)
	m.appendLine("held-after-output")
	m.followBottom = false
	for n, cells := range m.transcriptRows(80) {
		if strings.Contains(ui.CellsToString(cells), "held-after-output") {
			m.scrollAnchor = n
		}
	}
	next := source.pageRequest(1)
	next.setPage(next.body[next.offset:])
	if !installInlineToolOutput(m, "tool", source, next) {
		t.Fatal("current output page was rejected")
	}
	if got := ui.CellsToString(m.transcriptRows(80)[m.scrollAnchor]); !strings.Contains(got, "held-after-output") {
		t.Fatalf("page replacement moved held viewport to %q", got)
	}
	if installInlineToolOutput(m, "tool", source, source) || record.rows[0].output != next {
		t.Fatal("late old-page read overwrote current page")
	}
}

func TestInlineChangesRevealsEveryRecordedFile(t *testing.T) {
	changes := &fileChanges{tracked: true, omitted: 2}
	for i := range 7 {
		changes.changes = append(changes.changes, fileChange{path: fmt.Sprintf("file-%d", i), diff: fmt.Sprintf("--- old\n+++ new\n@@ -0 +1 @@\n+body-%d\n", i)})
	}
	row := toolDisclosureRow{pres: toolPresentation{changes: changes}}
	text := plainStyledText(row.changeDetail(20))
	for i := range 7 {
		if !strings.Contains(text, fmt.Sprintf("file-%d", i)) || !strings.Contains(text, fmt.Sprintf("+body-%d", i)) {
			t.Fatalf("file %d was hidden: %s", i, text)
		}
	}
	if !strings.Contains(text, "2 files not recorded") {
		t.Fatal("recording omissions were not distinguished from display pagination")
	}
}

func TestInlineOutputPagesCoverRecordedBytes(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		minPages   int
	}{
		{"CRLF lines", strings.Repeat("numbered line\r\n", inlineOutputLines*2+11), 2},
		{"long lines", strings.Repeat(strings.Repeat("x", 2000)+"\n", 400), 2},
		{"UTF-8 long line", strings.Repeat("界", inlineOutputBytes) + "last-byte", 2},
		{"CRLF byte boundary", strings.Repeat("x", inlineOutputBytes-1) + "\r\n" + strings.Repeat("tail\n", 510), 2},
		{"exact line budget", strings.Repeat("n\n", inlineOutputLines), 1},
		{"CR lines", strings.Repeat("line\r", 1001), 2},
		{"mixed line endings", strings.Repeat("one\rtwo\r\nthree\n", 401), 2},
		{"CRLF line boundary", strings.Repeat("line\r", inlineOutputLines-1) + "split\r\nlast", 2},
	} {
		for _, artifact := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/artifact=%v", tc.name, artifact), func(t *testing.T) {
				body := tc.body
				o := newInlineToolOutput(messages.ChatMessageToolCall{}, messages.ChatMessage{Content: body})
				var store artifacts.Store
				if artifact {
					store = testArtifactStore(t)
					ref, err := store.Put(context.Background(), artifacts.Blob{Kind: artifacts.KindText, Data: []byte(body)})
					if err != nil {
						t.Fatal(err)
					}
					o = newInlineToolOutput(messages.ChatMessageToolCall{}, messages.ChatMessage{Content: "preview", Parts: []messages.ContentPart{{Type: "artifact", Artifact: &ref}}})
					o = readInlineToolOutput(context.Background(), store, o, 0)
				}
				var starts []int64
				var captured strings.Builder
				for {
					if o.notice != "" || o.offset != int64(captured.Len()) || o.end <= o.offset || o.end > int64(len(body)) || o.end-o.offset > inlineOutputBytes {
						t.Fatalf("invalid page: %+v", o)
					}
					part := body[o.offset:o.end]
					if !utf8.ValidString(part) || strings.Count(part, "\n") > inlineOutputLines {
						t.Fatalf("broken UTF-8 or raw line budget at %d–%d", o.offset, o.end)
					}
					// A lone CR is a valid line ending, but a CRLF pair must stay together.
					if o.end < int64(len(body)) && strings.HasSuffix(part, "\r") && body[o.end] == '\n' {
						t.Fatal("page split a CRLF pair")
					}
					if o.more && strings.ContainsAny(part, "\r\n") && !strings.HasSuffix(part, "\n") && !strings.HasSuffix(part, "\r") {
						t.Fatal("page split an ordinary line despite an earlier newline")
					}
					text := plainStyledText(o.text)
					if len(text) > inlineOutputBytes || strings.Count(text, "\n")+1 > inlineOutputLines {
						t.Fatalf("display budget exceeded at %d–%d", o.offset, o.end)
					}
					if text != strings.TrimRight(cleanInlineToolOutput(part), "\n") {
						t.Fatalf("page text differs at %d–%d", o.offset, o.end)
					}
					starts = append(starts, o.offset)
					captured.WriteString(part)
					if !o.more {
						break
					}
					if artifact {
						o = readInlineToolOutput(context.Background(), store, o, 1)
					} else {
						o = o.pageRequest(1)
						o.setPage(body[o.offset:])
					}
				}
				if len(starts) < tc.minPages || captured.String() != body {
					t.Fatal("paging stranded recorded bytes or skipped a required page")
				}
				for n := len(starts) - 2; n >= 0; n-- {
					if artifact {
						o = readInlineToolOutput(context.Background(), store, o, -1)
					} else {
						o = o.pageRequest(-1)
						o.setPage(body[o.offset:])
					}
					if o.offset != starts[n] || !o.more || o.notice != "" {
						t.Fatal("previous page did not restore the recorded boundary")
					}
				}
			})
		}
	}
}

func TestInlineKeyboardOutputProgressionAndEscape(t *testing.T) {
	withDisplayTTY(t)
	r, screen := affordanceTestREPL(t)
	t.Cleanup(func() { _ = r.work.close() })
	screen.SetSize(80, 30)
	m := r.model
	call := messages.ChatMessageToolCall{ID: "long", Name: "bash"}
	m.hydrateHistory([]messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "read"},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{call}},
		{Role: messages.MessageRoleTool, ToolCallID: call.ID, Content: strings.Repeat("page one\n", inlineOutputLines) + "last page"},
	}, "ctx")
	m.ed.setText("draft")
	inlineTestKey(r, "<C-o>")
	link := m.toolOutputLinks[0]
	chooseInlineTestAction(t, r, link.keyboardKey())
	inlineTestKey(r, "<Enter>")
	link.action = "next"
	chooseInlineTestAction(t, r, link.keyboardKey())
	inlineTestKey(r, "<Enter>")
	record, row := m.toolOutputRow(link)
	if row.output.offset == 0 || row.output.more || !strings.Contains(plainStyledText(row.output.text), "last page") {
		t.Fatal("keyboard Next did not reveal the last output page")
	}
	link.action = "previous"
	chooseInlineTestAction(t, r, link.keyboardKey())
	inlineTestKey(r, "<Escape>")
	if row.output.offset == 0 || m.inlineKeyboardAction != "" || m.ed.text() != "draft" {
		t.Fatal("Escape activated the page or altered draft")
	}
	chooseInlineTestAction(t, r, link.keyboardKey())
	inlineTestKey(r, "<Enter>")
	if row.output.offset != 0 || !row.output.more || m.ed.text() != "draft" {
		t.Fatal("keyboard Previous lost the first page or submitted draft")
	}
	// Detailed mode retains the selected page action; only the next global
	// step folds its enclosure and removes the target before Enter.
	nextKey := toolOutputLink{key: link.key, action: "next"}.keyboardKey()
	chooseInlineTestAction(t, r, nextKey)
	if !m.toggleAllDisclosures(80) || m.disclosures != disclosuresDetailed || !record.expanded || !row.outputExpanded {
		t.Fatal("second Ctrl-O did not retain the open output in detailed mode")
	}
	r.render()
	if action, ok := r.selectedInlineAction(); !ok || action.key != nextKey {
		t.Fatal("detailed mode lost the selected page action")
	}
	if !m.toggleAllDisclosures(80) || m.disclosures != disclosuresFolded || record.expanded || row.outputExpanded {
		t.Fatal("third Ctrl-O did not fold the output and its enclosure")
	}
	r.render()
	if _, ok := r.selectedInlineAction(); ok || m.inlineKeyboardAction != nextKey {
		t.Fatal("folding did not leave the disappeared target selected by identity")
	}
	inlineTestKey(r, "<Enter>")
	if m.ed.text() != "draft" || m.inlineKeyboardAction != "" || row.output.offset != 0 {
		t.Fatal("disappeared action submitted draft or activated the old page action")
	}
}

func TestAgentInlineOutputPageKeyboardAndRefresh(t *testing.T) {
	withDisplayTTY(t)
	fixture, screen := affordanceTestREPL(t)
	t.Cleanup(func() { _ = fixture.work.close() })
	for _, saved := range []bool{false, true} {
		t.Run(fmt.Sprint(saved), func(t *testing.T) {
			store := testOpenMemoryStore(t, nil)
			r := newTabTestREPL(t, store, "root")
			r.setupWidgets()
			screen.SetSize(160, 40)
			child, err := store.Acquire(context.Background(), "agent", sessions.AcquireOptions{Parent: "root"})
			if err != nil {
				t.Fatal(err)
			}
			ref, err := child.ArtifactStore().Put(context.Background(), artifacts.Blob{Kind: artifacts.KindText, Data: []byte(strings.Repeat("first page\n", inlineOutputLines) + "last agent page")})
			if err != nil {
				t.Fatal(err)
			}
			call := messages.ChatMessageToolCall{ID: "read", Name: "read_file", Arguments: `{"path":"a.txt"}`}
			history := []messages.ChatMessage{
				{Role: messages.MessageRoleUser, Content: "read"},
				{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{call}},
				{Role: messages.MessageRoleTool, ToolCallID: call.ID, Content: "preview", Parts: []messages.ContentPart{{Type: "artifact", Artifact: &ref}}},
			}
			var source *replModel
			if saved {
				testAddMessages(t, child, history)
				if err := child.Close(); err != nil {
					t.Fatal(err)
				}
				r.inspect(viewTarget{session: sessions.ViewTarget{Name: "agent"}})
				r.workspace().viewState(r.workspace().inspector.target).watch = true
			} else {
				t.Cleanup(func() { _ = child.Close() })
				source = newReplModel()
				source.artifactStore = child.ArtifactStore()
				source.hydrateHistory(history, "agent")
				watchTestAgent(r, source)
			}
			v := waitInspector(t, r, 160)
			r.workspace().inspector.focused = true
			inlineTestKey(r, "<C-o>")
			v = waitInspector(t, r, 160)
			r.render()
			if len(v.model.toolOutputLinks) != 1 {
				t.Fatalf("agent row not reachable: %+v", v.model.toolOutputLinks)
			}
			link := v.model.toolOutputLinks[0]
			choose := func(action string) {
				t.Helper()
				link.action = action
				for range len(r.inspectorKeyboardActions()) + 1 {
					inlineTestKey(r, "<S-Tab>")
					if selected, ok := r.selectedInspectorAction(); ok && selected.key == link.keyboardKey() {
						inlineTestKey(r, "<Enter>")
						return
					}
				}
				t.Fatalf("agent page action %q unavailable: %+v", action, r.inspectorKeyboardActions())
			}
			waitPage := func(offset int64) {
				t.Helper()
				deadline := time.NewTimer(3 * time.Second)
				defer deadline.Stop()
				for {
					v = waitInspector(t, r, 160)
					r.render()
					_, row := v.model.toolOutputRow(link)
					if row != nil && row.outputExpanded && !row.outputLoading && row.output != nil && row.output.loaded && row.output.offset == offset {
						return
					}
					select {
					case task := <-r.uiTasks:
						task()
					case <-deadline.C:
						t.Fatalf("agent page %d did not load: %+v", offset, row)
					}
				}
			}
			choose("")
			waitPage(0)
			choose("next")
			waitPage(int64(len("first page\n") * inlineOutputLines))
			_, row := v.model.toolOutputRow(link)
			if row.output.offset == 0 || row.output.more || !strings.Contains(row.output.text, "last agent page") {
				t.Fatal("agent keyboard Next did not reveal final recorded output")
			}
			if source != nil {
				source.appendLine("new agent message")
			} else {
				// Retire a settled projection so it remains in the bounded cache.
				v = waitInspector(t, r, 160)
				// Reopening the read-only view also re-applies its section state.
				target := r.workspace().inspector.target
				r.closeInspector()
				r.inspect(target)
				r.workspace().viewState(r.workspace().inspector.target).watch = true
			}
			v = waitInspector(t, r, 160)
			r.workspace().inspector.focused = true
			r.render()
			_, row = v.model.toolOutputRow(link)
			if row == nil || row.output.offset == 0 || !row.outputExpanded {
				t.Fatal("agent refresh lost the selected output page")
			}
			choose("previous")
			waitPage(0)
			_, row = v.model.toolOutputRow(link)
			if row.output.offset != 0 || !row.output.more {
				t.Fatal("agent keyboard Previous lost the first page")
			}
			if source != nil {
				for _, record := range source.toolDisclosures.all() {
					if record.rows[0].outputExpanded {
						t.Fatal("pane navigation mutated the running agent")
					}
				}
			}
		})
	}
}

// A canceled skip must stop promptly, and every successful Open must close,
// even when the next page fails. The previous page remains available to retry.
type inlinePageTestStore struct {
	artifacts.Store
	body         string
	read, closed int
	cancel       context.CancelFunc
}

func (s *inlinePageTestStore) Open(context.Context, string) (io.ReadCloser, error) {
	return &inlinePageTestReader{Reader: strings.NewReader(s.body), store: s}, nil
}

type inlinePageTestReader struct {
	io.Reader
	store *inlinePageTestStore
}

func (r *inlinePageTestReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.store.read += n
	if r.store.cancel != nil {
		r.store.cancel()
	}
	return n, err
}

func (r *inlinePageTestReader) Close() error { r.store.closed++; return nil }

func TestInlineArtifactPageCancellationAndRetry(t *testing.T) {
	body := strings.Repeat("x", inlineOutputBytes*3)
	ref := artifacts.Ref{ID: "test", Kind: artifacts.KindText}
	source := newInlineToolOutput(messages.ChatMessageToolCall{}, messages.ChatMessage{Content: "preview", Parts: []messages.ContentPart{{Type: "artifact", Artifact: &ref}}})
	store := &inlinePageTestStore{body: body}
	first := readInlineToolOutput(context.Background(), store, source, 0)
	if store.read != inlineOutputBytes+1 || store.closed != 1 || !first.more {
		t.Fatalf("initial artifact read was not bounded: %+v", store)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store.cancel, store.read = cancel, 0
	failed := readInlineToolOutput(ctx, store, first, 1)
	if failed.notice == "" || failed.text != first.text || failed.offset != first.offset || failed.retry != 1 || store.closed != 2 || store.read > inlineOutputBytes {
		t.Fatal("canceled page did not retain previous content, bound the skip or close the reader")
	}
	store.cancel = nil
	next := readInlineToolOutput(context.Background(), store, failed, failed.retry)
	if next.offset != first.end || next.notice != "" || !next.more {
		t.Fatal("retry stranded the remaining output")
	}
}
