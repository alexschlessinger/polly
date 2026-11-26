package main

import (
	"image"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/headlessscreen"
	"github.com/alexschlessinger/pollytool/messages"
	ui "github.com/metaspartan/gotui/v5"
)

// refreshContextPopover brings the open context window's dialog up to now,
// as a frame does, with no image protocol to draw its chart.
func (r *managedREPL) refreshContextPopover(now time.Time) {
	if p := r.panes.floatOf(floatContext); p != nil {
		r.refreshContext(p.panel.modal, now, false)
	}
}

// panelPage is the page of the window polly draws for kind, as last painted.
func panelPage(r *managedREPL, kind floatKind) *modalParagraph {
	if p := r.panes.floatOf(kind); p != nil && p.own {
		return p.panel.page
	}
	return newModalParagraph() // No such window: an empty page fails the caller's check.
}

// listLabels is the rows of a list polly drew, one a line.
func listLabels(m *replModal) string {
	var rows []string
	for _, item := range m.items {
		rows = append(rows, item.label)
	}
	return strings.Join(rows, "\n")
}

// openHelpPage opens the help page the way a user does: /help lists the
// commands, and the list's last row is the page.
func openHelpPage(r *managedREPL) {
	r.runCommand("/help")
	for _, id := range []string{"<End>", "<Enter>"} {
		r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: id})
	}
}

func ownKey(r *managedREPL, id string) {
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: id})
}

func TestPanelsStandBesideTheConversationOnPollysOwnScreen(t *testing.T) {
	withDisplayTTY(t)
	r, screen := affordanceTestREPL(t)
	screen.SetSize(140, 40)
	store := testOpenMemoryStore(t, nil)
	r.state = &conversationState{session: testAcquireSession(t, store, "ctx"), settings: Settings{MaxHistoryTokens: 256_000}}
	plan := progressFixture()
	r.model.setPlanProgress(plan, plan.Todos)
	r.model.ed.setText("half a thought")

	// Both are up at once, each in a window of its own, and neither covers
	// the other or the composer.
	r.model.mu.Lock()
	r.openTodoPanel()
	r.openContextPopover()
	r.model.mu.Unlock()
	r.render()
	todo, usage := r.panes.floatOf(floatChecklist), r.panes.floatOf(floatContext)
	if todo == nil || usage == nil || !todo.own || !usage.own {
		t.Fatalf("the panels are not windows of polly's own screen: %v %v", todo, usage)
	}
	rows := r.frameLayoutFor(140, 40).transcriptHeight
	if todo.rect.Empty() || usage.rect.Empty() || todo.rect.Overlaps(usage.rect) || todo.rect.Min.Y != 0 || usage.rect.Max.Y != rows || todo.rect.Max.X != 140 || usage.rect.Max.X != 140 {
		t.Fatalf("the windows are at %v and %v, want a column at the right of %d rows", todo.rect, usage.rect, rows)
	}
	if todo.rect.Min.X != usage.rect.Min.X {
		t.Fatalf("the windows' left edges do not line up: %v and %v", todo.rect, usage.rect)
	}
	if text := screenText(t, screen); !strings.Contains(text, "Rank exact matches") || !strings.Contains(text, "Session usage") {
		t.Fatalf("the screen does not show both panels:\n%s", text)
	}

	// The one opened last has the keys, typing gives them back to the
	// composer with both still up, and a click gives them to the other.
	if r.focusedOwnWindow() != usage || r.model.modal != nil {
		t.Fatal("the window opened last did not take the keys")
	}
	ownKey(r, "!")
	if r.focusedOwnWindow() != nil || r.model.ed.text() != "half a thought!" || len(r.panes.floats) != 2 {
		t.Fatalf("typing beside the windows left the draft at %q with %d windows", r.model.ed.text(), len(r.panes.floats))
	}
	r.handleEvent(mouseEvent("<MouseLeft>", todo.rect.Min.Add(image.Pt(2, 1))))
	if r.focusedOwnWindow() != todo {
		t.Fatal("a click on a window did not give it the keys")
	}

	// Escape closes the window with the keys, and the mark on the other's
	// border closes that one, without the keys ever leaving the composer.
	ownKey(r, "<Escape>")
	if r.panes.floatOf(floatChecklist) != nil || r.panes.floatOf(floatContext) != usage || r.focusedOwnWindow() != nil {
		t.Fatal("Escape did not close just the window that had the keys")
	}
	r.render()
	reading := usage.panel.modal.context.ctx
	r.handleEvent(mouseEvent("<MouseLeft>", usage.closeAt.Min))
	if len(r.panes.floats) != 0 || r.focusedOwnWindow() != nil || reading.Err() == nil {
		t.Fatal("the mark on the window's border did not close it")
	}
	if r.model.ed.text() != "half a thought!" {
		t.Fatalf("the windows changed the draft to %q", r.model.ed.text())
	}
}

