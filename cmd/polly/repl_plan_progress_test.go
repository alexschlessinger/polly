package main

import (
	"context"
	"image"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/sessions"
	rw "github.com/mattn/go-runewidth"
	ui "github.com/metaspartan/gotui/v5"
)

func progressFixture() *sessions.Plan {
	return &sessions.Plan{Mode: sessions.PlanAccepted, Todos: []sessions.PlanTodo{
		{Number: 3, PlanStep: 1, Text: "Match [titles]", State: sessions.TodoDone},
		{Number: 4, PlanStep: 2, Text: "Rank exact matches", State: sessions.TodoActive},
		{Number: 5, PlanStep: 3, Text: "Verify ordering", State: sessions.TodoPending},
	}}
}

func TestPlanProgressLiveDividerAndPanel(t *testing.T) {
	withDisplayTTY(t)
	r, screen := affordanceTestREPL(t)
	m := r.model
	m.busy, m.turnID = true, 3
	m.ed.setText("unfinished draft")
	before := m.fullTranscript()
	w, h := ui.TerminalDimensions()
	beforeLayout := r.frameLayoutFor(w, h)
	tui := &gotuiTurnUI{repl: r, model: m, turnID: m.turnID}
	plan := progressFixture()
	tui.TodoProgress(plan, plan.Todos)
	r.render()
	if layout := r.frameLayoutFor(w, h); layout != beforeLayout {
		t.Fatalf("tasks changed frame layout: %+v → %+v", beforeLayout, layout)
	}
	y := beforeLayout.composerRow(0) - 1
	link := m.todoLink
	divider := func() string { return screenRow(screenSnapshot(t, screen), y) }
	if row := divider(); !strings.HasSuffix(row, " ✓☐☐ ─") || link != image.Rect(w-5, y, w-2, y+1) || m.modal != nil {
		t.Fatalf("unexpected initial divider: %q, link %v, modal %v", row, link, m.modal)
	}
	if row := strings.TrimSpace(screenRow(screenSnapshot(t, screen), h-1)); row != "ctx" {
		t.Fatalf("task marks changed the status row: %q", row)
	}
	// Pending and blocked look the same until the checklist is opened.
	plan.Todos[2].State, plan.Todos[2].Reason = sessions.TodoBlocked, "[rank] check failed"
	tui.TodoProgress(plan, plan.Todos)
	r.render()
	if row := divider(); !strings.HasSuffix(row, " ✓☐☐ ─") {
		t.Fatalf("blocker added noise to the divider: %q", row)
	}
	key := func(id string) { r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: id}) }
	key("<S-Tab>")
	key("<Enter>")
	r.render()
	modal := r.focusedPanelModal()
	if modal == nil || modal.todo == nil || m.modal != nil || modal.bounds.Min.Y != 0 || modal.bounds.Max.X != w || modal.bounds.Max.Y > beforeLayout.transcriptHeight {
		t.Fatalf("keyboard did not open the checklist at the conversation's top right: %v", modal)
	}
	wantBody := "✓ #3  Match [titles]\n▶ #4  Rank exact matches\n! #5  Verify ordering\n      [rank] check failed\nView plan"
	if body := plainStyledText(panelPage(r, floatChecklist).Text); body != wantBody {
		t.Fatalf("expanded plan checklist:\n%s\nwant:\n%s", body, wantBody)
	}
	key("<Tab>")
	if modal.selectedValue() != planViewAction {
		t.Fatal("accepted view not keyboard-reachable")
	}
	// Refresh an open panel in place, retaining its focus and scroll selection.
	plan.Todos[2].State, plan.Todos[2].Reason = sessions.TodoDone, ""
	tui.TodoProgress(plan, plan.Todos)
	r.render()
	title := screenRow(screenSnapshot(t, screen), modal.bounds.Min.Y)
	if r.focusedPanelModal() != modal || !strings.Contains(title, "TODO · 2/3 done") || !strings.HasSuffix(divider(), " ✓☐✓ ─") {
		t.Fatalf("live update did not refresh the panel and divider together: %q", title)
	}
	plan.Todos[1].State, plan.Todos[1].Reason = sessions.TodoBlocked, strings.Repeat("long diagnostic ", 12)
	tui.TodoProgress(plan, plan.Todos)
	r.render()
	if modal.selectedValue() != planViewAction {
		t.Fatal("a new multiline reason moved keyboard focus off the plan action")
	}
	screen.SetSize(16, 12) // Narrow enough to wrap the action itself.
	r.render()
	if modal.selectedValue() != planViewAction || modal.top == 0 {
		t.Fatal("resizing lost the plan action or its scroll position")
	}
	screen.SetSize(w, h)
	r.render()
	key("<Escape>")
	r.render()
	click := mouseEvent("<MouseLeft>", link.Min)
	r.handleEvent(click)
	r.render()
	if r.focusedPanelModal() == nil {
		t.Fatal("clicking the status marks did not open the checklist")
	}
	r.handleEvent(click) // The marks sit outside the panel.
	if r.focusedPanelModal() != nil || r.panes.floatOf(floatChecklist) != nil || m.fullTranscript() != before || m.ed.text() != "unfinished draft" || !m.busy {
		t.Fatal("inspection changed the composer, transcript or turn")
	}
}

