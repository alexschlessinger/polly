package main

import (
	"context"
	"image"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	rw "github.com/mattn/go-runewidth"
	ui "github.com/metaspartan/gotui/v5"
)

func headerButton(buttons []inspectorButton, action string) image.Rectangle {
	for _, button := range buttons {
		if button.action == action {
			return button.rect
		}
	}
	return image.Rectangle{}
}

func checkInspectorHeaderGeometry(t *testing.T, header inspectorHeaderLayout, bounds image.Rectangle) {
	t.Helper()
	lines := strings.Split(header.text, "\n")
	for _, line := range lines {
		if width := styledTextWidth(line); width > bounds.Dx() {
			t.Fatalf("header row overflows: %d > %d: %q", width, bounds.Dx(), plainStyledText(line))
		}
	}
	for n, button := range header.buttons {
		if button.rect.Empty() || !button.rect.In(bounds) {
			t.Fatalf("button escapes header: %#v", button)
		}
		for _, other := range header.buttons[:n] {
			if button.rect.Overlaps(other.rect) {
				t.Fatalf("overlapping buttons: %#v and %#v", button, other)
			}
		}
		var label strings.Builder
		for _, cell := range ui.BuildCellWithXArray(parseStyledCells(lines[button.rect.Min.Y-bounds.Min.Y], ui.StyleClear)) {
			if bounds.Min.X+cell.X >= button.rect.Min.X && bounds.Min.X+cell.X < button.rect.Max.X {
				label.WriteRune(cell.Cell.Rune)
			}
		}
		if rw.StringWidth(label.String()) != button.rect.Dx() || strings.TrimSpace(label.String()) == "" {
			t.Fatalf("hitbox does not match visible text: %#v text=%q", button, label.String())
		}
	}
}

func TestInspectorHeaderAgentControlsReflectRuntime(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "root", "agent")
	root, child := r.tabs[0], r.tabs[1]
	child.parent, child.parentName, child.workspaceRoot = root, root.name, false
	r.showTab(0)
	r.inspect(tabViewTarget(child))
	waitInspector(t, r, 140)
	// The projection has no execution state; availability must use the owner.
	child.model.busy = true
	child.model.approval = &approvalState{}
	defer func() { child.model.busy, child.model.approval = false, nil }()
	header := r.inspectorHeader(50, 20, 90, 3)
	checkInspectorHeaderGeometry(t, header, image.Rect(90, 3, 140, 3+header.rows))
	for _, action := range []string{"stop", "review", "parent"} {
		if headerButton(header.buttons, action).Empty() {
			t.Fatalf("missing %s control: %s", action, plainStyledText(header.text))
		}
	}
	for _, action := range []string{"root", "back", "forward", "close", "find", "narrower", "wider", "message", "maximize", "prev", "next", "args", "raw"} {
		if !headerButton(header.buttons, action).Empty() {
			t.Fatalf("inapplicable action %s is clickable", action)
		}
	}
	if strings.Contains(plainStyledText(header.text), "[Prev]") {
		t.Fatal("conversation shows tool navigation")
	}
	child.model.busy, child.model.approval = false, nil
	header = r.inspectorHeader(50, 20, 90, 3)
	if strings.Contains(plainStyledText(header.text), "Stop agent") || strings.Contains(plainStyledText(header.text), "Review approval") {
		t.Fatal("inactive agent still shows stop/approval controls")
	}
	if header.rows != 1 {
		t.Fatal("inactive agent header retained an empty action row")
	}
	if strings.Contains(plainStyledText(header.text), "F6") {
		t.Fatal("header still advertises focus switching")
	}
	// A settled agent keeps how it ended, with the time it took.
	child.model.lastOutcome, child.model.lastElapsed = turnOutcomeDone, 41800*time.Millisecond
	defer func() { child.model.lastOutcome, child.model.lastElapsed = turnOutcomeNone, 0 }()
	header = r.inspectorHeader(50, 20, 90, 3)
	if rows := strings.Split(plainStyledText(header.text), "\n"); header.rows != 2 || rows[1] != "done · 41.8s" {
		t.Fatalf("settled agent header = %q", plainStyledText(header.text))
	}
	child.model.lastOutcome = turnOutcomeCanceled
	header = r.inspectorHeader(50, 20, 90, 3)
	if rows := strings.Split(plainStyledText(header.text), "\n"); header.rows != 2 || rows[1] != "canceled" {
		t.Fatalf("canceled agent header = %q", plainStyledText(header.text))
	}
}