func TestTheViewFloatsOnPollysOwnScreenUntilDocked(t *testing.T) {
	withDisplayTTY(t)
	r, screen := chromeTestREPL(t)
	screen.SetSize(160, 40)
	t.Cleanup(func() { _ = saveDockedPanels(nil) })
	r.panes.docks = loadDockedPanels()
	plan := progressFixture()
	r.model.setPlanProgress(plan, plan.Todos)
	r.model.appendLine("a conversation as wide as the terminal")
	r.inspect(tabViewTarget(r.visibleTab()))
	waitInspector(t, r, 160)
	r.render()

	// The view stands over the conversation, which keeps its whole width.
	g := r.chrome
	if !g.floating || g.main.Dx() != 160 || g.frame.Max.X != 160 || g.frame.Dx() < inspectorMinWidth || g.frame.Dx() > 100 {
		t.Fatalf("the view is framed at %v beside a conversation of %v, want a window over it", g.frame, g.main)
	}
	if r.surface.body.Inner.Dx() != r.inspectorGeometry(160).width {
		t.Fatal("the view is wrapped for another width than its window's")
	}
	frame := screenSnapshot(t, screen)
	if screenGlyph(frame, g.frame.Min) != "╭" || !strings.Contains(screenRow(frame, g.frame.Min.Y), pinDockMark) {
		t.Fatalf("the window's border is missing, or its word to dock it: %q", screenRow(frame, g.frame.Min.Y))
	}

	// The checklist takes the top of the same column, and the view the rest.
	r.model.mu.Lock()
	r.openTodoPanel()
	r.model.mu.Unlock()
	r.render()
	todo := r.panes.floatOf(floatChecklist)
	if g = r.chrome; todo.rect.Empty() || todo.rect.Overlaps(g.frame) || g.frame.Min.Y != todo.rect.Max.Y || todo.rect.Min.X != g.frame.Min.X {
		t.Fatalf("the checklist at %v and the view at %v do not share the column, edge to edge", todo.rect, g.frame)
	}

	// Its word docks it beside the conversation, for later launches too, and
	// the word it then carries floats it again.
	r.handleEvent(mouseEvent("<MouseLeft>", g.pin.Min))
	r.render()
	g = r.chrome
	if g.floating || g.main.Dx() >= 160 || g.frame.Intersect(g.main).Dx() > 0 || !loadDockedPanels()[floatView] {
		t.Fatalf("docking left the view at %v over a conversation of %v (saved: %v)", g.frame, g.main, loadDockedPanels())
	}
	if todo.rect.Max.X != g.main.Max.X {
		t.Fatalf("the checklist at %v is not over the conversation beside the docked view (%v)", todo.rect, g.main)
	}
	if !strings.Contains(screenRow(screenSnapshot(t, screen), g.frame.Min.Y), pinFloatMark) {
		t.Fatal("the docked view has no word to float it")
	}
	r.handleEvent(mouseEvent("<MouseLeft>", g.pin.Min))
	r.render()
	if !r.chrome.floating || loadDockedPanels()[floatView] {
		t.Fatal("floating the view again did not take, or was not remembered")
	}

	// A terminal too narrow for both gives the view all of it, as before.
	screen.SetSize(100, 40)
	waitInspector(t, r, 100)
	r.render()
	if g = r.chrome; g.floating || !g.main.Empty() || !g.pin.Empty() {
		t.Fatalf("a narrow terminal framed the view at %v beside %v", g.frame, g.main)
	}
}