func TestTodoPanelTaskWrapsAndStyles(t *testing.T) {
	todo := &sessions.PlanTodo{Number: 42, PlanStep: 3, State: sessions.TodoBlocked,
		Text: "確認 [rank] with fixtures", Reason: "[old] export failed"}
	items := appendTodoPanelTask(nil, todo, 22)
	want := "! #42  確認 [rank]\n       with fixtures\n       [old] export\n       failed"
	var lines []string
	for _, item := range items {
		lines = append(lines, plainStyledText(item.display))
	}
	if got := strings.Join(lines, "\n"); got != want {
		t.Fatalf("wrapped task:\n%s\nwant:\n%s", got, want)
	}
	if !strings.HasPrefix(items[0].display, "[!](fg:err,mod:bold) [#42](fg:muted) ") {
		t.Fatalf("status and ID styles missing: %q", items[0].display)
	}

	// Even with too little space for a hanging indent, keep every cell of
	// the badge, task and blocker reachable instead of truncating them.
	for width := 2; width < 15; width++ {
		var text strings.Builder
		for _, item := range appendTodoPanelTask(nil, todo, width) {
			if rw.StringWidth(item.label) > width {
				t.Fatalf("width %d: overwide row %q", width, item.label)
			}
			text.WriteString(strings.ReplaceAll(item.label, " ", ""))
		}
		if got, want := text.String(), "!#42確認[rank]withfixtures[old]exportfailed"; got != want {
			t.Fatalf("width %d: got %q, want %q", width, got, want)
		}
	}
}

func TestTodoDividerClearsOnCompletion(t *testing.T) {
	withDisplayTTY(t)
	r, screen := affordanceTestREPL(t)
	m := r.model
	plan := progressFixture()
	tui := &gotuiTurnUI{repl: r, model: m, turnID: m.turnID}
	tui.TodoProgress(plan, plan.Todos)
	r.render()
	if m.todoLink.Empty() {
		t.Fatal("unfinished tasks should show task marks")
	}
	r.openTodoPanel()
	panel := r.focusedPanelModal()
	for i := range plan.Todos {
		plan.Todos[i].State = sessions.TodoDone
	}
	tui.TodoProgress(plan, plan.Todos)
	r.render()
	w, _ := ui.TerminalDimensions()
	if !m.todoLink.Empty() || plainStyledText(r.dividerW.Text) != strings.Repeat("─", w) {
		t.Fatal("all-done tasks left marks or a hitbox on the divider")
	}
	title := screenRow(screenSnapshot(t, screen), panel.bounds.Min.Y)
	if r.focusedPanelModal() != panel || !strings.Contains(title, "TODO · 3/3 done") || strings.Contains(plainStyledText(panelPage(r, floatChecklist).Text), "3/3 done") {
		t.Fatalf("completion should stay in the open panel's header: %q", title)
	}
	r.closeFloats()
	plan.Mode = sessions.PlanDraft
	tui.TodoProgress(plan, plan.Todos)
	r.render()
	if row := plainStyledText(r.dividerW.Text); !strings.HasSuffix(row, " PLAN ─") || m.todoLink.Empty() {
		t.Fatalf("draft lost its PLAN label or kept completed marks: %q", row)
	}
	plan.Mode = sessions.PlanAccepted
	plan.Todos = append(plan.Todos, sessions.PlanTodo{Number: 6, Text: "Check the release", State: sessions.TodoPending})
	tui.TodoProgress(plan, plan.Todos)
	r.render()
	if row := plainStyledText(r.dividerW.Text); !strings.HasSuffix(row, " ✓✓✓☐ ─") || m.todoLink.Empty() {
		t.Fatalf("new task did not bring back the divider marks: %q", row)
	}
}