func TestAgentsShortcutPreservesComposerAndInspector(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "root", "other")
	child, err := store.Acquire(context.Background(), "saved-agent", sessions.AcquireOptions{Parent: "root"})
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	r.showTab(0)
	r.inspect(tabViewTarget(r.visibleTab()))
	target := r.workspace().inspector.target
	r.model.ed.setText("keep this draft")
	r.model.ed.left()
	cursor := r.model.ed.cursor
	key := func(id string) {
		r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: id})
		r.applyTabRequests()
		settleInspectorWork(r)
	}
	for _, busy := range []bool{false, true} {
		r.model.busy = busy
		key("<C-g>")
		modal := r.model.modal
		if modal == nil || modal.title != "Sessions" {
			t.Fatalf("Ctrl-G did not open the sessions picker: %#v", modal)
		}
		nested := false
		for _, item := range modal.items {
			nested = nested || item.value == "saved-agent" && item.parent == "root"
		}
		if !nested {
			t.Fatalf("the picker does not nest the saved agent under its workspace: %#v", modal.items)
		}
		if len(r.tabs) != 2 || sessionInUse(t, store, "saved-agent") {
			t.Fatal("opening the Agents dialog activated an agent runtime")
		}
		key("<Escape>")
		if r.model.ed.text() != "keep this draft" || r.model.ed.cursor != cursor || !r.workspace().inspector.open || r.workspace().inspector.target != target {
			t.Fatal("opening or dismissing the Agents dialog changed the draft or inspector")
		}
	}
	r.model.busy = false
	r.model.hist.startSearch()
	key("<C-g>")
	if r.model.hist.searching || r.model.modal != nil {
		t.Fatal("Ctrl-G did not retain its history-search cancellation behavior")
	}
	r.workspace().inspector.searching = true
	key("<C-g>")
	if r.model.modal != nil || !r.workspace().inspector.searching {
		t.Fatal("Ctrl-G stole input from inspector search")
	}
}

func TestInspectorHeaderAgentTitleSurvivesRuntimeRetirement(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	ctx := context.Background()
	const title = "Tui refactor and code preview"
	for _, seed := range []struct{ name, parent, description string }{
		{"sly-hare", "", "Root description"},
		{"merry-panda", "sly-hare", title},
	} {
		session, err := store.Acquire(ctx, seed.name, sessions.AcquireOptions{Parent: seed.parent})
		if err != nil {
			t.Fatal(err)
		}
		md, err := session.GetMetadata(ctx)
		if err != nil {
			t.Fatal(err)
		}
		md.Description = seed.description
		if err := session.SetMetadata(ctx, md); err != nil {
			t.Fatal(err)
		}
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
	}
	r := newTabTestREPL(t, store, "sly-hare", "merry-panda")
	child := r.tabs[1]
	child.keepOpen = true
	r.showTab(0)
	target := tabViewTarget(child)
	r.inspect(target)
	waitInspector(t, r, 240)
	checkTitle := func(want string) {
		t.Helper()
		header := r.inspectorHeader(120, 20, 120, 0)
		line := strings.Split(plainStyledText(header.text), "\n")[0]
		if line != "‹ "+want || strings.Contains(line, "sly-hare") {
			t.Fatalf("unexpected agent heading: %q", line)
		}
		checkInspectorHeaderGeometry(t, header, image.Rect(120, 0, 240, header.rows))
	}
	checkTitle("merry-panda · " + title)
	child.model.mu.Lock()
	child.model.status.description = " \n "
	child.model.mu.Unlock()
	waitInspector(t, r, 240)
	checkTitle("merry-panda")
	// Retiring execution must retain the title from saved session metadata.
	child.keepOpen = false
	if !r.closeSpentChild(child) {
		t.Fatal("agent runtime did not retire")
	}
	r.inspectorRefreshAt = time.Time{}
	waitInspector(t, r, 240)
	checkTitle("merry-panda · " + title)
	if len(r.tabs) != 1 || r.inspectionTab(target) != nil {
		t.Fatal("showing a saved title activated the agent runtime")
	}
}

