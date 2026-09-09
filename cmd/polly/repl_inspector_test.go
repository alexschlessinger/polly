package main

import (
	"context"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/termimg"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/gdamore/tcell/v3"
	ui "github.com/metaspartan/gotui/v5"
)

func waitInspector(t *testing.T, r *managedREPL, width int) *viewInstance {
	t.Helper()
	r.refreshInspector(width)
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		v := r.workspace().inspector.current
		if v != nil && !v.loading && v.model != nil {
			return v
		}
		select {
		case fn := <-r.uiTasks:
			fn()
		case <-deadline.C:
			t.Fatal("inspector did not load")
		}
	}
}

func inspectorText(v *viewInstance) string {
	return plainStyledText(strings.Join(transcriptTexts(v.model), "\n"))
}

func TestAgentInspectorCollapsesOnlyLaunchPrompt(t *testing.T) {
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "private launch task\nwith another line"},
		{Role: messages.MessageRoleAssistant, Content: "First agent message."},
		{Role: messages.MessageRoleUser, Content: "visible follow-up"},
		{Role: messages.MessageRoleAssistant, Content: "Second agent message."},
	}
	rendered := func(v *viewInstance, width int) string {
		var rows []string
		for _, row := range v.view.Rows(v.model, width) {
			rows = append(rows, plainCells(row))
		}
		return strings.Join(rows, "\n")
	}
	for _, live := range []bool{true, false} {
		t.Run(fmt.Sprintf("live=%v", live), func(t *testing.T) {
			store := testOpenMemoryStore(t, nil)
			r := newTabTestREPL(t, store, "root")
			r.model.hydrateHistory(history, "root")
			var target viewTarget
			var source *replModel
			if live {
				r = newTabTestREPL(t, store, "live-root", "agent")
				child := r.tabs[1]
				r.showTab(0)
				r.model.hydrateHistory(history, "live-root")
				child.model.hydrateHistory(history, "agent")
				source, target = child.model, tabViewTarget(child)
			} else {
				child, err := store.Acquire(context.Background(), "agent", sessions.AcquireOptions{Parent: "root"})
				if err != nil {
					t.Fatal(err)
				}
				if err := child.AddMessages(context.Background(), history); err != nil {
					t.Fatal(err)
				}
				if err := child.Close(); err != nil {
					t.Fatal(err)
				}
				target = viewTarget{session: sessions.ViewTarget{Name: "agent"}}
			}
			r.inspect(target)
			for _, width := range []int{140, 240} {
				v := waitInspector(t, r, width)
				got := rendered(v, r.inspectorGeometry(width).width)
				if !strings.HasPrefix(got, "▸ Prompt\nFirst agent message.") || strings.Contains(got, "private launch task") || !strings.Contains(got, "visible follow-up") || !strings.Contains(got, "Second agent message.") {
					t.Fatalf("unexpected agent inspector transcript: %q", got)
				}
				if !strings.Contains(inspectorText(v), "private launch task") {
					t.Fatal("display filtering removed the underlying prompt")
				}
			}
			r.inspectorAction("prompt")
			if got := rendered(waitInspector(t, r, 140), 50); !strings.HasPrefix(got, "▾ Prompt\n") || !strings.Contains(got, "private launch task") || !strings.Contains(got, "with another line") {
				t.Fatalf("expanded prompt missing: %q", got)
			}
			r.closeInspector()
			r.inspectCommand("")
			if got := rendered(waitInspector(t, r, 240), 70); !strings.Contains(got, "private launch task") {
				t.Fatal("cached view lost prompt expansion")
			}
			r.inspectorAction("prompt")
			if source != nil {
				got := rendered(&viewInstance{model: source, view: conversationView{}}, 80)
				if !strings.Contains(got, "private launch task") {
					t.Fatal("inspector changed the live agent transcript")
				}
			}
			r.closeInspector()
			r.inspectCommand("")
			if got := rendered(waitInspector(t, r, 140), 50); strings.Contains(got, "private launch task") {
				t.Fatal("cached inspector restored the launch prompt")
			}
			r.inspect(tabViewTarget(r.visibleTab()))
			if got := rendered(waitInspector(t, r, 140), 50); !strings.Contains(got, "private launch task") {
				t.Fatal("main-session inspector lost its user prompt")
			}
		})
	}
}

func TestAgentInspectorRetainsPromptsAfterHistoryWindowAndClear(t *testing.T) {
	m := newReplModel()
	var history []messages.ChatMessage
	for n := 0; n < resumedTurnLimit+1; n++ {
		history = append(history, messages.ChatMessage{Role: messages.MessageRoleUser, Content: fmt.Sprintf("task %d", n)}, messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "reply"})
	}
	m.hydrateHistory(history, "agent")
	view := conversationView{collapseInitialPrompt: true}
	if got := plainCells(view.Rows(m, 80)[0]); !strings.Contains(got, "task 1") {
		t.Fatalf("truncated history hid a follow-up: %q", got)
	}
	m.clearDisplay()
	m.appendUserPrompt("after clear")
	if got := plainCells(view.Rows(m, 80)[0]); !strings.Contains(got, "after clear") {
		t.Fatalf("clear caused another prompt to be hidden: %q", got)
	}
}

func TestAgentInspectorPromptClick(t *testing.T) {
	withDisplayTTY(t)
	fixture, screen := affordanceTestREPL(t)
	t.Cleanup(func() { _ = fixture.work.close() })
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root", "agent")
	r.setupWidgets()
	r.showTab(0)
	child := r.tabs[1]
	child.model.hydrateHistory([]messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "the launch task"},
		{Role: messages.MessageRoleAssistant, Content: "the answer"},
	}, "agent")
	r.inspect(tabViewTarget(child))
	for _, width := range []int{140, 80} {
		screen.SetSize(width, 32)
		waitInspector(t, r, width)
		r.render()
		button := headerButton(r.inspectorButtons, "prompt")
		if button.Empty() || button.Min != r.inspectorW.Inner.Min {
			t.Fatalf("prompt control does not match the first body row: %v", button)
		}
		for _, expanded := range []bool{true, false} {
			r.handleEvent(mouseEvent("<MouseLeft>", button.Min))
			r.render()
			m := r.workspace().inspector.current.model
			if m.initialPromptExpanded != expanded || len(r.inspectorW.OverlayBottom) != 0 {
				t.Fatal("prompt click failed or was treated as new output")
			}
		}
	}
}

