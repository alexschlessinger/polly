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

func TestInspectorToolResultPreservesComposerAndInlineSummary(t *testing.T) {
	withDisplayTTY(t)
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "root")
	m := r.model
	m.ed.setText("keep my draft")
	call := messages.ChatMessageToolCall{ID: "one", Name: "bash", Arguments: `{"command":"printf hi"}`}
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
	if !strings.Contains(inspectorText(v), "Arguments") || !strings.Contains(inspectorText(v), `"command": "printf hi"`) {
		t.Fatalf("tool arguments are not visible by default: %s", inspectorText(v))
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
	for _, record := range m.toolDisclosures {
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

func TestInspectorArrowNavigationFollowsMouse(t *testing.T) {
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
			r.inspectorBounds = image.Rect(70, 0, 140, 30)
			key := func(id string) { r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: id}) }
			move := func(x, y int) {
				e := convertTcellMouse(tcell.NewEventMouse(x, y, tcell.ButtonNone, tcell.ModNone))
				r.handleEvent(e)
				if r.wantsRenderForEvent(e) {
					t.Fatal("pointer motion requested a full repaint")
				}
			}
			last := r.workspace().inspector.target
			key("<Left>")
			if r.model.ed.cursor != 4 || r.workspace().inspector.target != last {
				t.Fatal("unknown pointer position stole an editor key")
			}
			key("<Right>")
			move(100, 10) // Hover, without clicking or changing editor focus.
			for _, step := range []struct {
				key   string
				index int
			}{{"<Left>", 2}, {"<Left>", 1}, {"<Left>", 1}, {"<Right>", 2}, {"<Right>", 3}, {"<Right>", 3}} {
				key(step.key)
				waitInspector(t, r, 140)
				index, total, _, _ := inspectorSequencePosition(&r.workspace().inspector)
				if index != step.index || total != 3 || r.model.ed.text() != "draft" || r.model.ed.cursor != 5 {
					t.Fatalf("%s: item %d/%d, draft %q at %d", step.key, index, total, r.model.ed.text(), r.model.ed.cursor)
				}
			}
			for _, mode := range []string{"result search", "history search", "dialog", "approval", "paste"} {
				switch mode {
				case "result search":
					r.workspace().inspector.searching = true
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
				if r.workspace().inspector.target != last {
					t.Fatalf("hover navigation stole %s input", mode)
				}
				r.workspace().inspector.searching = false
				r.model.hist.searching = false
				r.model.modal = nil
				r.model.approval = nil
				r.model.pasting = false
			}
			key("x")
			if r.model.ed.text() != "draftx" {
				t.Fatal("hover redirected typing away from the composer")
			}
			r.openModal(&replModal{inputMode: true})
			move(100, 30) // Composer starts at the inspector's bottom edge.
			r.closeModal()
			key("<Left>")
			if r.model.ed.cursor != 5 || r.workspace().inspector.target != last {
				t.Fatal("motion during a dialog left stale inspector hover")
			}
			move(100, 10)
			key(focusLostID)
			key(focusGainedID)
			key("<Left>")
			if r.model.ed.cursor != 4 || r.workspace().inspector.target != last {
				t.Fatal("focus loss retained stale inspector hover")
			}
			move(100, 10)
			r.closeInspector()
			key("<Right>")
			if r.model.ed.cursor != 5 {
				t.Fatal("closed inspector consumed arrow input")
			}
		})
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
	if r.workspace().inspector.current.geometry.width != 80 {
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
	for _, header := range m.visibleToolDisclosurePlacements(v) {
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
	for _, rec := range v.model.toolDisclosures {
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
	for _, rec := range v.model.toolDisclosures {
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
	r.images = &terminalImageManager{screen: screen, tty: tty, protocol: terminalImageKitty}
	t.Cleanup(func() { r.images.shutdown() })
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
		if r.inspectorBounds.Max.X != width {
			t.Fatal("inspector escapes pane")
		}
		if width < 120 && r.inspectorBounds.Min.X != 0 {
			t.Fatal("narrow inspector did not fill width")
		}
		if r.inspectorBounds.Dx() < 50 {
			t.Fatal("pane narrower than minimum")
		}
		if len(r.images.active) != 1 {
			t.Fatalf("image not placed at width %d: %#v", width, r.images.active)
		}
		for _, p := range r.workspace().inspector.current.model.imagePlacements {
			if !image.Rect(p.X, p.Y, p.X+p.Cols, p.Y+p.Rows).In(r.inspectorBounds) || !strings.HasPrefix(p.Key, "inspector:") {
				t.Fatalf("image outside inspector: %#v", p)
			}
		}
	}
	r.inspectorAction("maximize")
	waitInspector(t, r, 180)
	r.render()
	if r.inspectorBounds.Min.X != 0 {
		t.Fatal("maximize left root transcript visible")
	}
	r.closeInspector()
	r.render()
	if len(r.images.active) != 0 {
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
		overlay turnDockOverlay
		kind    viewKind
	}{{turnDockOverlayTools, toolViewKind}, {turnDockOverlayThought, thoughtViewKind}} {
		m.toggleLatestTurnTrailerOverlay(tc.overlay)
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