func TestQuestionsAreAskedOnPollysOwnScreen(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	r.model.ed.setText("")

	// /help lists the commands, typing filters them, and the one chosen is
	// typed for the user: this one waits for its argument.
	r.runCommand("/help")
	list := r.model.modal
	if list == nil || list.title != "Polly commands" || !strings.Contains(listLabels(list), "/rename <name>") {
		t.Fatalf("/help did not list the commands: %v", list)
	}
	for _, ch := range "rename" {
		ownKey(r, string(ch))
	}
	if rows := list.filteredItems(); len(rows) != 1 || rows[0].value != "/rename" {
		t.Fatalf("typing did not filter the list down to /rename: %v", rows)
	}
	ownKey(r, "<Enter>")
	if r.model.modal != nil || r.model.ed.text() != "/rename " {
		t.Fatalf("choosing /rename left the composer at %q", r.model.ed.text())
	}

	// Escape declines, and nothing is typed.
	r.model.ed.setText("")
	r.runCommand("/help")
	ownKey(r, "<Escape>")
	if r.model.modal != nil || r.model.ed.text() != "" {
		t.Fatal("Escape did not decline the list")
	}

	// /title asks for the title on a line that starts from the one it has.
	r.model.status.title = "Before"
	r.runCommand("/title")
	field := r.model.modal
	if field == nil || !field.inputMode || field.title != "Session title" || field.input.text() != "Before" {
		t.Fatalf("/title did not ask for the title: %v", field)
	}
	ownKey(r, "<Escape>")
	if r.model.modal != nil {
		t.Fatal("Escape did not decline the title")
	}
}

func TestPanelsDockBesideTheConversationOnPollysOwnScreen(t *testing.T) {
	withDisplayTTY(t)
	r, screen := chromeTestREPL(t)
	screen.SetSize(160, 40)
	t.Cleanup(func() { _ = saveDockedPanels(nil) })
	r.panes.docks = loadDockedPanels()
	store := testOpenMemoryStore(t, nil)
	r.state = &conversationState{session: testAcquireSession(t, store, "ctx"), settings: Settings{MaxHistoryTokens: 256_000}}
	plan := progressFixture()
	r.model.setPlanProgress(plan, plan.Todos)
	r.model.mu.Lock()
	r.openTodoPanel()
	r.openContextPopover()
	r.model.mu.Unlock()
	r.render()
	todo, usage := r.panes.floatOf(floatChecklist), r.panes.floatOf(floatContext)
	rows := r.frameLayoutFor(160, 40).transcriptHeight
	click := func(at image.Rectangle) {
		t.Helper()
		if at.Empty() {
			t.Fatal("the window has no word to dock or float it")
		}
		r.handleEvent(mouseEvent("<MouseLeft>", at.Min))
		r.render()
	}

	// Floating, both carry the word that docks them, over a conversation as
	// wide as the terminal.
	if r.chrome.main.Dx() != 160 || !strings.Contains(screenRow(screenSnapshot(t, screen), todo.rect.Min.Y), pinDockMark) {
		t.Fatalf("floating windows narrowed the conversation to %v, or carry no word to dock them", r.chrome.main)
	}

	// Docked, the checklist is a pane beside the conversation, as tall as
	// the column, and the popover floats over what is left of it.
	click(todo.pinAt)
	main := r.chrome.main
	if !loadDockedPanels()[floatChecklist] || main.Dx() != 160-todo.rect.Dx() || todo.rect.Min.X != main.Max.X || todo.rect.Min.Y != 0 || todo.rect.Max.Y != rows {
		t.Fatalf("the docked checklist is at %v beside a conversation of %v (saved: %v)", todo.rect, main, loadDockedPanels())
	}
	if usage.rect.Max.X != main.Max.X || usage.rect.Overlaps(todo.rect) {
		t.Fatalf("the popover at %v does not float over the conversation beside the docked checklist at %v", usage.rect, todo.rect)
	}
	if r.focusedOwnWindow() != usage {
		t.Fatal("docking a window moved the keys from the one that had them")
	}

	// The popover docks under it and takes the rest of the column.
	click(usage.pinAt)
	if !loadDockedPanels()[floatContext] || usage.rect.Min.X != todo.rect.Min.X || usage.rect.Min.Y != todo.rect.Max.Y || usage.rect.Max.Y != rows || r.chrome.main.Max.X != todo.rect.Min.X {
		t.Fatalf("docked, the checklist is at %v and the popover at %v", todo.rect, usage.rect)
	}

	// A docked view takes the middle of that column, between the two.
	r.model.mu.Lock()
	r.toggleDock(floatView)
	r.model.mu.Unlock()
	r.inspect(tabViewTarget(r.visibleTab()))
	waitInspector(t, r, 160)
	r.render()
	g := r.chrome
	if g.floating || g.frame.Min.X != todo.rect.Min.X || g.frame.Min.Y != todo.rect.Max.Y || g.frame.Max.Y != usage.rect.Min.Y || usage.rect.Min.X != g.frame.Min.X || g.main.Max.X != g.frame.Min.X {
		t.Fatalf("docked together: checklist %v, view %v, popover %v, conversation %v", todo.rect, g.frame, usage.rect, g.main)
	}

	// The word a docked pane carries floats it again, and that is saved too.
	click(todo.pinAt)
	if loadDockedPanels()[floatChecklist] || todo.rect.Max.X != r.chrome.main.Max.X || r.chrome.frame.Min.Y != 0 {
		t.Fatalf("floated again, the checklist is at %v beside a view at %v (saved: %v)", todo.rect, r.chrome.frame, loadDockedPanels())
	}

	// A terminal too narrow for two columns floats everything, with no word
	// to dock it.
	screen.SetSize(100, 40)
	waitInspector(t, r, 100)
	r.render()
	if !usage.pinAt.Empty() || !todo.pinAt.Empty() || usage.rect.Max.X != 100 {
		t.Fatalf("a narrow terminal kept a docked column: popover at %v", usage.rect)
	}
}