func TestInspectorToolResultPreservesComposerAndInlineSummary(t *testing.T) {
	withDisplayTTY(t)
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "root")
	m := r.model
	m.ed.setText("keep my draft")
	call := messages.ChatMessageToolCall{ID: "one", Name: "fetch", Arguments: `{"url":"https://x"}`}
	tui := &gotuiTurnUI{model: m, config: r.config, repl: r}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	tui.AppendToolEnd(call, `{"answer":42}`, time.Second, nil)
	record, _ := m.toolDisclosureRowForCall(call.ID)
	before := m.transcript[record.transcriptIndex].text
	r.inspectCommand("")
	v := waitInspector(t, r, 140)
	if !strings.Contains(inspectorText(v), `"answer": 42`) {
		t.Fatalf("result = %s", inspectorText(v))
	}
	// The state belongs to the header; the body opens with the arguments fence.
	if first := strings.Split(inspectorText(v), "\n")[0]; strings.Contains(first, "fetch") || first != "╭─ arguments · json" {
		t.Fatalf("expected the arguments fence without a repeated tool name: %q", first)
	}
	if !strings.Contains(inspectorText(v), `"url": "https://x"`) || !strings.Contains(inspectorText(v), "╭─ output · 3 lines") {
		t.Fatalf("tool arguments and output are not fenced by default: %s", inspectorText(v))
	}
	if !strings.Contains(inspectorText(v), "╭─ arguments · json\n│ {") || !strings.Contains(strings.Join(transcriptTexts(v.model), "\n"), style.Styled(`"https://x"`, "ok", "")) {
		t.Fatalf("arguments lack JSON code-block highlighting: %s", strings.Join(transcriptTexts(v.model), "\n"))
	}
	header := r.inspectorHeader(60, 20, 0, 0)
	if rows := strings.Split(plainStyledText(header.text), "\n"); len(rows) != 2 || !strings.HasPrefix(rows[0], "‹ fetch") || !strings.HasPrefix(rows[1], "completed · 1.0s") {
		t.Fatalf("tool header = %q", plainStyledText(header.text))
	}
	if m.ed.text() != "keep my draft" || r.model != m {
		t.Fatal("inspection stole composer")
	}
	if m.transcript[record.transcriptIndex].text != before || strings.Contains(before, "answer") {
		t.Fatal("inspection changed inline summary")
	}
	if len(r.tabs) != 1 {
		t.Fatal("inspection created a runtime tab")
	}
}

func TestInspectorWholeConversationAndDuplicateCallIDs(t *testing.T) {
	m := newReplModel()
	var history []messages.ChatMessage
	for n := 0; n < 8; n++ {
		call := messages.ChatMessageToolCall{ID: "reused", Name: "bash", Arguments: fmt.Sprintf(`{"command":"echo %d"}`, n)}
		history = append(history, messages.ChatMessage{Role: messages.MessageRoleUser, Content: fmt.Sprint(n)}, messages.ChatMessage{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{call}}, messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: call.ID, ToolName: call.Name, Content: fmt.Sprint(n)})
	}
	m.hydrateHistory(history, "history")
	if len(m.inspections.tools) != 8 {
		t.Fatalf("catalog size %d", len(m.inspections.tools))
	}
	seen := map[string]bool{}
	for n, tool := range m.inspections.tools {
		if seen[tool.key] || tool.result.Content != fmt.Sprint(n) {
			t.Fatalf("incorrect pairing: %#v", tool)
		}
		seen[tool.key] = true
	}
	for _, record := range m.toolDisclosures.all() {
		for _, row := range record.rows {
			if !seen[row.inspectionKey] {
				t.Fatal("inline row has no catalogue identity")
			}
		}
	}
}

func TestInspectorThoughtsRetainFullTextWithoutExpandingInline(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "root")
	m := r.model
	full := "BEGIN\n" + strings.Repeat("thinking line\n", 3000) + "END"
	m.appendThinking(full)
	r.inspectCommand("thoughts")
	v := waitInspector(t, r, 140)
	if !strings.Contains(inspectorText(v), "BEGIN") || !strings.Contains(inspectorText(v), "END") {
		t.Fatal("thought view lost full text")
	}
	record := m.currentReasoningRecord()
	if record.expanded || len(record.tail) > reasoningTailLimitRunes {
		t.Fatal("inline thought behavior changed")
	}
	m.appendThinking("\nUPDATE")
	v = waitInspector(t, r, 140)
	if !strings.Contains(inspectorText(v), "UPDATE") {
		t.Fatal("live thought did not refresh")
	}
}

func TestInspectorHistoryAndToolSequenceAreSeparate(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "root")
	for _, name := range []string{"bash", "spawn_agent", "read_file"} {
		r.model.appendToolCallStart(messages.ChatMessageToolCall{ID: name, Name: name})
	}
	r.inspectCommand("tools")
	waitInspector(t, r, 140)
	last := r.workspace().inspector.target.item
	r.inspectorSequence(-1)
	waitInspector(t, r, 140)
	if !strings.Contains(r.workspace().inspector.target.item, "spawn_agent") {
		t.Fatal("sequence skipped launch")
	}
	r.inspectorHistory(-1)
	waitInspector(t, r, 140)
	if r.workspace().inspector.target.item != last {
		t.Fatal("history did not return to prior selection")
	}
	r.inspectorSequence(1)
	if r.workspace().inspector.target.item != last {
		t.Fatal("sequence wrapped")
	}
}

func TestInspectorArrowNavigationFollowsFocus(t *testing.T) {
	for _, kind := range []string{"tools", "thoughts"} {
		t.Run(kind, func(t *testing.T) {
			store := testOpenMemoryStore(t, nil)
			r := newTabTestREPL(t, store, "root")
			var history []messages.ChatMessage
			for n := 0; n < 3; n++ {
				call := messages.ChatMessageToolCall{ID: fmt.Sprint(n), Name: "bash"}
				history = append(history,
					messages.ChatMessage{Role: messages.MessageRoleUser, Content: fmt.Sprint(n)},
					messages.ChatMessage{Role: messages.MessageRoleAssistant, Reasoning: fmt.Sprintf("thought %d", n), ToolCalls: []messages.ChatMessageToolCall{call}},
					messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: call.ID, Content: "done"},
				)
			}
			r.model.hydrateHistory(history, "root")
			r.model.ed.setText("draft")
			r.inspectCommand(kind)
			waitInspector(t, r, 140)
			key := func(id string) { r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: id}) }
			i := &r.workspace().inspector
			last := i.target
			key("<Left>")
			if r.model.ed.cursor != 4 || i.target != last {
				t.Fatal("an unfocused inspector stole an editor key")
			}
			key("<Right>")
			// Pointer position never matters: hovering the inspector leaves the keys alone.
			r.handleEvent(convertTcellMouse(tcell.NewEventMouse(100, 10, tcell.ButtonNone, tcell.ModNone)))
			key("<Left>")
			if r.model.ed.cursor != 4 || i.target != last {
				t.Fatal("hovering the inspector redirected an editor key")
			}
			key("<Right>")
			i.focused = true
			for _, step := range []struct {
				key   string
				index int
			}{{"<Left>", 2}, {"<Left>", 1}, {"<Left>", 1}, {"<Right>", 2}, {"<Right>", 3}, {"<Right>", 3}} {
				key(step.key)
				waitInspector(t, r, 140)
				index, total, _, _ := inspectorSequencePosition(i)
				if index != step.index || total != 3 || r.model.ed.text() != "draft" || r.model.ed.cursor != 5 {
					t.Fatalf("%s: item %d/%d, draft %q at %d", step.key, index, total, r.model.ed.text(), r.model.ed.cursor)
				}
			}
			for _, mode := range []string{"result search", "history search", "dialog", "approval", "paste"} {
				switch mode {
				case "result search":
					i.searching = true
				case "history search":
					r.model.hist.startSearch()
				case "dialog":
					r.openModal(&replModal{inputMode: true})
				case "approval":
					r.model.approval = &approvalState{}
				case "paste":
					key(pasteStartID)
				}
				key("<Left>")
				if i.target != last {
					t.Fatalf("focused navigation stole %s input", mode)
				}
				i.searching = false
				r.model.hist.searching = false
				r.model.modal = nil
				r.model.approval = nil
				r.model.pasting = false
			}
			key("x")
			if r.model.ed.text() != "draftx" || i.focused {
				t.Fatal("typing did not return the keys to the composer")
			}
			i.focused = true
			key("<Escape>")
			if i.focused || !i.open {
				t.Fatal("Escape did not return focus before closing the inspector")
			}
			i.focused = true
			r.closeInspector()
			key("<Left>")
			if r.model.ed.cursor != 5 || i.focused {
				t.Fatal("closed inspector consumed arrow input")
			}
		})
	}
}