func TestInspectorHeaderWrappingWithoutParentBreadcrumb(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "root")
	for _, name := range []string{"bash", "spawn_agent", "read_file"} {
		call := messages.ChatMessageToolCall{ID: name, Name: name}
		r.model.appendToolCallStart(call)
		r.model.inspections.setResult(call, messages.ChatMessage{Content: "done"})
	}
	r.inspectCommand("tools")
	waitInspector(t, r, 140)
	r.inspectorSequence(-1)
	v := waitInspector(t, r, 140)
	i := &r.workspace().inspector
	v.info.Metadata.Name = "界界-root-with-a-long-name"
	for _, width := range []int{50, 60, 80, 160} {
		header := r.inspectorHeader(width, 20, 71, 3)
		checkInspectorHeaderGeometry(t, header, image.Rect(71, 3, 71+width, 3+header.rows))
		for _, action := range []string{"agent", "parent"} {
			if headerButton(header.buttons, action).Empty() {
				t.Fatalf("width %d lost %s: %s", width, action, plainStyledText(header.text))
			}
		}
		if title := strings.Split(plainStyledText(header.text), "\n")[0]; title != "‹ spawn_agent · 2/3" {
			t.Fatalf("expected only the tool and position: %q", title)
		}
		for _, action := range []string{"back", "forward", "find", "narrower", "wider", "message", "maximize", "prev", "next", "args", "raw"} {
			if !headerButton(header.buttons, action).Empty() {
				t.Fatalf("removed control %s is still clickable", action)
			}
		}
		if !headerButton(header.buttons, "close").Empty() || !headerButton(header.buttons, "root").Empty() {
			t.Fatal("removed controls retain a click target")
		}
	}
	header := r.inspectorHeader(50, 1, 71, 3)
	checkInspectorHeaderGeometry(t, header, image.Rect(71, 3, 121, 4))
	if !headerButton(header.buttons, "args").Empty() {
		t.Fatal("clipped header row is still clickable")
	}
	// Clicking the title performs the same parent navigation as the arrow.
	r.inspectorButtons = header.buttons
	r.chrome.inner = image.Rect(71, 3, 121, 20)
	r.inspectorHeaderRows = header.rows
	parent := headerButton(header.buttons, "parent")
	if parent.Min != image.Pt(71, 3) || parent.Dy() != 1 || parent.Dx() != rw.StringWidth("‹ spawn_agent · 2/3") {
		t.Fatalf("parent control does not span the arrow and title: %v", parent)
	}
	r.handleEvent(ui.Event{Type: ui.MouseEvent, ID: "<MouseLeft>", Payload: ui.Mouse{X: 73, Y: 3}})
	if i.open || r.visibleTab().name != "root" {
		t.Fatal("title did not close inspector for a main-session item")
	}
}

