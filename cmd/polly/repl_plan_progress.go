package main

import (
	"context"
	"fmt"
	"image"
	"slices"
	"strings"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/sessions"
	rw "github.com/mattn/go-runewidth"
	ui "github.com/metaspartan/gotui/v5"
)

// readChecklist is inspection only. An accepted snapshot and a materialized
// checklist are different things; never infer rows from the Markdown here.
func readChecklist(ctx context.Context, state *conversationState) (*sessions.Plan, []sessions.PlanTodo, error) {
	plan, err := currentPlan(ctx, state)
	if err != nil || state == nil {
		return nil, nil, err
	}
	store, ok := state.session.(sessions.TodoSession)
	if !ok {
		return plan, nil, nil
	}
	todos, err := store.GetTodos(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("read session todos: %w", err)
	}
	return plan, todos, nil
}

type planProgress struct {
	mode           sessions.PlanMode
	path           string
	todos          []sessions.PlanTodo
	count, current string
	done, linked   int
}

func projectChecklist(plan *sessions.Plan, todos []sessions.PlanTodo) *planProgress {
	p := &planProgress{todos: slices.Clone(todos)}
	if plan != nil {
		p.mode, p.path = plan.Mode, plan.Path
	}
	priority := 0
	for _, step := range todos {
		_, label, _, rank := todoAppearance(step.State)
		if step.State == sessions.TodoDone {
			p.done++
		}
		if step.PlanStep > 0 {
			p.linked++
		}
		if rank > priority {
			priority = rank
			p.current = label + ": " + planLabel(step.Text)
			if step.State == sessions.TodoBlocked && step.Reason != "" {
				p.current = label + ": " + planLabel(step.Reason) + " — " + planLabel(step.Text)
			}
		}
	}
	p.count = fmt.Sprintf("%d/%d done", p.done, len(todos))
	if p.done == len(todos) && p.done > 0 {
		p.current = "Complete"
		if p.mode == sessions.PlanAccepted && p.linked == 0 {
			p.current = "Plan checklist not started"
		}
	}
	return p
}

// Caller holds m.mu. Preserve panel layout on repeated snapshots. In
// particular, neither a committed update nor a resize should open a panel.
func (m *replModel) setPlanProgress(plan *sessions.Plan, todos []sessions.PlanTodo) {
	var mode sessions.PlanMode
	var path string
	if plan != nil {
		mode, path = plan.Mode, plan.Path
	}
	if p := m.planProgress; p != nil && p.mode == mode && p.path == path && slices.Equal(p.todos, todos) {
		return
	}
	m.planProgress = projectChecklist(plan, todos)
}

// TodoProgress receives a committed database projection on the turn's tab.
// Detached generations cannot overwrite the current session's checklist.
func (t *gotuiTurnUI) TodoProgress(p *sessions.Plan, todos []sessions.PlanTodo) {
	t.model.mu.Lock()
	defer t.model.mu.Unlock()
	if t.activeLocked() {
		t.model.setPlanProgress(p, todos)
		if t.repl != nil {
			t.repl.wakeTabs()
		}
	}
}

func planLabel(s string) string { return planDisplayText(strings.Join(strings.Fields(s), " ")) }

func todoAppearance(s sessions.TodoState) (mark, label, role string, priority int) {
	switch s {
	case sessions.TodoBlocked:
		return "!", "Blocked", "err", 4
	case sessions.TodoActive:
		return "▶", "Active", "accent", 3
	case sessions.TodoPending:
		return "○", "Pending", "muted", 2
	case sessions.TodoDone:
		return "✓", "Done", "ok", 0
	default:
		return "?", "Unknown", "muted", 1
	}
}

const todoDividerMarkLimit = 10

// dividerLabel keeps the collapsed control to completion marks. The checklist
// panel carries the task names, states, counts, and blocker reasons.
func (p *planProgress) dividerLabel(width int) string {
	if p == nil || width <= 0 {
		return ""
	}
	label := ""
	if p.mode == sessions.PlanDraft {
		label = "PLAN"
		if width < len(label) {
			return ""
		}
		width -= len(label) + 1
	}
	if p.done == len(p.todos) || width <= 0 {
		return label
	}
	var marks strings.Builder
	for _, todo := range p.todos[:min(len(p.todos), todoDividerMarkLimit+1)] {
		if todo.State == sessions.TodoDone {
			marks.WriteRune('✓')
		} else {
			marks.WriteRune('☐')
		}
	}
	width = min(width, todoDividerMarkLimit)
	tail := "…"
	if width == 1 {
		tail = "" // Keep a real mark rather than showing a lone ellipsis.
	}
	text := rw.Truncate(marks.String(), width, tail)
	if label != "" && text != "" {
		return label + " " + text
	}
	return label + text
}

const (
	planViewAction   = "/plan view"
	planReviewAction = "/plan review"
)