// ownPanels is a REPL on a width x height screen with the checklist, the
// inspected view and the context popover all open on polly's own screen, the
// view floating, and the keys in the composer.
func ownPanels(t *testing.T, width, height int) (r *managedREPL, screen *headlessscreen.Screen, todo, usage *satellitePane) {
	t.Helper()
	withDisplayTTY(t)
	r, screen = chromeTestREPL(t)
	screen.SetSize(width, height)
	t.Cleanup(func() { _ = saveDockedPanels(nil) })
	r.panes.docks = loadDockedPanels()
	store := testOpenMemoryStore(t, nil)
	r.state = &conversationState{session: testAcquireSession(t, store, "ctx"), settings: Settings{MaxHistoryTokens: 256_000}}
	plan := progressFixture()
	r.model.setPlanProgress(plan, plan.Todos)
	r.model.ed.setText("half a thought")
	r.inspect(tabViewTarget(r.visibleTab()))
	waitInspector(t, r, width)
	r.model.mu.Lock()
	r.openTodoPanel()
	r.openContextPopover()
	r.yieldPanelKeys()
	r.model.mu.Unlock()
	r.render()
	todo, usage = r.panes.floatOf(floatChecklist), r.panes.floatOf(floatContext)
	if todo == nil || usage == nil || !todo.own || !usage.own || !r.framed() {
		t.Fatal("the three panels are not all on polly's own screen")
	}
	return r, screen, todo, usage
}

// keysAt names what has the keys on polly's own screen.
func keysAt(r *managedREPL) string {
	switch p := r.focusedOwnWindow(); {
	case p != nil && r.inspectorFocused():
		return "two at once"
	case p != nil:
		return panelSpecs[p.panel.kind].name
	case r.inspectorFocused():
		return "agents"
	}
	return "composer"
}

func TestTabGoesRoundTheWindowsOfPollysOwnScreen(t *testing.T) {
	r, _, todo, usage := ownPanels(t, 160, 40)

	// From the composer down the column and back: a window that has an action
	// to choose has Tab choose it first, and the next Tab moves on.
	var round []string
	for range 8 {
		ownKey(r, "<Tab>")
		r.render()
		if at := keysAt(r); len(round) == 0 || round[len(round)-1] != at {
			round = append(round, at)
		}
		if round[len(round)-1] == "composer" {
			break
		}
	}
	if got := strings.Join(round, " "); got != "todo agents context composer" {
		t.Fatalf("Tab took the keys to: %s", got)
	}
	if todo.panel.modal.selectedValue() != planViewAction {
		t.Fatal("Tab passed the checklist by without choosing its action")
	}
	if r.model.ed.text() != "half a thought" || len(r.panes.floats) != 2 || !r.workspace().inspector.open {
		t.Fatalf("going round changed the draft to %q, or closed a window", r.model.ed.text())
	}

	// A window alone keeps Tab for its own action, as it always has.
	r.model.mu.Lock()
	r.closeInspector()
	r.endFloat(todo, true)
	r.model.mu.Unlock()
	r.render()
	for press := range 4 {
		ownKey(r, "<Tab>")
		if keysAt(r) != "context" || r.panes.floatOf(floatContext) != usage {
			t.Fatalf("Tab %d beside the only window took the keys to the %s", press+1, keysAt(r))
		}
	}

	// A command being typed keeps Tab to complete it; any other draft lets
	// Tab go to the window.
	ownKey(r, "!")
	r.model.ed.setText("/mod")
	ownKey(r, "<Tab>")
	if keysAt(r) != "composer" || !strings.HasPrefix(r.model.ed.text(), "/mod") {
		t.Fatalf("Tab on a command left %q with the keys at the %s", r.model.ed.text(), keysAt(r))
	}
	r.model.ed.setText("a thought")
	ownKey(r, "<Tab>")
	if keysAt(r) != "context" || r.model.ed.text() != "a thought" {
		t.Fatalf("Tab beside a window left %q with the keys at the %s", r.model.ed.text(), keysAt(r))
	}
}