func TestInspectorHeaderArrowReturnsToOwnerOutsideMainSession(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root", "agent")
	child := r.tabs[1]
	child.parent, child.parentName = r.tabs[0], r.tabs[0].name
	r.showTab(0)
	call := messages.ChatMessageToolCall{ID: "read", Name: "read_file"}
	child.model.appendToolCallStart(call)
	child.model.inspections.setResult(call, messages.ChatMessage{Content: "result"})
	child.model.appendThinking("agent thought")
	for _, kind := range []viewKind{toolViewKind, thoughtViewKind} {
		target := tabViewTarget(child)
		target.kind = kind
		if kind == toolViewKind {
			target.item = child.model.inspections.tools[0].key
		} else {
			target.item = child.model.inspections.thoughts[0].key
		}
		r.inspect(target)
		waitInspector(t, r, 140)
		for _, width := range []int{24, 50, 80} {
			header := r.inspectorHeader(width, 20, 71, 3)
			checkInspectorHeaderGeometry(t, header, image.Rect(71, 3, 71+width, 3+header.rows))
			title := strings.Split(plainStyledText(header.text), "\n")[0]
			if !strings.HasPrefix(title, "‹ ") || strings.Contains(title, "agent") || strings.Contains(title, "root") || strings.Contains(title, "›") || headerButton(header.buttons, "parent").Min != image.Pt(71, 3) {
				t.Fatalf("expected leading arrow without parent breadcrumb: %q", title)
			}
		}
		header := r.inspectorHeader(50, 20, 71, 3)
		r.inspectorButtons, r.inspectorHeaderRows = header.buttons, header.rows
		r.chrome.inner = image.Rect(71, 3, 121, 23)
		r.handleEvent(ui.Event{Type: ui.MouseEvent, ID: "<MouseLeft>", Payload: ui.Mouse{X: 73, Y: 3}})
		i := &r.workspace().inspector
		if !i.open || i.target.kind != conversationViewKind || i.target.session.ID != child.viewID() {
			t.Fatal("item title did not return to the owning agent's conversation")
		}
		waitInspector(t, r, 140)
		header = r.inspectorHeader(50, 20, 71, 3)
		r.inspectorButtons, r.inspectorHeaderRows = header.buttons, header.rows
		r.handleEvent(ui.Event{Type: ui.MouseEvent, ID: "<MouseLeft>", Payload: ui.Mouse{X: 73, Y: 3}})
		if i.open {
			t.Fatal("agent title did not close inspector when returning to main session")
		}
	}
}

func TestInspectorHeaderSearchReplacesActionsAndOwnsInput(t *testing.T) {
	withDisplayTTY(t)
	r, screen := affordanceTestREPL(t)
	t.Cleanup(func() { _ = r.work.close() })
	screen.SetSize(120, 32)
	r.model.ed.setText("main draft")
	call := messages.ChatMessageToolCall{ID: "one", Name: "bash"}
	r.model.appendToolCallStart(call)
	r.model.inspections.setResult(call, messages.ChatMessage{Content: strings.Repeat("line\n", 100)})
	r.inspectCommand("tools")
	waitInspector(t, r, 120)
	r.render()
	r.inspectCommand("find")
	r.render()
	if !headerButton(r.inspectorButtons, "args").Empty() || !headerButton(r.inspectorButtons, "raw").Empty() {
		t.Fatal("search left hidden controls clickable")
	}
	if _, _, visible := screen.GetCursor(); visible {
		t.Fatal("main composer cursor shown while typing into search")
	}
	s := r.workspace().viewState(r.workspace().inspector.target)
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "line"})
	if r.model.ed.text() != "main draft" || r.workspace().inspector.searchInput.text() != "line" {
		t.Fatal("search input or old action click changed the underlying view/composer")
	}
	r.render()
	if !strings.Contains(plainStyledText(r.inspectorHeaderW.Text), "Find: line▏") {
		t.Fatalf("search query is not visible: %s", plainStyledText(r.inspectorHeaderW.Text))
	}
	r.workspace().inspector.searchInput.setText(strings.Repeat("界", 100) + "needle")
	r.render()
	searchHeader := plainStyledText(r.inspectorHeaderW.Text)
	if !strings.Contains(searchHeader, "Find: …") || !strings.Contains(searchHeader, "needle▏") {
		t.Fatalf("long search query lost its visible tail: %s", searchHeader)
	}
	if r.inspectorW.Inner.Min.Y != r.chrome.inner.Min.Y+r.inspectorHeaderRows {
		t.Fatal("body origin disagrees with header height")
	}
	r.inspectorAction("follow")
	r.inspectorScroll(-3)
	wantTop := len(r.workspace().inspector.current.model.visual.rows) - (r.chrome.inner.Dy() - r.inspectorHeaderRows) - 3
	if s.top != wantTop {
		t.Fatalf("scroll used wrong header height: %d != %d", s.top, wantTop)
	}
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Escape>"})
	r.render()
	if !r.workspace().inspector.open || r.workspace().inspector.searching {
		t.Fatal("Escape did not restore inspector actions")
	}
}