// Tab on an empty composer hands the navigation keys to the inspector; the
// editor's control-key shortcuts and typing always reach the composer.
func TestFocusedNavigationAddressesInspector(t *testing.T) {
	withDisplayTTY(t)
	r, screen := affordanceTestREPL(t)
	t.Cleanup(func() { _ = r.work.close() })
	r.model.appendLine(strings.Repeat("main transcript line\n", 100))
	r.model.appendThinking(strings.Repeat("inspected thought\n", 100))
	r.inspectCommand("thoughts")
	key := func(id string) { r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: id}); r.render() }
	for _, mode := range []struct {
		width     int
		maximized bool
	}{{140, false}, {100, false}, {140, true}} {
		screen.SetSize(mode.width, 40)
		i := &r.workspace().inspector
		i.maximized = mode.maximized
		i.focused = false
		waitInspector(t, r, mode.width)
		r.render()
		s := r.workspace().viewState(i.target)
		r.model.ed.setText("draft")
		key("<Tab>")
		if i.focused {
			t.Fatal("Tab with a draft focused the inspector instead of completing")
		}
		r.model.ed.setText("")
		key("<Tab>")
		if !i.focused {
			t.Fatal("Tab on an empty composer did not focus the inspector")
		}
		if _, _, visible := screen.GetCursor(); visible {
			t.Fatal("composer cursor shown while the inspector has the keys")
		}
		key("<Home>")
		key("<Down>")
		if s.top != 1 || s.follow {
			t.Fatalf("inspector line scrolling at width %d: %+v", mode.width, s)
		}
		key("<PageDown>")
		top := s.top
		if top <= 1 {
			t.Fatal("Page Down did not page inspector")
		}
		key("<Up>")
		if s.top != top-1 {
			t.Fatal("Up did not scroll inspector one row")
		}
		key("<PageUp>")
		if s.top != 0 {
			t.Fatal("Page Up did not restore inspector top")
		}
		key("<End>")
		if !s.follow {
			t.Fatal("End failed to follow inspector")
		}
		if !r.model.followBottom {
			t.Fatal("inspector paging scrolled the conversation")
		}
		r.model.ed.setText("draft")
		key("<C-a>")
		if r.model.ed.cursor != 0 || !i.focused {
			t.Fatal("Ctrl-A stopped addressing the editor")
		}
		key("<C-e>")
		if r.model.ed.cursor != 5 {
			t.Fatal("Ctrl-E stopped addressing the editor")
		}
		key("<Escape>")
		if i.focused || !i.open {
			t.Fatal("Escape did not hand the keys back")
		}
		top = s.top
		key("<Up>")
		if s.top != top || !s.follow {
			t.Fatal("unfocused Up scrolled the inspector")
		}
		r.model.ed.setText("")
		key("<Tab>")
		key("x")
		if r.model.ed.text() != "x" || i.focused {
			t.Fatal("typing did not return focus to the composer")
		}
		r.model.ed.setText("")
	}
}

func TestInspectorCacheEvictionPreservesViewState(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "root")
	r.model.appendThinking(strings.Repeat("line\n", 100))
	r.inspectCommand("thoughts")
	waitInspector(t, r, 140)
	w := r.workspace()
	target := w.inspector.target
	s := w.viewState(target)
	s.follow = false
	s.top = 12
	s.search = "line"
	r.closeInspector()
	if r.childViews.entries["inspector:"+target.key()] == nil {
		t.Fatal("view was not cached")
	}
	r.childViews.take("inspector:" + target.key())
	r.inspectCommand("")
	waitInspector(t, r, 160)
	if s.top != 12 || s.follow || s.search != "line" {
		t.Fatalf("eviction changed state: %#v", s)
	}
	if r.workspace().inspector.current.geometry.width != 50 {
		t.Fatalf("geometry = %#v", r.workspace().inspector.current.geometry)
	}
}

func TestInspectorLoadsFullArtifact(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "root")
	full := "artifact head\n" + strings.Repeat("body\n", 2000) + "artifact tail"
	ref, err := r.state.artifactStore.Put(context.Background(), artifacts.Blob{Kind: artifacts.KindText, MIMEType: "text/plain", Data: []byte(full)})
	if err != nil {
		t.Fatal(err)
	}
	call := messages.ChatMessageToolCall{ID: "artifact", Name: "read_file"}
	r.model.appendToolCallStart(call)
	r.model.inspections.setResult(call, messages.ChatMessage{Role: messages.MessageRoleTool, Content: "preview", Parts: []messages.ContentPart{{Type: "artifact", Artifact: &ref}}})
	r.inspectCommand("tools")
	v := waitInspector(t, r, 140)
	if !strings.Contains(inspectorText(v), "artifact head") || !strings.Contains(inspectorText(v), "artifact tail") {
		t.Fatalf("full artifact missing: %s", inspectorText(v))
	}
}

func TestInspectorEscapeClosesBeforeCancelAndTypingStaysInComposer(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "root")
	r.model.appendThinking("inspect me")
	r.inspectCommand("thoughts")
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "x"})
	if r.model.ed.text() != "x" {
		t.Fatal("opening inspection took keyboard focus")
	}
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<F6>"})
	for _, key := range []string{"z", "[", "]", "b", "f", "/", "n", "m", "-", "+", "a", "r"} {
		r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: key})
	}
	if r.model.ed.text() != "xz[]bf/nm-+ar" {
		t.Fatal("inspector consumed composer input after F6")
	}
	r.model.busy = true
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Escape>"})
	if r.workspace().inspector.open || r.model.canceling {
		t.Fatal("Escape canceled instead of closing inspection")
	}
	r.model.busy = false
}