func TestAltPDocksTheWindowWithTheKeys(t *testing.T) {
	r, _, todo, _ := ownPanels(t, 160, 40)
	press := func(id string) {
		ownKey(r, id)
		r.render()
	}

	// The checklist has the keys: Alt-P docks it, and again floats it.
	press("<Tab>")
	press("<M-p>")
	if !loadDockedPanels()[floatChecklist] || !todo.docked || r.chrome.main.Max.X != todo.rect.Min.X || keysAt(r) != "todo" {
		t.Fatalf("Alt-P left the checklist at %v beside a conversation of %v (saved: %v), keys at the %s", todo.rect, r.chrome.main, loadDockedPanels(), keysAt(r))
	}
	press("<M-p>")
	if loadDockedPanels()[floatChecklist] || todo.docked || r.chrome.main.Dx() != 160 || keysAt(r) != "todo" {
		t.Fatalf("Alt-P again left the checklist at %v (saved: %v)", todo.rect, loadDockedPanels())
	}

	// The same key docks the agent panel while it has them.
	press("<Tab>")
	press("<Tab>")
	if keysAt(r) != "agents" || !r.chrome.floating {
		t.Fatalf("the keys are at the %s, floating=%v", keysAt(r), r.chrome.floating)
	}
	press("<M-p>")
	if r.chrome.floating || !loadDockedPanels()[floatView] || keysAt(r) != "agents" {
		t.Fatalf("Alt-P did not dock the agent panel (saved: %v)", loadDockedPanels())
	}

	// Its divider's width controls move a docked panel, and not one that
	// floats: that has no divider.
	was := r.inspectorRatio
	r.resizeInspector(1)
	if r.inspectorRatio == was {
		t.Fatal("a docked panel's width control did nothing")
	}
	press("<M-p>")
	was = r.inspectorRatio
	r.resizeInspector(1)
	if !r.chrome.floating || r.inspectorRatio != was {
		t.Fatalf("a floating panel's width was changed where nothing shows it: %v to %v", was, r.inspectorRatio)
	}

	// In the composer the key is not a window's, and docks nothing.
	press("!")
	press("<M-p>")
	if docks := loadDockedPanels(); len(docks) != 0 || keysAt(r) != "composer" {
		t.Fatalf("Alt-P in the composer docked %v", docks)
	}
}

// An approval is answered before a window is closed: Escape denies the call
// whichever of them has the keys.
func TestEscapeAnswersAnApprovalBeforeItClosesAWindow(t *testing.T) {
	r, _, todo, _ := ownPanels(t, 160, 40)
	ownKey(r, "<Tab>")
	if keysAt(r) != "todo" {
		t.Fatalf("the keys are at the %s", keysAt(r))
	}
	approval := &approvalState{calls: []messages.ChatMessageToolCall{{ID: "1", Name: "bash"}}, reply: make(chan []bool, 1)}
	r.model.approval = approval
	r.model.renderInputForTerminal(6, 160)
	ownKey(r, "<Escape>")
	select {
	case out := <-approval.reply:
		if len(out) != 1 || out[0] {
			t.Fatalf("Escape did not deny the call: %v", out)
		}
	default:
		t.Fatal("Escape left the approval pending")
	}
	if r.panes.floatOf(floatChecklist) != todo || !r.workspace().inspector.open {
		t.Fatal("Escape closed a window instead of answering the approval")
	}
	// With nothing else waiting on it, Escape closes the window with the keys.
	r.handleEvent(mouseEvent("<MouseLeft>", todo.rect.Min.Add(image.Pt(2, 1))))
	ownKey(r, "<Escape>")
	if r.panes.floatOf(floatChecklist) != nil || !r.workspace().inspector.open {
		t.Fatal("Escape did not close just the window that had the keys")
	}
}