// Share checklist order and plan details, not the frontends' task formatting.
type checklistRow struct {
	text, action string
	heading      bool
	todo         *sessions.PlanTodo
}

func (p *planProgress) checklistRows() []checklistRow {
	var rows []checklistRow
	if len(p.todos) == 0 && p.mode != sessions.PlanAccepted {
		rows = append(rows, checklistRow{text: "No todos yet. Ask Polly to track a task here."})
	}
	for _, linked := range []bool{true, false} {
		if linked && p.linked == 0 || !linked && len(p.todos) == p.linked {
			continue
		}
		heading := "Session tasks"
		if linked {
			heading = "Approved outcomes"
		}
		rows = append(rows, checklistRow{text: heading, heading: true})
		for i := range p.todos {
			step := &p.todos[i]
			if (step.PlanStep > 0) == linked {
				rows = append(rows, checklistRow{todo: step})
			}
		}
	}
	switch p.mode {
	case sessions.PlanAccepted:
		if p.linked == 0 {
			rows = append(rows, checklistRow{text: "Plan accepted. Its checklist hasn’t been started yet."})
		}
		rows = append(rows, checklistRow{text: "View accepted plan (read-only)", action: planViewAction})
	case sessions.PlanDraft:
		rows = append(rows, checklistRow{text: "Draft: " + planLabel(p.path)}, checklistRow{text: "Review draft", action: planReviewAction})
	}
	return rows
}

func (p *planProgress) checklistLines() []string {
	var lines []string
	if len(p.todos) > 0 {
		lines = append(lines, p.count)
	}
	for _, row := range p.checklistRows() {
		if step := row.todo; step != nil {
			mark, label, _, _ := todoAppearance(step.State)
			text := fmt.Sprintf("%s #%d  %s · %s", mark, step.Number, label, planLabel(step.Text))
			if step.PlanStep > 0 {
				text += fmt.Sprintf(" (step %d)", step.PlanStep)
			}
			lines = append(lines, text)
			if step.Reason != "" {
				lines = append(lines, "    Reason: "+planLabel(step.Reason))
			}
		} else if row.action != "" {
			lines = append(lines, row.text+": "+row.action)
		} else {
			lines = append(lines, row.text)
		}
	}
	return lines
}

// todoPanel is the checklist's read-only dialog. Its wrapped rows are rebuilt
// only when the projection or width changes, not on every activity tick.
// plain leaves the letter keys out of its rows: where the panel has no
// terminal of its own, letters are the composer's.
type todoPanel struct {
	model   *replModel
	painted *planProgress
	width   int
	plain   bool
}

// checklistWantLocked returns the live desired size for the TODO panel based
// on the current checklist length. Caller holds r.model.mu.
func (r *managedREPL) checklistWantLocked() floatWant {
	want := checklistWant
	want.rows++
	if p := r.model.planProgress; p != nil {
		want.rows += len(p.checklistRows()) - 1
	}
	return want
}

// openTodoPanel shows the checklist in a window (see repl_pane_float.go),
// which opening it again closes. Caller holds the visible model's lock.
func (r *managedREPL) openTodoPanel() {
	if r.closeFloat(floatChecklist) {
		return
	}
	m := r.newTodoModal()
	// A row per task and a few to spare, at the tab's top right.
	r.floatModal(floatChecklist, m, panelSpecs[floatChecklist].label, r.checklistWantLocked())
}

func (r *managedREPL) newTodoModal() *replModal {
	return &replModal{title: "TODO", width: 56, details: []string{}, todo: &todoPanel{model: r.model}}
}

func appendTodoPanelText(items []replModalItem, item replModalItem, role, indent string, width int) []replModalItem {
	for _, cells := range style.VisualRows(style.Escape(item.label), ui.StyleClear, width-len(indent)) {
		text := ui.CellsToString(cells)
		row := item
		row.label, row.display = indent+text, indent+style.Styled(text, role, "")
		items = append(items, row)
	}
	return items
}

func appendTodoPanelTask(items []replModalItem, todo *sessions.PlanTodo, width int) []replModalItem {
	mark, _, role, _ := todoAppearance(todo.State)
	number := fmt.Sprintf("#%d", todo.Number)
	id := fmt.Sprintf("todo/%d", todo.Number)
	badge := mark + " " + number
	styledMark := style.Styled(mark, role, "bold")
	styledBadge := styledMark + " " + style.Styled(number, "muted", "")
	indent := strings.Repeat(" ", rw.StringWidth(badge)+2)
	prefix, styledPrefix := badge+"  ", styledBadge+"  "
	if width-len(indent) < 8 {
		// On a very narrow terminal, put the badge above the task rather
		// than squeezing all its text into a thin hanging column.
		if rw.StringWidth(badge) <= width {
			items = append(items, replModalItem{label: badge, display: styledBadge, identity: id})
		} else {
			items = append(items, replModalItem{label: mark, display: styledMark, identity: id})
			items = appendTodoPanelText(items, replModalItem{label: number, identity: id}, "muted", "", width)
		}
		prefix, styledPrefix, indent = "", "", ""
	}
	start := len(items)
	items = appendTodoPanelText(items, replModalItem{label: planLabel(todo.Text), identity: id}, "", indent, width)
	if len(items) > start {
		row := &items[start]
		row.label = prefix + row.label[len(indent):]
		row.display = styledPrefix + row.display[len(indent):]
	}
	if todo.Reason != "" {
		items = appendTodoPanelText(items, replModalItem{label: planLabel(todo.Reason), identity: id + "/reason"}, "muted", indent, width)
	}
	return items
}