func TestInspectorInlineToolAndThoughtLinksDoNotReplaceDropdowns(t *testing.T) {
	withDisplayTTY(t)
	m := newReplModel()
	m.beginTurn("question")
	m.appendThinking("thought content")
	thought := m.currentReasoningRecord()
	thought.expanded = true
	m.refreshReasoningRecord(thought, 100)
	call := messages.ChatMessageToolCall{ID: "cmd", Name: "bash", Arguments: `{"command":"echo unique"}`}
	record := m.appendToolCallStart(call)
	record.expanded = true
	m.refreshToolDisclosure(record)
	rows := m.transcriptRows(100)
	v := (frameLayout{width: 100, transcriptHeight: 60}).transcriptViewport(len(rows), 0, false, 0)
	links := m.visibleInspectionLinks(v, 0)
	foundTool, foundThought := false, false
	for _, link := range links {
		if link.kind == toolViewKind {
			foundTool = true
		}
		if link.kind == thoughtViewKind {
			foundThought = true
		}
		if link.rect.Min.Y < 0 || link.rect.Max.Y > len(rows) {
			t.Fatalf("link outside content: %#v", link)
		}
	}
	if !foundTool || !foundThought {
		t.Fatalf("missing links: %#v", links)
	}
	for _, header := range m.visibleDisclosurePlacements(v, activityTools) {
		for _, link := range links {
			if link.rect.Overlaps(image.Rect(header.X, header.Y, header.X+header.Cols, header.Y+1)) {
				t.Fatal("detail link swallowed dropdown header")
			}
		}
	}
}

func TestWorkspaceParentIdentitySurvivesDeletionAndNameReuse(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	ctx := context.Background()
	parent := testAcquireSession(t, store, "parent")
	child, err := store.Acquire(ctx, "child", sessions.AcquireOptions{Parent: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	testAddMessages(t, child, []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "kept"}})
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}
	e, err := resolveWorkspaceEntry(ctx, store.(sessions.ViewStore), "child")
	if err != nil {
		t.Fatal(err)
	}
	if e.root.Metadata.Name != "parent" || e.orphan {
		t.Fatalf("entry = %#v", e)
	}
	if err := store.Delete(ctx, "parent"); err != nil {
		t.Fatal(err)
	}
	replacement := testAcquireSession(t, store, "parent")
	defer replacement.Close()
	e, err = resolveWorkspaceEntry(ctx, store.(sessions.ViewStore), "child")
	if err != nil {
		t.Fatal(err)
	}
	if e.root.ID != e.selected.ID || !e.orphan {
		t.Fatal("reattached agent to unrelated reused parent name")
	}
}

func TestInspectorLateLoadCannotReplaceNewSelection(t *testing.T) {
	r, store, activity := savedChildViewFixture(t)
	gate := make(chan struct{})
	r.state.sessionStore = &gatedChildViewStore{SessionStore: store, reader: store, gate: gate}
	r.inspect(viewTarget{session: sessions.ViewTarget{Name: activity.session}})
	r.refreshInspector(140)
	old := r.workspace().inspector.current
	r.model.appendThinking("stay on this thought")
	r.inspectCommand("thoughts")
	want := r.workspace().inspector.target
	waitInspector(t, r, 140)
	close(gate)
	r.work.wg.Wait()
	drainUITasks(r)
	if r.workspace().inspector.target != want || r.workspace().inspector.current == old || !strings.Contains(inspectorText(r.workspace().inspector.current), "stay on this thought") {
		t.Fatal("late saved load replaced the selected view")
	}
}

func TestInspectorSectionsAndLogicalAnchorSurviveRefreshAndEviction(t *testing.T) {
	withDisplayTTY(t)
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "root")
	call := messages.ChatMessageToolCall{ID: "call", Name: "bash"}
	r.model.appendToolCallStart(call)
	for n := 0; n < 100; n++ {
		r.model.appendLine(fmt.Sprintf("line %03d: %s", n, strings.Repeat("content ", 12)))
	}
	r.inspect(tabViewTarget(r.visibleTab()))
	v := waitInspector(t, r, 140)
	s := r.workspace().viewState(v.target)
	for _, rec := range v.model.toolDisclosures.all() {
		v.model.toggleToolDisclosure(rec.id)
	}
	rememberViewSections(v.model, s)
	rows := v.view.Rows(v.model, v.geometry.width)
	s.follow = false
	for n, row := range rows {
		if strings.Contains(plainCells(row), "line 060:") {
			s.top = n
			break
		}
	}
	r.model.appendLine("new output")
	r.closeInspector()
	for key := range r.childViews.entries {
		r.childViews.take(key)
	}
	r.inspectCommand("")
	v = waitInspector(t, r, 120)
	if !strings.Contains(plainCells(v.model.visual.rows[s.top]), "line 060:") {
		t.Fatalf("lost logical anchor: %d %s", s.top, plainCells(v.model.visual.rows[s.top]))
	}
	for _, rec := range v.model.toolDisclosures.all() {
		if !rec.expanded {
			t.Fatal("expanded tools reset")
		}
	}
}

func plainCells(row []ui.Cell) string {
	var b strings.Builder
	for _, c := range row {
		b.WriteRune(c.Rune)
	}
	return b.String()
}

func TestWorkspaceSwitchRetiresInspectorWithoutLosingDraft(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "first", "second")
	r.model.appendThinking("second thought")
	r.model.ed.setText("root draft")
	r.inspectCommand("thoughts")
	waitInspector(t, r, 140)
	w := r.workspace()
	target := w.inspector.target
	r.showTab(0)
	if w.inspector.current != nil || !w.inspector.open {
		t.Fatal("inactive workspace retained display or lost selection")
	}
	if r.childViews.entries["inspector:"+target.key()] == nil {
		t.Fatal("retired view missing from shared cache")
	}
	r.showTab(1)
	waitInspector(t, r, 140)
	if r.model.ed.text() != "root draft" || r.workspace().inspector.target != target {
		t.Fatal("workspace did not restore")
	}
}