// Every window of a frame is placed from one layout: what floats lines up in
// one column whatever is docked beside it, a window's height is settled
// before the view's frame is cut around it, and a terminal too short to dock
// in shows the mark for it on none of them.
func TestTheWindowsOfOneFrameAgree(t *testing.T) {
	r, screen, todo, usage := ownPanels(t, 130, 40)
	rows := r.frameLayoutFor(130, 40).transcriptHeight
	column := func(what string) {
		t.Helper()
		g := r.chrome
		if !g.floating || todo.rect.Min.X != g.frame.Min.X || todo.rect.Max.X != g.frame.Max.X {
			t.Fatalf("%s: the checklist at %v and the view at %v do not line up", what, todo.rect, g.frame)
		}
		if g.frame.Min.Y != todo.rect.Max.Y || g.frame.Max.X != g.main.Max.X {
			t.Fatalf("%s: the view at %v does not start under the checklist at %v, at the conversation's right (%v)", what, g.frame, todo.rect, g.main)
		}
	}

	// All three float in one column, edge to edge, the first frame they are in.
	column("floating")
	if g := r.chrome; usage.rect.Min.X != g.frame.Min.X || g.frame.Max.Y != usage.rect.Min.Y || usage.rect.Max.Y != rows {
		t.Fatalf("the view at %v does not end where the popover at %v starts, above the composer at row %d", g.frame, usage.rect, rows)
	}

	// A reading that lands with more rows makes the popover taller in the
	// same frame that cuts the view shorter: neither is a frame behind. On a
	// terminal tall enough for the popover to have every row it asks for.
	screen.SetSize(130, 90)
	waitInspector(t, r, 130)
	r.render()
	tall := r.frameLayoutFor(130, 90).transcriptHeight
	d, was := usage.panel.modal.context, usage.rect
	reading := d.snapshot
	for _, role := range []string{"first", "second", "third", "fourth"} {
		reading.roles = append(reading.roles, contextRoleUsage{role: role, count: 2, tokens: 900})
	}
	landed := make(chan contextSnapshotResult, 1)
	landed <- contextSnapshotResult{snapshot: reading}
	d.pending = landed
	r.render()
	if usage.rect.Dy() == was.Dy() {
		t.Fatalf("the reading left the popover at %v: nothing to lay out again", usage.rect)
	}
	column("after a reading")
	if g := r.chrome; g.frame.Max.Y != usage.rect.Min.Y || usage.rect.Max.Y != tall {
		t.Fatalf("after a reading the view at %v does not end where the popover at %v starts", g.frame, usage.rect)
	}
	screen.SetSize(130, 40)
	waitInspector(t, r, 130)
	r.render()

	// The popover docks: the other two still share one column, beside it,
	// and leave the conversation columns of its own to be read in.
	r.model.mu.Lock()
	r.toggleDock(floatContext)
	r.model.mu.Unlock()
	r.render()
	column("beside a docked popover")
	if g := r.chrome; !usage.docked || usage.rect.Min.X != g.main.Max.X || g.frame.Min.X < ownMinConversationCols {
		t.Fatalf("the popover is docked at %v beside %v, with the view at %v", usage.rect, g.main, g.frame)
	}
	if r.surface.body.Inner.Dx() != r.chrome.inner.Dx() {
		t.Fatalf("the view is wrapped for %d columns in a window with %d", r.surface.body.Inner.Dx(), r.chrome.inner.Dx())
	}

	// A terminal too short to dock in carries no mark to: on the view's
	// frame as on the others'.
	r.model.mu.Lock()
	r.toggleDock(floatContext)
	r.model.mu.Unlock()
	screen.SetSize(130, 8)
	waitInspector(t, r, 130)
	r.render()
	if g := r.chrome; !g.pin.Empty() || !todo.pinAt.Empty() || !usage.pinAt.Empty() {
		t.Fatalf("a short terminal shows the mark to dock: view %v, checklist %v, popover %v", g.pin, todo.pinAt, usage.pinAt)
	}
}