func TestTodoDividerFitsNarrowWidths(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	plan := progressFixture()
	todos := append(plan.Todos, make([]sessions.PlanTodo, 20)...)
	m.setPlanProgress(plan, todos)
	for width := 1; width <= 80; width++ {
		l := r.frameLayoutFor(width, 10)
		row := plainStyledText(m.dividerRow(l))
		link := m.todoLink
		want := ""
		switch {
		case width >= 15:
			want = "✓" + strings.Repeat("☐", 8) + "…"
		case width >= 7:
			want = "✓" + strings.Repeat("☐", width-7) + "…"
		case width == 6:
			want = "✓"
		}
		if rw.StringWidth(row) != width || link.Dx() != rw.StringWidth(want) || want != "" && (!strings.HasSuffix(row, " "+want+" ─") || link.Max.X != width-2 || link.Min.Y != l.composerRow(0)-1) {
			t.Fatalf("width %d: row %q, hitbox %v, want marks %q", width, row, link, want)
		}
	}
	plan.Mode = sessions.PlanDraft
	m.setPlanProgress(plan, plan.Todos)
	for width, want := range map[int]string{9: "PLAN", 21: "PLAN ✓☐☐"} {
		if row := plainStyledText(m.dividerRow(r.frameLayoutFor(width, 10))); !strings.HasSuffix(row, " "+want+" ─") {
			t.Fatalf("draft divider at %d columns: %q, want %q", width, row, want)
		}
	}
	m.status.parentName = "caller"
	for _, width := range []int{20, 80} {
		row := plainStyledText(m.dividerRow(r.frameLayoutFor(width, 10)))
		if !strings.HasPrefix(row, "← Back to caller ") || m.parentLink.Overlaps(m.todoLink) || m.todoLink.Empty() != (width == 20) {
			t.Fatalf("width %d: caller and task links collide or disappear: %q", width, row)
		}
	}
	if row := m.dividerRow(r.frameLayoutFor(80, 3)); row != "" || !m.todoLink.Empty() {
		t.Fatalf("short terminal retained the divider or hitbox: %q, %v", row, m.todoLink)
	}
	m.quiet = true
	if row := m.dividerRow(r.frameLayoutFor(80, 24)); row != "" || !m.todoLink.Empty() {
		t.Fatalf("quiet mode retained the divider or hitbox: %q, %v", row, m.todoLink)
	}
}

func TestTodoDividerSharedWithInspector(t *testing.T) {
	withDisplayTTY(t)
	r, screen := chromeTestREPL(t)
	m := r.model
	plan := progressFixture()
	m.setPlanProgress(plan, plan.Todos)
	m.appendNoticeLine("inspected body")
	r.inspect(tabViewTarget(r.visibleTab()))
	for _, width := range []int{140, 80} {
		screen.SetSize(width, 40)
		waitInspector(t, r, width)
		m.ed.setText("first line\nsecond line")
		r.workspace().inspector.focused = true
		r.render()
		link := m.todoLink
		y := r.inputW.Inner.Min.Y - 1
		if row := screenRow(screenSnapshot(t, screen), y); !strings.HasSuffix(row, " ✓☐☐ ╯") || link != image.Rect(width-5, y, width-2, y+1) {
			t.Fatalf("width %d: inspector overwrote or displaced task marks: %q, %v", width, row, link)
		}
		r.handleEvent(mouseEvent("<MouseLeft>", link.Min))
		if p := r.focusedPanelModal(); p == nil || p.todo == nil || r.inspectorFocused() || !r.workspace().inspector.open {
			t.Fatalf("width %d: inspector intercepted the task control", width)
		}
		r.closeFloats()
	}
}