func TestInspectorAgentMessageAndApprovalKeepRootOwnership(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "root", "agent")
	root, child := r.tabs[0], r.tabs[1]
	child.parent, child.parentName, child.workspaceRoot = root, root.name, false
	r.showTab(0)
	root.model.ed.setText("main draft")
	child.model.busy = true
	defer func() { child.model.busy = false }()
	r.inspect(tabViewTarget(child))
	waitInspector(t, r, 140)
	r.messageInspectedAgent()
	r.applyTabRequests()
	if r.model.modal == nil || r.model.modal.title != "Message agent" {
		t.Fatal("editor not addressed")
	}
	r.model.modal.onSubmit("follow up")
	r.closeModal()
	r.applyTabRequests()
	if len(child.model.queue) != 1 || child.model.queue[0].text != "follow up" || len(root.model.queue) != 0 || root.model.ed.text() != "main draft" {
		t.Fatal("message ownership changed")
	}
	reply := make(chan []bool, 1)
	child.model.approval = &approvalState{calls: []messages.ChatMessageToolCall{{ID: "approve", Name: "bash"}}, reply: reply}
	target := r.workspace().inspector.target
	r.reviewAgentApproval(target)
	r.applyTabRequests()
	if r.model.modal == nil || !strings.Contains(r.model.modal.title, "agent") {
		t.Fatal("approval not addressed")
	}
	if body := plainStyledText(strings.Join(r.model.modal.body, "\n")); !strings.HasPrefix(body, "  ╭─ bash") {
		t.Fatalf("approval dialog does not show the call: %q", body)
	}
	for _, item := range r.model.modal.items {
		if item.value == "v" || item.value == "a" {
			t.Fatalf("single-call approval offers %q", item.label)
		}
	}
	r.model.modal.onSubmit("y")
	r.closeModal()
	r.applyTabRequests()
	select {
	case allowed := <-reply:
		if len(allowed) != 1 || !allowed[0] {
			t.Fatal("wrong approval")
		}
	default:
		t.Fatal("agent approval not delivered")
	}
	if r.workspace().inspector.target != target || r.model != root.model || root.model.ed.text() != "main draft" {
		t.Fatal("approval redirected workspace")
	}
}

func TestInspectorMediaFramesAndResize(t *testing.T) {
	withDisplayTTY(t)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	r, screen := affordanceTestREPL(t)
	t.Cleanup(func() { _ = r.work.close() })
	tty := &imageTestTTY{window: tcell.WindowSize{Width: 140, Height: 40, PixelWidth: 1400, PixelHeight: 800}}
	r.images = termimg.NewManagerFor(screen, tty, termimg.ProtocolKitty)
	t.Cleanup(func() { r.images.Shutdown() })
	r.model.nativeImages = true
	store := testArtifactStore(t)
	r.model.artifactStore = store
	path := filepath.Join(t.TempDir(), "image.png")
	writeImageFixture(t, path, 24, 12)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.Put(context.Background(), artifacts.Blob{Kind: artifacts.KindImage, MIMEType: "image/png", Name: "image.png", Data: data})
	if err != nil {
		t.Fatal(err)
	}
	call := messages.ChatMessageToolCall{ID: "image", Name: "view_image"}
	r.model.appendToolCallStart(call)
	r.model.inspections.setResult(call, messages.ChatMessage{Role: messages.MessageRoleTool, Parts: []messages.ContentPart{{Type: "image_artifact", Artifact: &ref}}})
	r.inspect(tabViewTarget(r.visibleTab()))
	waitInspector(t, r, 140)
	r.inspectCommand("tools")
	for _, width := range []int{140, 80, 120, 180} {
		screen.SetSize(width, 40)
		waitInspector(t, r, width)
		r.render()
		// The frame's right edge owns the last column; content stops before it.
		if r.chrome.inner.Max.X != width-1 {
			t.Fatal("inspector escapes pane")
		}
		if width < 120 && r.chrome.inner.Min.X != 1 {
			t.Fatal("narrow inspector did not fill width")
		}
		if r.chrome.inner.Dx() < 50 {
			t.Fatal("pane narrower than minimum")
		}
		if r.images.ActiveCount() != 1 {
			t.Fatalf("image not placed at width %d: %d", width, r.images.ActiveCount())
		}
		for _, p := range r.workspace().inspector.current.model.imagePlacements {
			if !image.Rect(p.X, p.Y, p.X+p.Cols, p.Y+p.Rows).In(r.chrome.inner) || !strings.HasPrefix(p.Key, "inspector:") {
				t.Fatalf("image outside inspector: %#v", p)
			}
		}
	}
	r.inspectorAction("maximize")
	waitInspector(t, r, 180)
	r.render()
	if r.chrome.inner.Min.X != 1 || !r.chrome.main.Empty() {
		t.Fatal("maximize left root transcript visible")
	}
	r.closeInspector()
	r.render()
	if r.images.ActiveCount() != 0 {
		t.Fatal("closing inspector left image displayed")
	}
}

func TestInspectorOpensFromSettledDropdownDetails(t *testing.T) {
	withDisplayTTY(t)
	r, screen := affordanceTestREPL(t)
	t.Cleanup(func() { _ = r.work.close() })
	screen.SetSize(140, 40)
	m := r.model
	m.beginTurn("inspect settled details")
	m.appendThinking("retained thought")
	call := messages.ChatMessageToolCall{ID: "settled", Name: "bash"}
	tui := &gotuiTurnUI{model: m, config: r.config, repl: r}
	tui.AppendToolStart([]messages.ChatMessageToolCall{call})
	tui.AppendToolEnd(call, "captured result", time.Second, nil)
	tui.AppendAssistantText("finished")
	r.endTurn(nil)
	for _, tc := range []struct {
		open func() bool
		kind viewKind
	}{
		{func() bool { return m.toggleToolDisclosure(m.currentToolDisclosure().id) }, toolViewKind},
		{func() bool { return m.toggleReasoning(m.reasoningOrder[0], 140) }, thoughtViewKind},
	} {
		if !tc.open() {
			t.Fatal("settled disclosure did not expand")
		}
		r.render()
		found := false
		for _, link := range m.inspectionLinks {
			if link.kind == tc.kind {
				found = true
				r.handleEvent(ui.Event{Type: ui.MouseEvent, ID: "<MouseLeft>", Payload: ui.Mouse{X: link.rect.Min.X, Y: link.rect.Min.Y}})
				break
			}
		}
		if !found || !r.workspace().inspector.open || r.workspace().inspector.target.kind != tc.kind {
			t.Fatal("settled detail did not open inspector")
		}
		r.closeInspector()
	}
}

func TestWorkspaceMainProjectionUsesSharedCache(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "first", "second")
	tab := r.visibleTab()
	tab.model.appendLine("retained root content")
	tab.model.ed.setText("draft")
	tab.model.transcriptRows(80)
	r.showTab(0)
	r.work.wg.Wait()
	drainUITasks(r)
	if len(tab.model.visual.rows) != 0 || r.childViews.entries["main:"+tab.viewID()] == nil {
		t.Fatal("root projection escaped inactive cache budget")
	}
	r.showTab(1)
	if !tab.model.visual.valid || tab.model.ed.text() != "draft" {
		t.Fatal("root view or draft not restored")
	}
}