func TestInspectorCommandResizeFollowsDisplayedPane(t *testing.T) {
	r, screen := affordanceTestREPL(t)
	t.Cleanup(func() { _ = r.work.close() })
	r.inspect(tabViewTarget(r.visibleTab()))
	screen.SetSize(400, 32)
	// A dragged divider can be beyond the old keyboard ratio limits.
	r.inspectorRatio = .9
	before := r.inspectorGeometry(400).width
	r.inspectorAction("wider")
	if after := r.inspectorGeometry(400).width; after <= before {
		t.Fatalf("wider moved in the wrong direction: %d -> %d", before, after)
	}
	for range 30 {
		r.inspectorAction("narrower")
	}
	if width := r.inspectorGeometry(400).width; width != 50 {
		t.Fatalf("minimum inspector width = %d", width)
	}
	for range 30 {
		r.inspectorAction("wider")
	}
	// The conversation keeps its readable minimum and the frame its two
	// border columns.
	if width := r.inspectorGeometry(400).width; width != 348 {
		t.Fatalf("maximum inspector width = %d", width)
	}
	r.inspectorAction("maximize")
	ratio := r.inspectorRatio
	r.inspectorAction("narrower")
	header := r.inspectorHeader(400, 20, 0, 0)
	if r.inspectorRatio != ratio || !headerButton(header.buttons, "wider").Empty() || !headerButton(header.buttons, "maximize").Empty() || !strings.HasPrefix(strings.Split(plainStyledText(header.text), "\n")[0], "‹ ") {
		t.Fatal("maximized inspector exposes ineffective width controls")
	}
}

func mouseEvent(id string, p image.Point) ui.Event {
	return ui.Event{Type: ui.MouseEvent, ID: id, Payload: ui.Mouse{X: p.X, Y: p.Y}}
}

// A tool header is two rows: the title, then its state and actions with no
// brackets. A launch tool's second row links to the agent; a thought has no
// second row at all.
func TestInspectorHeaderTwoRows(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root")
	call := messages.ChatMessageToolCall{ID: "launch", Name: "spawn_agent"}
	r.model.appendToolCallStart(call)
	r.model.inspections.setResult(call, messages.ChatMessage{Content: "done"})
	r.model.appendThinking("a thought")
	r.inspectCommand("tools")
	waitInspector(t, r, 140)
	header := r.inspectorHeader(60, 20, 71, 3)
	checkInspectorHeaderGeometry(t, header, image.Rect(71, 3, 131, 3+header.rows))
	rows := strings.Split(plainStyledText(header.text), "\n")
	if len(rows) != 2 || rows[0] != "‹ spawn_agent · 1/1" || !strings.HasPrefix(rows[1], "completed") || !strings.HasSuffix(rows[1], "Open agent") {
		t.Fatalf("tool header rows = %q", rows)
	}
	if strings.ContainsAny(plainStyledText(header.text), "[]") {
		t.Fatal("header actions are bracketed")
	}
	agent := headerButton(header.buttons, "agent")
	if agent.Empty() || agent.Min.Y != 4 || agent.Dx() != rw.StringWidth("Open agent") {
		t.Fatalf("Open agent link hitbox = %v", agent)
	}
	r.inspectCommand("thoughts")
	waitInspector(t, r, 140)
	header = r.inspectorHeader(60, 20, 71, 3)
	if header.rows != 1 || plainStyledText(header.text) != "‹ Thought · 1/1" {
		t.Fatalf("thought header = %q rows=%d", plainStyledText(header.text), header.rows)
	}
}