func TestPlanProgressWidthsAndDraftActions(t *testing.T) {
	withDisplayTTY(t)
	r, screen := affordanceTestREPL(t)
	m := r.model
	path := "/private/draft plans/本日 [steps].md"
	m.setPlanProgress(&sessions.Plan{Mode: sessions.PlanDraft, Path: path}, nil)
	r.render()
	if row := plainStyledText(r.dividerW.Text); !strings.HasSuffix(row, " PLAN ─") {
		t.Fatalf("unexpected draft divider: %q", row)
	}
	screen.SetSize(21, 10)
	r.render()
	if row := screenRow(screenSnapshot(t, screen), 7); !strings.HasSuffix(row, " PLAN ─") {
		t.Fatalf("bad narrow draft divider: %q", row)
	}
	r.handleEvent(mouseEvent("<MouseLeft>", m.todoLink.Min))
	r.render()
	panel := r.focusedPanelModal()
	if panel == nil || panel.title != "PLAN" || !panel.bounds.In(image.Rect(0, 0, 21, 10)) {
		t.Fatalf("missing narrow plan panel")
	}
	var details strings.Builder
	for _, item := range panel.items {
		details.WriteString(item.label)
	}
	if !strings.Contains(strings.ReplaceAll(details.String(), " ", ""), strings.ReplaceAll(path, " ", "")) {
		t.Fatalf("full path unreachable: %q", details.String())
	}
	r.closeFloats()
	plan := progressFixture()
	plan.Todos[1].Text = "改善 [rank] " + strings.Repeat("終", 20)
	m.setPlanProgress(plan, plan.Todos)
	r.openTodoPanel()
	r.render()
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<End>"})
	r.render()
	panel = r.focusedPanelModal()
	if panel.top == 0 || panel.selectedValue() != planViewAction {
		t.Fatalf("short panel does not scroll to view action: %+v", panel)
	}
	for _, item := range panel.items {
		if rw.StringWidth(item.label) > panel.bounds.Dx()-2 {
			t.Fatalf("wide row: %q", item.label)
		}
	}
	r.closeFloats()
	m.setPlanProgress(&sessions.Plan{Mode: sessions.PlanExited, Path: path}, nil)
	r.render()
	if !m.todoLink.Empty() || plainStyledText(r.dividerW.Text) != strings.Repeat("─", 21) {
		t.Fatal("exited draft kept a stale marker or hitbox")
	}
	m.setPlanProgress(&sessions.Plan{Mode: sessions.PlanAccepted, Path: path}, nil)
	r.render()
	if !m.todoLink.Empty() || plainStyledText(r.dividerW.Text) != strings.Repeat("─", 21) {
		t.Fatal("accepted plan without tasks added an indicator")
	}
}

func TestTodoPanelOpensPlanWithKeyboardAndMouse(t *testing.T) {
	withDisplayTTY(t)
	r, screen := affordanceTestREPL(t)
	c, run, _ := planCommandFixture(t)
	r.state = c.state
	run("/plan")
	const draft = "# Import rollout\n\n## Steps\n- Check the staging export\n"
	rev := sessions.PlanRevision(draft)
	editPlanInTest(t, mustReadCommandPlan(t, c).Path, draft)
	key := func(id string) { r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: id}) }

	r.runCommand("/todo")
	r.render()
	key("<Tab>")
	key("<Enter>")
	if r.focusedPanelModal() != nil || r.state.planReview != rev {
		t.Fatal("Tab and Enter did not review the draft")
	}
	if r.panes.floatOf(floatChecklist) == nil {
		t.Fatal("reviewing the draft closed the checklist beside it")
	}
	r.runCommand("/todo") // Closed, to open again with the keys.
	ctx := context.Background()
	ps := r.state.session.(sessions.PlanSession)
	if started, err := ps.AcceptPlan(ctx, rev, draft); !started || err != nil {
		t.Fatalf("accept plan fixture: started=%v err=%v", started, err)
	}

	screen.SetSize(24, 10)
	r.runCommand("/todo")
	r.render()
	key("<End>")
	r.render()
	m := r.focusedPanelModal()
	y := m.listBounds.Min.Y + m.selected - m.top
	if row := screenRow(screenSnapshot(t, screen), y); !strings.Contains(row, "View plan") || strings.Contains(row, "· v") {
		t.Fatalf("End did not scroll to the painted plan action: %q", row)
	}
	r.handleEvent(mouseEvent("<MouseLeft>", image.Pt(m.listBounds.Min.X, y)))
	if r.focusedPanelModal() != nil || !strings.Contains(r.model.fullTranscript(), "Accepted plan (read-only) · "+rev) {
		t.Fatal("click did not view the accepted plan")
	}
	// The checklist stands beside the composer, where a letter is typed.
	r.handleEvent(mouseEvent("<MouseLeft>", image.Pt(m.listBounds.Min.X, m.listBounds.Min.Y)))
	key("v")
	if r.focusedPanelModal() != nil || r.panes.floatOf(floatChecklist) == nil || r.model.ed.text() != "v" || strings.Count(r.model.fullTranscript(), "Accepted plan (read-only) · "+rev) != 1 {
		t.Fatalf("a letter typed beside the checklist did not reach the composer: %q", r.model.ed.text())
	}
	if rows, err := r.state.session.(sessions.TodoSession).GetTodos(ctx); err != nil || len(rows) != 0 || r.model.busy {
		t.Fatalf("inspection started work or initialized todos: %+v, %v", rows, err)
	}
}