func TestWorkspaceNestedAgentUnderLeasedParentIsReadOnly(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	ctx := context.Background()
	parent := testAcquireSession(t, store, "external-parent")
	defer parent.Close()
	child, err := store.Acquire(ctx, "child", sessions.AcquireOptions{Parent: "external-parent"})
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	leaf, err := store.Acquire(ctx, "leaf", sessions.AcquireOptions{Parent: "child"})
	if err != nil {
		t.Fatal(err)
	}
	defer leaf.Close()
	r := newTabTestREPL(t, store, "local")
	r.opener.open = func(context.Context, string, Settings, bool) (*conversationState, error) {
		t.Error("inspection started runtime")
		return nil, fmt.Errorf("unexpected runtime")
	}
	r.model.mu.Lock()
	r.requestOpenLocked("leaf")
	r.model.mu.Unlock()
	select {
	case result := <-r.openDone:
		r.finishOpen(result)
	case <-time.After(5 * time.Second):
		t.Fatal("open timed out")
	}
	if r.visibleTab().name != "external-parent" || r.state.session != nil || r.workspace().inspector.target.session.Name != "leaf" {
		t.Fatal("nested ancestry did not select readonly root")
	}
	v := waitInspector(t, r, 140)
	if v.info.ParentID != child.(sessions.ViewIdentity).ViewID() {
		t.Fatal("nested parent identity missing")
	}
}

func TestViewSizeHandlesCyclicFormattingState(t *testing.T) {
	var builder strings.Builder
	builder.WriteString("formatting state") // strings.Builder retains a self pointer.
	m := newReplModel()
	m.inspections.tools = []inspectedTool{{result: messages.ChatMessage{Metadata: map[string]any{"format": &builder}}}}
	if size := childViewSize(m); size <= 0 || size > 1<<20 {
		t.Fatalf("invalid retained size: %d", size)
	}
}

func TestEscapeDefersToApprovalAndSearchWhileInspecting(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root")
	r.inspect(tabViewTarget(r.visibleTab()))
	waitInspector(t, r, 140)
	m := r.model
	approval := &approvalState{calls: []messages.ChatMessageToolCall{{ID: "1", Name: "bash"}}, reply: make(chan []bool, 1)}
	m.approval = approval
	m.renderInputForTerminal(6, 140)
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Escape>"})
	if !r.workspace().inspector.open {
		t.Fatal("escape closed the inspector instead of answering the approval")
	}
	select {
	case out := <-approval.reply:
		if len(out) != 1 || out[0] {
			t.Fatalf("escape did not deny the call: %v", out)
		}
	default:
		t.Fatal("escape left the approval pending")
	}
	m.hist.searching = true
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Escape>"})
	if !r.workspace().inspector.open || m.hist.searching {
		t.Fatal("escape closed the inspector instead of cancelling the search")
	}
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Escape>"})
	if r.workspace().inspector.open {
		t.Fatal("escape with nothing else pending did not close the inspector")
	}
}

func TestSavedConversationInspectorLinksNestedAgents(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "root")
	ctx := context.Background()
	child, err := store.Acquire(ctx, "child", sessions.AcquireOptions{Parent: "root"})
	if err != nil {
		t.Fatal(err)
	}
	call := agentCall("gcall", `{"label":"grandchild"}`)
	testAddMessages(t, child, []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "delegate"},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{call}},
		{Role: messages.MessageRoleTool, ToolCallID: call.ID, ToolName: call.Name, Content: "The grandchild answer."},
		{Role: messages.MessageRoleAssistant, Content: "Delegated."},
	})
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	grand, err := store.Acquire(ctx, "grandchild", sessions.AcquireOptions{Parent: "child"})
	if err != nil {
		t.Fatal(err)
	}
	if err := updateMetadata(ctx, grand, func(md *sessions.Metadata) { md.SpawnCallID = "gcall"; md.SpawnOutcome = sessions.ReportFinished }); err != nil {
		t.Fatal(err)
	}
	testAddMessages(t, grand, []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "deeper"}, {Role: messages.MessageRoleAssistant, Content: "The grandchild answer."}})
	if err := grand.Close(); err != nil {
		t.Fatal(err)
	}
	r.inspect(viewTarget{session: sessions.ViewTarget{Name: "child"}})
	v := waitInspector(t, r, 140)
	var row *toolDisclosureRow
	for _, record := range v.model.toolDisclosures.all() {
		for i := range record.rows {
			if record.rows[i].callID == "gcall" {
				row = &record.rows[i]
			}
		}
	}
	if row == nil || row.agent == nil || row.agent.session != "grandchild" || row.agent.viewID == "" {
		t.Fatalf("saved child view did not resolve its spawned agent: %+v", row)
	}
}

func TestInspectedLiveToolRecoversAfterReset(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root")
	m := r.model
	call := messages.ChatMessageToolCall{ID: "call_0", Name: "bash"}
	m.appendToolCallStart(call)
	m.inspections.setResult(call, messages.ChatMessage{Content: "first output"})
	r.inspectCommand("tools")
	v := waitInspector(t, r, 140)
	if !strings.Contains(inspectorText(v), "first output") {
		t.Fatalf("tool view = %q", inspectorText(v))
	}
	epoch := m.inspections.epoch
	m.mu.Lock()
	handled, quit := r.runCommand("/reset confirm")
	m.mu.Unlock()
	if !handled || quit {
		t.Fatalf("reset handled=%v quit=%v", handled, quit)
	}
	if m.inspections.epoch == epoch {
		t.Fatal("reset did not move the inspection epoch")
	}
	r.refreshInspector(140)
	settleInspectorWork(r)
	v = r.workspace().inspector.current
	if !v.unavailable || !strings.Contains(inspectorText(v), "no longer available") {
		t.Fatalf("wiped item still shows: %q", inspectorText(v))
	}
	m.appendToolCallStart(call)
	m.inspections.setResult(call, messages.ChatMessage{Content: "second output"})
	v = waitInspector(t, r, 140)
	if v.unavailable || !strings.Contains(inspectorText(v), "second output") {
		t.Fatalf("inspector did not recover the reused key: unavailable=%v %q", v.unavailable, inspectorText(v))
	}
}