func (d *todoPanel) refreshItems(m *replModal, width int) {
	p := d.model.planProgress
	if width == d.width && p == d.painted {
		return
	}
	var selectedID string
	offset, screenRow := 0, max(0, m.selected-m.top)
	if i := m.selected; i >= 0 && i < len(m.items) {
		selectedID = m.items[i].identity
		for i > 0 && m.items[i-1].identity == selectedID {
			i--
			offset++
		}
	}
	d.width, d.painted = width, p
	if p == nil {
		p = &planProgress{}
	}
	m.items = nil
	m.title = "TODO"
	if p.mode == sessions.PlanDraft && len(p.todos) == 0 {
		m.title = "PLAN"
	}
	m.title = rw.Truncate(m.title, max(0, width-3), "")
	m.titleNotice, m.titleNoticeRole = "", "muted"
	if len(p.todos) > 0 {
		for _, notice := range []string{"· " + p.count + " ", fmt.Sprintf("· %d/%d ", p.done, len(p.todos))} {
			if rw.StringWidth(m.title+" "+notice) <= width-3 {
				m.titleNotice = notice
				break
			}
		}
	}
	bodyWidth := max(1, width-2)
	mixed := p.linked > 0 && p.linked < len(p.todos)
	for _, row := range p.checklistRows() {
		if row.todo != nil {
			m.items = appendTodoPanelTask(m.items, row.todo, bodyWidth)
			continue
		}
		if row.heading && !mixed {
			continue
		}
		text, role, id := row.text, "muted", row.text
		if row.action != "" {
			if row.action == planViewAction {
				text = "View plan"
			}
			if !d.plain {
				text += " · v"
			}
			role, id = "accent", row.action
		}
		item := replModalItem{label: text, value: row.action, identity: id}
		m.items = appendTodoPanelText(m.items, item, role, "", bodyWidth)
	}
	selected := todoPanelItemIndex(m.items, selectedID, offset)
	if taskID, reason := strings.CutSuffix(selectedID, "/reason"); selected < 0 && reason {
		// Clearing a blocker returns the reading position to its task.
		selected = todoPanelItemIndex(m.items, taskID, 0)
	}
	if selected >= 0 {
		m.selected = selected
		m.top = max(0, selected-screenRow)
	}
}

func todoPanelItemIndex(items []replModalItem, identity string, offset int) int {
	i := slices.IndexFunc(items, func(item replModalItem) bool { return item.identity == identity })
	if i >= 0 {
		for offset > 0 && i+1 < len(items) && items[i+1].identity == identity {
			i++
			offset--
		}
	}
	return i
}

func (p *planProgress) viewAction() string {
	if p != nil {
		switch p.mode {
		case sessions.PlanDraft:
			return planReviewAction
		case sessions.PlanAccepted:
			return planViewAction
		}
	}
	return ""
}

// A shortcut or painted action goes through the same command as typed input,
// without touching the draft. Re-check mode in case acceptance just landed.
func (r *managedREPL) activatePlanAction(action string) bool {
	if action == "" || action != r.model.planProgress.viewAction() {
		return false
	}
	// A window the action was chosen in stays up; a dialog closes.
	if r.focusedOwnWindow() != nil {
		r.yieldPanelKeys()
	} else if r.model.modal != nil {
		r.closeModal()
	}
	r.runCommand(action)
	return true
}

func (r *managedREPL) handleTodoPanelEvent(m *replModal, e ui.Event) bool {
	if e.Type == ui.MouseEvent {
		mouse, ok := e.Payload.(ui.Mouse)
		if !ok || e.ID != "<MouseLeft>" || !image.Pt(mouse.X, mouse.Y).In(m.listBounds) {
			return false
		}
		i := m.top + mouse.Y - m.listBounds.Min.Y
		return i >= 0 && i < len(m.items) && r.activatePlanAction(m.items[i].value)
	}
	switch e.ID {
	case "v":
		return r.activatePlanAction(r.model.planProgress.viewAction())
	case "<Enter>":
		return r.activatePlanAction(m.selectedValue())
	case "<Tab>", "<S-Tab>", "<Backtab>":
		for i, item := range m.items {
			if item.value != "" {
				m.selected = i
				break
			}
		}
		return true
	}
	return false
}