func TestTodoPanelShowsOnlyApplicablePlanDetails(t *testing.T) {
	withDisplayTTY(t)
	r, screen := affordanceTestREPL(t)
	ordinary := []sessions.PlanTodo{{Number: 1, Text: "Check notes", State: sessions.TodoPending}}
	const empty = "No todos yet. Ask Polly to track a task here."
	for _, tc := range []struct {
		name        string
		plan        *sessions.Plan
		todos       []sessions.PlanTodo
		title, body string
	}{
		{name: "empty", title: "TODO", body: empty},
		{name: "standalone", todos: ordinary, title: "TODO · 0/1 done", body: "○ #1  Check notes"},
		{name: "exited", plan: &sessions.Plan{Mode: sessions.PlanExited, Path: "old.md"}, todos: ordinary,
			title: "TODO · 0/1 done", body: "○ #1  Check notes"},
		{name: "draft", plan: &sessions.Plan{Mode: sessions.PlanDraft, Path: "draft.md"},
			title: "PLAN", body: empty + "\nDraft: draft.md\nReview draft"},
		{name: "accepted without linked items", plan: &sessions.Plan{Mode: sessions.PlanAccepted}, todos: ordinary,
			title: "TODO · 0/1 done", body: "○ #1  Check notes\nPlan accepted. Its checklist hasn’t been started yet.\nView plan"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r.model.setPlanProgress(tc.plan, tc.todos)
			r.openTodoPanel()
			defer r.closeFloats()
			r.render()
			title := screenRow(screenSnapshot(t, screen), r.focusedPanelModal().bounds.Min.Y)
			if !strings.Contains(title, "╭─"+tc.title) || !strings.HasSuffix(title, "╮") {
				t.Fatalf("checklist header = %q, want %q", title, tc.title)
			}
			if body := plainStyledText(panelPage(r, floatChecklist).Text); body != tc.body {
				t.Fatalf("checklist body:\n%s\nwant:\n%s", body, tc.body)
			}
		})
	}
}

func TestTodoPanelKeepsTaskAnchorAcrossLiveUpdates(t *testing.T) {
	r, screen := affordanceTestREPL(t)
	screen.SetSize(46, 20)
	plan := progressFixture()
	all := append([]sessions.PlanTodo{{Number: 1, State: sessions.TodoPending, Text: "Send notes"}}, plan.Todos...)
	ui := &gotuiTurnUI{repl: r, model: r.model, turnID: r.model.turnID}
	ui.TodoProgress(plan, all)
	r.openTodoPanel()
	r.render()
	m := r.focusedPanelModal()
	if m == nil {
		t.Fatal("missing checklist")
	}
	wantBody := "Approved outcomes\n✓ #3  Match [titles]\n▶ #4  Rank exact matches\n○ #5  Verify ordering\nSession tasks\n○ #1  Send notes\nView plan"
	if text := plainStyledText(panelPage(r, floatChecklist).Text); text != wantBody {
		t.Fatalf("mixed checklist:\n%s\nwant:\n%s", text, wantBody)
	}
	for i, item := range m.items {
		if strings.Contains(item.label, "Send notes") {
			m.selected = i
			break
		}
	}
	p := r.model.planProgress
	ui.TodoProgress(plan, all)
	if p != r.model.planProgress {
		t.Fatal("identical committed snapshot rebuilt the checklist")
	}
	all[3].State, all[3].Reason = sessions.TodoBlocked, strings.Repeat("new diagnostic ", 9)
	all[0].Text = "Share notes"
	ui.TodoProgress(plan, all)
	r.render()
	if r.focusedPanelModal() != m || !strings.Contains(m.items[m.selected].label, "Share notes") {
		t.Fatalf("live update lost the reading position: %+v", m.items[m.selected])
	}
}