func TestHydratedThoughtKeysFollowProseSplits(t *testing.T) {
	m := newReplModel()
	call := messages.ChatMessageToolCall{ID: "c", Name: "bash", Arguments: `{"command":"true"}`}
	m.hydrateHistory([]messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "one"},
		{Role: messages.MessageRoleAssistant, Reasoning: "thought A", Content: "prose one"},
		{Role: messages.MessageRoleUser, Content: "two"},
		{Role: messages.MessageRoleAssistant, Reasoning: "thought B", Content: "prose two", ToolCalls: []messages.ChatMessageToolCall{call}},
		{Role: messages.MessageRoleTool, ToolCallID: call.ID, ToolName: call.Name, Content: "ok"},
		{Role: messages.MessageRoleAssistant, Reasoning: "thought C", Content: "prose three"},
	}, "history")
	if len(m.inspections.thoughts) != 3 || len(m.reasoningOrder) != 3 {
		t.Fatalf("thoughts %d records %d, want 3 and 3", len(m.inspections.thoughts), len(m.reasoningOrder))
	}
	for i, want := range []string{"thought A", "thought B", "thought C"} {
		record := m.reasoningRecords.get(m.reasoningOrder[i])
		_, thought := m.inspections.selected(viewTarget{kind: thoughtViewKind, item: record.inspectionKey})
		if thought == nil || !strings.Contains(thought.text, want) {
			t.Fatalf("record %d opens %+v, want %q", i, thought, want)
		}
	}
	if last := m.reasoningRecords.get(m.reasoningOrder[2]); !last.complete || last.expanded {
		t.Fatalf("the turn's last reasoning record did not settle collapsed: %+v", last)
	}
}

func TestInspectorSwitchStartsAtTop(t *testing.T) {
	withDisplayTTY(t)
	r, screen := affordanceTestREPL(t)
	t.Cleanup(func() { _ = r.work.close() })
	screen.SetSize(140, 32)
	for _, name := range []string{"first", "second"} {
		call := messages.ChatMessageToolCall{ID: name, Name: name}
		r.model.appendToolCallStart(call)
		r.model.inspections.setResult(call, messages.ChatMessage{Content: strings.Repeat(name+" output\n", 100)})
	}
	r.inspectCommand("tools")
	checkTop := func() {
		t.Helper()
		waitInspector(t, r, 140)
		r.render()
		s := r.workspace().viewState(r.workspace().inspector.target)
		if s.top != 0 || s.follow || r.inspectorW.TopRow != 0 || r.inspectorW.PinBottom {
			t.Fatalf("item did not open at the top: %+v", s)
		}
		if len(r.inspectorW.OverlayBottom) != 0 {
			t.Fatal("initial content was marked as new output")
		}
	}
	checkTop()
	second := r.workspace().inspector.target
	r.inspectorScroll(15)
	r.render()
	if r.inspectorW.TopRow == 0 {
		t.Fatal("test item did not scroll")
	}
	r.inspectorSequence(-1)
	checkTop()
	r.inspectorAction("follow")
	r.render()
	r.inspectorSequence(1)
	checkTop()
	r.inspectorScroll(15)
	r.inspectorHistory(-1)
	checkTop()
	r.inspectorHistory(1)
	checkTop()
	r.inspectorSequence(-1)
	checkTop()
	r.inspect(second)
	checkTop()
	// Refreshing the same selection must retain a deliberate scroll position.
	r.inspectorScroll(15)
	r.render()
	top := r.inspectorW.TopRow
	r.inspect(second)
	waitInspector(t, r, 140)
	r.render()
	if r.inspectorW.TopRow != top {
		t.Fatal("refresh reset the current item's scroll position")
	}
}

func TestReplaceChildDisplayKeepsLaunchPromptIdentity(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root", "agent")
	tab := r.tabs[1]
	next := newReplModel()
	next.hydrateHistory([]messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "launch"}, {Role: messages.MessageRoleAssistant, Content: "reply"}}, "agent")
	r.replaceChildDisplay(tab, next)
	tab.model.appendUserPrompt("follow-up")
	flagged := 0
	for _, entry := range tab.model.transcript {
		if entry.initialPrompt {
			flagged++
		}
	}
	if flagged != 1 || tab.model.transcript[len(tab.model.transcript)-1].initialPrompt {
		t.Fatalf("follow-up after a display swap was flagged as the launch prompt (%d flagged)", flagged)
	}
}

func TestReinspectingResolvedTargetKeepsSelection(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "root")
	saved := testAcquireSession(t, store, "saved")
	testAddMessages(t, saved, []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "q"}, {Role: messages.MessageRoleAssistant, Content: "a"}})
	if err := saved.Close(); err != nil {
		t.Fatal(err)
	}
	byName := viewTarget{session: sessions.ViewTarget{Name: "saved"}}
	r.inspect(byName)
	waitInspector(t, r, 140)
	i := &r.workspace().inspector
	if i.target.session.ID == "" || i.target.key() == byName.key() {
		t.Fatalf("target was not resolved to its identity: %+v", i.target)
	}
	s := r.workspace().viewState(i.target)
	s.follow, s.top = false, 7
	r.inspect(byName)
	if s.top != 7 || s.follow || len(i.history) != 1 || i.generation != 1 {
		t.Fatalf("re-click through the name key reset the open view: top=%d follow=%v history=%d generation=%d", s.top, s.follow, len(i.history), i.generation)
	}
}

func TestParentActionClearsSearchAndWaitsForFirstRead(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root", "agent")
	child := r.tabs[1]
	r.showTab(0)
	call := messages.ChatMessageToolCall{ID: "c", Name: "bash"}
	child.model.appendToolCallStart(call)
	child.model.inspections.setResult(call, messages.ChatMessage{Content: "out"})
	target := tabViewTarget(child)
	target.kind, target.item = toolViewKind, child.model.inspections.tools[0].key
	r.inspect(target)
	waitInspector(t, r, 140)
	i := &r.workspace().inspector
	i.searching = true
	r.inspectorAction("parent")
	if !i.open || i.searching || i.target.kind != conversationViewKind {
		t.Fatalf("parent navigation left search open or went elsewhere: open=%v searching=%v kind=%v", i.open, i.searching, i.target.kind)
	}
	r.inspect(viewTarget{session: sessions.ViewTarget{Name: "someone-else"}})
	i.current = &viewInstance{target: i.target, view: viewFor(i.target.kind), loading: true}
	r.inspectorAction("parent")
	if !i.open {
		t.Fatal("parent click during the first read closed the inspector")
	}
}

func TestNewOutputBaselineIgnoresStaleModelWhileLoading(t *testing.T) {
	withDisplayTTY(t)
	r, screen := affordanceTestREPL(t)
	t.Cleanup(func() { _ = r.work.close() })
	screen.SetSize(140, 32)
	call := messages.ChatMessageToolCall{ID: "a", Name: "a"}
	r.model.appendToolCallStart(call)
	r.model.inspections.setResult(call, messages.ChatMessage{Content: strings.Repeat("out\n", 100)})
	r.inspectCommand("tools")
	v := waitInspector(t, r, 140)
	s := r.workspace().viewState(r.workspace().inspector.target)
	s.resetScroll()
	v.loading = true
	r.render()
	if s.lastRows != -1 || len(r.inspectorW.OverlayBottom) != 0 {
		t.Fatalf("stale model seeded the new-output baseline: lastRows=%d", s.lastRows)
	}
	v.loading = false
	r.render()
	if s.lastRows < 0 || len(r.inspectorW.OverlayBottom) != 0 {
		t.Fatalf("settled paint did not seed the baseline cleanly: lastRows=%d overlay=%d", s.lastRows, len(r.inspectorW.OverlayBottom))
	}
}

// A bash call's body shows the command it runs, exactly as the approval
// block did, rather than the JSON envelope around it.
func TestToolBodyShowsBashCommandNotJSON(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root")
	call := messages.ChatMessageToolCall{ID: "one", Name: "bash", Arguments: `{"command":"ls -la /tmp | head -3\n"}`}
	r.model.appendToolCallStart(call)
	r.model.inspections.setResult(call, messages.ChatMessage{Content: "alpha"})
	r.inspectCommand("tools")
	v := waitInspector(t, r, 140)
	text := inspectorText(v)
	if !strings.Contains(text, "╭─ command\n│ ls -la /tmp | head -3\n╭─ output · 1 line\n│ alpha") {
		t.Fatalf("bash body should fence the command verbatim: %s", text)
	}
	for _, stale := range []string{"arguments", `"command"`, "{", "}"} {
		if strings.Contains(text, stale) {
			t.Fatalf("bash body still shows its JSON envelope (%q): %s", stale, text)
		}
	}
	// A bash call without a usable command falls back to the JSON view.
	odd := messages.ChatMessageToolCall{ID: "two", Name: "bash", Arguments: `{"command":"  "}`}
	r.model.appendToolCallStart(odd)
	r.model.inspections.setResult(odd, messages.ChatMessage{Content: "beta"})
	r.inspectCommand("tools")
	v = waitInspector(t, r, 140)
	if text := inspectorText(v); !strings.Contains(text, "╭─ arguments · json\n│ {") {
		t.Fatalf("blank command should keep the JSON fence: %s", text)
	}
}

// The tool body is two titled payloads under one gutter, so raw output wraps
// like code and empty or pending output still shows its title.
func TestToolBodyFences(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root")
	call := messages.ChatMessageToolCall{ID: "one", Name: "fetch", Arguments: `{"url":"https://x"}`}
	r.model.appendToolCallStart(call)
	r.model.inspections.setResult(call, messages.ChatMessage{Content: "alpha\n" + strings.Repeat("wide ", 30) + "\ngamma"})
	r.inspectCommand("tools")
	v := waitInspector(t, r, 140)
	text := inspectorText(v)
	for _, want := range []string{"╭─ arguments · json\n│ {", "╭─ output · 3 lines\n│ alpha"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %s", want, text)
		}
	}
	for _, stale := range []string{"\nArguments\n", "\nOutput\n", "completed"} {
		if strings.Contains(text, stale) {
			t.Fatalf("stale label %q in %s", stale, text)
		}
	}
	rows := v.view.Rows(v.model, 40)
	continued := 0
	for _, row := range rows {
		if line := plainCells(row); strings.HasPrefix(line, "│ ") && strings.Contains(line, "wide") {
			continued++
		}
	}
	if continued < 2 {
		t.Fatalf("long raw output did not hard-wrap under the gutter: %d rows", continued)
	}
	pending := messages.ChatMessageToolCall{ID: "two", Name: "bash"}
	r.model.appendToolCallStart(pending)
	r.inspectCommand("tools")
	v = waitInspector(t, r, 140)
	if text := inspectorText(v); !strings.Contains(text, "╭─ arguments\n│ (none)") || !strings.Contains(text, "╭─ output\nRunning…") {
		t.Fatalf("pending tool body = %s", text)
	}
}

func TestInspectorDeclinesMessagesToASessionHeldElsewhere(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "root")
	held, err := store.Acquire(context.Background(), "held-agent", sessions.AcquireOptions{Parent: "root"})
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := held.AddMessage(context.Background(), messages.ChatMessage{Role: messages.MessageRoleUser, Content: "task"}); err != nil {
		t.Fatal(err)
	}
	r.inspect(viewTarget{session: sessions.ViewTarget{Name: "held-agent"}})
	v := waitInspector(t, r, 140)
	if v.info == nil || !v.info.InUse || len(r.tabs) != 1 {
		t.Fatalf("inspecting a held session: info=%#v tabs=%d", v.info, len(r.tabs))
	}
	header := plainStyledText(r.inspectorHeader(80, 3, 0, 0).text)
	if !strings.Contains(header, "open in another polly") {
		t.Fatalf("header = %q, want the held-elsewhere status", header)
	}
	r.messageInspectedAgent()
	r.applyTabRequests()
	if r.model.modal != nil {
		t.Fatalf("message editor opened for a held session: %q", r.model.modal.title)
	}
	if transcript := plainStyledText(r.model.fullTranscript()); !strings.Contains(transcript, "held-agent is open in another polly") {
		t.Fatalf("transcript = %q, want the held-elsewhere notice", transcript)
	}
	if len(r.tabs) != 1 {
		t.Fatal("declining a message changed the tabs")
	}

	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	r.inspectorRefreshAt = time.Time{}
	v = waitInspector(t, r, 140)
	if v.info == nil || v.info.InUse {
		t.Fatalf("view did not notice the released lease: %#v", v.info)
	}
	if header := plainStyledText(r.inspectorHeader(80, 3, 0, 0).text); strings.Contains(header, "open in another polly") {
		t.Fatalf("header = %q, still reports the released lease", header)
	}
	r.messageInspectedAgent()
	r.applyTabRequests()
	if r.model.modal == nil || r.model.modal.title != "Message held-agent" {
		t.Fatalf("message editor did not open once the lease ended: %#v", r.model.modal)
	}
}

func TestEntryMeasurementCountsTheCollapsedPromptRow(t *testing.T) {
	m := newReplModel()
	m.collapseInitialPrompt = true
	m.appendUserPrompt("launch task\nline two\nline three")
	m.appendLine("agent reply")
	for _, expanded := range []bool{false, true} {
		m.setInitialPromptExpanded(expanded)
		rows := m.transcriptRows(80)
		last := m.visual.blocks[len(m.visual.blocks)-1]
		wantStart := len(rows) - len(last.rows)
		if got := m.entryVisualStart(1, 80); got != wantStart {
			t.Fatalf("expanded=%v: entry 1 starts at %d, display shows it at %d", expanded, got, wantStart)
		}
		if got := m.entryVisualLineCount(0, 80); got != wantStart-m.mastheadRowCount(80) {
			t.Fatalf("expanded=%v: prompt entry measures %d rows, display uses %d", expanded, got, wantStart-m.mastheadRowCount(80))
		}
		if got := m.entryVisualLineCount(1, 80); got != len(last.rows) {
			t.Fatalf("expanded=%v: reply measures %d rows, display uses %d", expanded, got, len(last.rows))
		}
	}
}