func TestTodoPanelKeepsReadingAnchorWhenReasonClears(t *testing.T) {
	r, screen := affordanceTestREPL(t)
	screen.SetSize(27, 12)
	todos := []sessions.PlanTodo{
		{Number: 1, Text: "Verify the [staging] fixtures", State: sessions.TodoBlocked, Reason: strings.Repeat("new diagnostic ", 16)},
		{Number: 2, Text: "Send notes", State: sessions.TodoPending},
	}
	r.model.setPlanProgress(nil, todos)
	r.openTodoPanel()
	r.render()
	m := r.focusedPanelModal()
	const reasonID = "todo/1/reason"
	for i, item := range m.items {
		if item.identity == reasonID {
			m.selected = i + 4 // Read a continuation well below the first screen.
			break
		}
	}
	r.render()
	if m.top == 0 || m.items[m.selected].identity != reasonID {
		t.Fatal("test did not scroll to a blocker continuation")
	}
	screenRow := m.selected - m.top
	screen.SetSize(32, 12)
	r.render()
	if m.items[m.selected].identity != reasonID || m.selected-m.top != screenRow {
		t.Fatalf("resize lost the blocker reading position: %+v", m.items[m.selected])
	}
	todos[0].State, todos[0].Reason = sessions.TodoActive, ""
	r.model.setPlanProgress(nil, todos)
	r.render()
	if item := m.items[m.selected]; item.identity != "todo/1" || !strings.HasPrefix(item.label, "▶ #1  Verify") {
		t.Fatalf("clearing a reason moved focus away from its task: %+v", item)
	}
}

func TestTodoHydratesStandaloneWithoutOpeningPanel(t *testing.T) {
	ctx := context.Background()
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "ordinary")
	store := r.state.session.(sessions.TodoSession)
	if err := store.AddTodo(ctx, "Check notes"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetTodoState(ctx, 1, sessions.TodoActive, ""); err != nil {
		t.Fatal(err)
	}
	_, m, err := r.newTabModelContext(ctx, r.state)
	if err != nil {
		t.Fatal(err)
	}
	if m.planProgress.dividerLabel(80) != "☐" || m.planProgress.current != "Active: Check notes" || m.modal != nil || m.busy {
		t.Fatalf("incorrect resume: %+v", m.planProgress)
	}
	if strings.Contains(m.fullTranscript(), "Check notes") {
		t.Fatal("rehydration duplicated todos in the transcript")
	}
	prompt := &replCommandContext{state: r.state}
	var notices []string
	prompt.reply = func(line string) error { notices = append(notices, line); return nil }
	for _, first := range []bool{true, false} {
		if got, err := linePlanPrompt(ctx, prompt, first); got != "> " || err != nil {
			t.Fatalf("prompt %q: %v", got, err)
		}
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "TODO 0/1 done · Active: Check notes") {
		t.Fatalf("resume notices: %v", notices)
	}
}

func TestPlanReviewPresentationAtNarrowWidth(t *testing.T) {
	withDisplayTTY(t)
	r, screen := affordanceTestREPL(t)
	screen.SetSize(30, 32)
	c, run, turns := planCommandFixture(t)
	run("/plan")
	text := "# Report\nKeep [notes] literally.\n## Steps\n- Confirm the result\n"
	editPlanInTest(t, mustReadCommandPlan(t, c).Path, text)
	c.replyMarkup = r.model.appendLine
	run("/plan review")
	r.render()
	frame := screenSnapshot(t, screen)
	var lines []string
	for y := 0; y < 32; y++ {
		lines = append(lines, strings.TrimSpace(screenRow(frame, y)))
	}
	visible := strings.Join(strings.Fields(strings.Join(lines, " ")), " ")
	for _, want := range []string{"Plan review", "Keep [notes] literally.", "For approval · 1 outcome", "1. Confirm the result", "/plan accept", "/plan exit"} {
		if !strings.Contains(visible, want) {
			t.Fatalf("narrow review missing %q: %q", want, visible)
		}
	}
	if len(*turns) != 0 || c.state.planReview != sessions.PlanRevision(text) {
		t.Fatal("view started work or failed to review the exact revision")
	}
}

func TestPlanProgressRemainsOnOriginatingGeneration(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	origin := newReplModel()
	origin.turnID = 7
	tui := &gotuiTurnUI{repl: r, model: origin, turnID: 7}
	plan := progressFixture()
	tui.TodoProgress(plan, plan.Todos)
	plan.Todos[0].Text = "later mutation"
	if r.model.planProgress != nil || origin.planProgress.todos[0].Text == "later mutation" {
		t.Fatal("progress aliased another tab or caller snapshot")
	}
	origin.turnID++
	tui.TodoProgress(nil, nil)
	if origin.planProgress == nil {
		t.Fatal("late callback replaced the new turn's projection")
	}
}
