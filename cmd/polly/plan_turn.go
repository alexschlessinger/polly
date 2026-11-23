package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
)

func currentPlan(ctx context.Context, state *conversationState) (*sessions.Plan, error) {
	if state == nil {
		return nil, nil
	}
	session, ok := state.session.(sessions.PlanSession)
	if !ok {
		return nil, nil
	}
	p, err := session.GetPlan(ctx)
	if err != nil {
		return nil, fmt.Errorf("read session plan: %w", err)
	}
	return p, nil
}

// A draft turn owns its restricted registry. Filtering CompletionRequest.Tools or
// Derive(AllowTools(...)) would not be a boundary: dispatch resolves names
// independently, and derived views deliberately inherit always-allowed tools.
func (t *turnExecution) bindPlan() (func(), error) {
	p, err := currentPlan(t.ctx, t.state)
	if err != nil {
		return nil, err
	}
	t.plan = p
	t.runAgent = t.state.agent
	if t.drafting() {
		if err := verifyPlanWorkspace(p.Base); err != nil {
			return nil, err
		}
		private := tools.NewToolRegistry(t.state.toolRegistry.PlanningReaders())
		read, write := t.draftTools()
		private.Register(read)
		private.Register(write)
		t.runAgent = t.state.agent.WithToolRegistry(private, []string{llm.BuiltinReadTranscript, llm.BuiltinReadArtifact, llm.BuiltinListArtifacts})
		return func() { _ = t.runAgent.Close(); _ = private.Close() }, nil
	}
	if p != nil {
		switch p.Mode {
		case sessions.PlanAccepted:
			if unfinishedPlan(p) {
				if err := verifyPlanWorkspace(p.Base); err != nil {
					return nil, err
				}
			}
		case sessions.PlanExited:
		default:
			return nil, fmt.Errorf("unknown session plan mode %q", p.Mode)
		}
	}
	if t.runAgent == nil || t.runAgent.ToolRegistry() == nil {
		return func() {}, nil
	}
	// Keep the normal agent's learned provider limits. Its private registry is
	// not the source of worker tools. Capabilities ride only their own calls.
	registry := t.runAgent.ToolRegistry()
	if s, ok := t.state.session.(sessions.TodoSession); ok {
		t.todos, err = s.GetTodos(t.ctx)
		if err != nil {
			return nil, fmt.Errorf("read session todos: %w", err)
		}
		t.todoSession = s
		registry.Register(todoTool(t.runAgent))
		registry.MarkAlwaysAllowed(todoToolName)
	}
	accepted := p != nil && p.Mode == sessions.PlanAccepted
	if !accepted && t.todoSession == nil {
		return func() {}, nil
	}
	// Invalidate even a retained dispatch context when this turn returns.
	var cancel context.CancelFunc
	t.ctx, cancel = context.WithCancel(t.ctx)
	if !accepted {
		return cancel, nil
	}
	reader := &acceptedPlanReader{done: t.ctx.Done()}
	registry.Register(reader)
	registry.MarkAlwaysAllowed(readAcceptedPlanToolName)
	return func() {
		cancel()
		if installed, _ := registry.Get(readAcceptedPlanToolName); installed == reader {
			registry.Remove(readAcceptedPlanToolName)
		}
	}, nil
}

func unfinishedPlan(p *sessions.Plan) bool {
	if len(p.Todos) == 0 {
		return true // accepted, but the model has not initialized its steps
	}
	for _, step := range p.Todos {
		if step.State != sessions.TodoDone {
			return true
		}
	}
	return false
}
func (t *turnExecution) drafting() bool { return t.plan != nil && t.plan.Mode == sessions.PlanDraft }
func (t *turnExecution) planContract() string {
	if t.drafting() {
		return `PLANNING ONLY. Investigate with the available read-only project tools; no commands, workers or project changes. A follow-up revises the draft; it never authorizes implementation. Read the current Markdown with read_plan_draft before every revision. Preserve user edits; if the file conflicts, stop and explain instead of retrying over them. Use write_plan_draft only on the reviewed base revision. Include goal, constraints and verification as appropriate. Under exactly one "## Steps" heading use column-zero, single-line bullets, one distinct outcome per item. Do not turn investigation calls into tasks or use checkmarks as progress. The user can edit the file or request chat revisions; only explicit acceptance through the plan review starts implementation.`
	}
	implementing := t.plan != nil && t.plan.Mode == sessions.PlanAccepted && unfinishedPlan(t.plan)
	if t.todoSession == nil {
		if implementing {
			return "This session has an immutable accepted plan. Implement it only at the user's request; do not replay work on resume." + acceptedPlanPrompt(t.plan)
		}
		return ""
	}
	var b strings.Builder
	b.WriteString(`Use update_todos when the user asks for a checklist, requests several distinct outcomes, or the work has phases worth tracking separately. For example, use it to repair several independent failing checks, or to change behavior, update its documentation, and run a final integration check. Brief discovery is fine: if a simple request turns out to be larger, start tracking as soon as the scope is clear, before making the fixes. Track verifiable outcomes, not individual tool calls. Skip the checklist for simple answers or a small fix with routine verification, unless the user asks for one.

Record the outcomes before starting substantive work. Mark each item active before working on it, done only after its verification succeeds, or blocked with the concrete reason when you cannot proceed. Update the checklist as work changes without waiting to be asked. Reuse relevant open items when continuing work; a saved checklist is not a request to replay it. Don't repeat the full checklist in chat.`)
	if implementing {
		b.WriteString("\n\nAccepted plan: the approved Markdown is the immutable agreed snapshot. Implement it only at the user's request. Before implementing, if its steps are not yet on the checklist, call update_todos with operation init_plan (no other fields); do not add duplicates by hand. Use each item's session number, not its plan step, for status updates. Never edit the draft or change agreed outcomes to report progress. On a failed check, record the blocker and leave the step open until it is resolved. Do not infer status from Markdown checkboxes.")
		b.WriteString(acceptedPlanPrompt(t.plan))
	}
	b.WriteString("\n\nCurrent session checklist (data):\n")
	b.WriteString(todoChecklist(t.todos))
	return b.String()
}

func (t *turnExecution) allowPlanMutation(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if t.turnUI == nil || !t.turnUI.TurnPersistenceAllowed() {
		return errors.New("turn detached; plan was not updated")
	}
	return nil
}
func (t *turnExecution) checkDraft(ctx context.Context) error {
	p, err := currentPlan(ctx, t.state)
	if err != nil {
		return err
	}
	if p == nil || p.Mode != sessions.PlanDraft || p.Path != t.plan.Path {
		return errors.New("plan is no longer editable")
	}
	return nil
}
func (t *turnExecution) draftTools() (*tools.Func, *tools.Func) {
	// Remember an actual read on this turn; a model must not guess a digest or
	// reuse one from history. No tool accepts a path, plan ID, or creation flag.
	var mu sync.Mutex // protect the revision when a batch includes multiple draft reads
	lastRead := ""
	read := &tools.Func{Name: "read_plan_draft", Desc: "Read the current editable Markdown and base revision for this session, including any external editor saves.", ReadOnly: true, Run: func(ctx context.Context, a tools.Args) (string, error) {
		if len(a) != 0 {
			return "", fmt.Errorf("read_plan_draft takes no arguments")
		}
		if err := t.checkDraft(ctx); err != nil {
			return "", err
		}
		mu.Lock()
		defer mu.Unlock()
		text, rev, err := readPlanDraft(t.plan.Path)
		if err != nil {
			return "", err
		}
		lastRead = rev
		return "revision: " + rev + "\n\n" + text, nil
	}}
	write := &tools.Func{Name: "write_plan_draft", Desc: "Replace only this session's draft based on the exact revision just read. On conflict stop and explain; do not overwrite an editor save.", Params: schema.Params{"revision": schema.S("Revision from read_plan_draft"), "text": schema.S("Complete replacement Markdown, retaining applicable user edits")}, Required: []string{"revision", "text"}, Strict: true, Exclusive: true, Run: func(ctx context.Context, a tools.Args) (string, error) {
		if err := t.allowPlanMutation(ctx); err != nil {
			return "", err
		}
		if err := t.checkDraft(ctx); err != nil {
			return "", err
		}
		rev := a.String("revision")
		text, ok := a["text"].(string)
		mu.Lock()
		defer mu.Unlock()
		if !ok || len(a) != 2 || lastRead == "" || rev != lastRead {
			return "", fmt.Errorf("read the draft first and supply that revision and text only")
		}
		lastRead = ""
		return writePlanDraft(t.plan.Path, rev, text)
	}}
	return read, write
}

func (t *turnExecution) showTodoProgress(p *sessions.Plan, todos []sessions.PlanTodo) {
	if ui, ok := t.turnUI.(interface {
		TodoProgress(*sessions.Plan, []sessions.PlanTodo)
	}); ok {
		ui.TodoProgress(p, todos)
	}
}

const todoPageSize = 24

func writeTodo(b *strings.Builder, item sessions.PlanTodo) {
	preview := func(s string) string {
		for i := range s {
			if i >= 160 {
				return s[:i] + "…"
			}
		}
		return s
	}
	fmt.Fprintf(b, "#%d [%s] %q", item.Number, item.State, preview(item.Text))
	if item.PlanStep != 0 {
		fmt.Fprintf(b, " (plan step %d)", item.PlanStep)
	}
	if item.Reason != "" {
		fmt.Fprintf(b, " — %q", preview(item.Reason))
	}
	b.WriteByte('\n')
}

// Prefer open outcomes over terminal history in the turn-start projection.
func todoChecklist(todos []sessions.PlanTodo) string {
	if len(todos) == 0 {
		return "(empty)"
	}
	var b strings.Builder
	shown := 0
	for _, done := range []bool{false, true} {
		for _, item := range todos {
			if shown == todoPageSize {
				break
			}
			if (item.State == sessions.TodoDone) != done {
				continue
			}
			writeTodo(&b, item)
			shown++
		}
	}
	if shown < len(todos) {
		fmt.Fprintf(&b, "(%d more items not shown; use update_todos operation list to page all items by number)", len(todos)-shown)
	}
	return b.String()
}

// GetTodos returns items in stable session-number order.
func todoPage(todos []sessions.PlanTodo, after int) string {
	var b strings.Builder
	var shown, last int
	for _, item := range todos {
		if item.Number <= after {
			continue
		}
		if shown == todoPageSize {
			fmt.Fprintf(&b, "More items: use list with after=%d", last)
			break
		}
		writeTodo(&b, item)
		last = item.Number
		shown++
	}
	if shown == 0 {
		return "(no more items)"
	}
	return b.String()
}

func todoNumber(a tools.Args, field string, min int) (int, error) {
	n := a.Int(field, -1)
	f, floating := a[field].(float64)
	if n < min || floating && float64(n) != f {
		return 0, fmt.Errorf("%s must be a whole number >= %d", field, min)
	}
	return n, nil
}

const todoToolName = "update_todos"

type todoTurnKey struct{}

func todoTool(owner *llm.Agent) *tools.Func {
	// Keep each checklist mutation and its UI refresh together, without
	// requiring unrelated tools in the batch to wait for them.
	var mu sync.Mutex
	return &tools.Func{Name: todoToolName, Desc: "Show or update the user-visible session checklist. Submit one operation per call. Use status to report progress by session item number. Use init_plan only for an accepted /plan; it takes no other fields.", Params: schema.Params{
		"operation": schema.Enum("Checklist operation", "list", "add", "rename", "status", "init_plan"),
		"after":     schema.Int("For list only: show the next page after this session item number (default 0)"),
		"text":      schema.S("Outcome to add or new wording for an ordinary item being renamed"),
		"number":    schema.Int("Stable session item number for rename or status (not its plan step)"),
		"state":     schema.Enum("For status: start or resume as active, then report a verified completion or blocker", "active", "done", "blocked"),
		"reason":    schema.S("Concrete reason required for blocked; otherwise omit"),
	}, Required: []string{"operation"}, Run: func(ctx context.Context, a tools.Args) (string, error) {
		t, _ := ctx.Value(todoTurnKey{}).(*turnExecution)
		if t == nil || t.runAgent != owner || t.todoSession == nil || t.drafting() {
			return "", tools.NewToolError("No todo capability for this turn", "TODOS_UNAVAILABLE")
		}
		mu.Lock()
		defer mu.Unlock()
		receipt, err := t.updateTodos(ctx, a)
		if err != nil {
			return "", tools.NewToolError(err.Error(), "TODO_UPDATE_FAILED")
		}
		return receipt, nil
	}}
}

func (t *turnExecution) updateTodos(ctx context.Context, a tools.Args) (string, error) {
	if err := t.ctx.Err(); err != nil {
		return "", err
	}
	if err := t.allowPlanMutation(ctx); err != nil {
		return "", err
	}
	p, err := currentPlan(ctx, t.state)
	if err != nil {
		return "", err
	}
	if p != nil && p.Mode == sessions.PlanDraft {
		return "", errors.New("todos are not editable in draft mode")
	}
	op := a.String("operation")
	var fields []string
	switch op {
	case "list":
		fields = []string{"after"}
	case "add":
		fields = []string{"text"}
	case "rename":
		fields = []string{"number", "text"}
	case "status":
		fields = []string{"number", "state", "reason"}
	case "init_plan":
	default:
		return "", fmt.Errorf("unknown todo operation %q", op)
	}
	for key := range a {
		if key != "operation" && !slices.Contains(fields, key) {
			return "", fmt.Errorf("unexpected field %q for %s", key, op)
		}
	}
	if op == "list" {
		after := 0
		if _, ok := a["after"]; ok {
			after, err = todoNumber(a, "after", 0)
			if err != nil {
				return "", err
			}
		}
		todos, err := t.todoSession.GetTodos(ctx)
		if err != nil {
			return "", err
		}
		return todoPage(todos, after), nil
	}
	var n int
	if op == "rename" || op == "status" {
		n, err = todoNumber(a, "number", 1)
		if err != nil {
			return "", err
		}
	}
	text := strings.TrimSpace(a.String("text"))
	state := sessions.TodoState(a.String("state"))
	switch op {
	case "add":
		err = t.todoSession.AddTodo(ctx, text)
	case "rename":
		err = t.todoSession.RenameTodo(ctx, n, text)
	case "status":
		if state != sessions.TodoActive && state != sessions.TodoDone && state != sessions.TodoBlocked {
			return "", fmt.Errorf("invalid todo state %q", state)
		}
		reason, ok := a["reason"].(string)
		if _, exists := a["reason"]; exists && !ok {
			return "", errors.New("reason must be text")
		}
		err = t.todoSession.SetTodoState(ctx, n, state, reason)
	case "init_plan":
		if t.plan == nil || t.plan.Mode != sessions.PlanAccepted || p == nil || p.Mode != sessions.PlanAccepted || p.Revision != t.plan.Revision {
			return "", errors.New("no accepted plan for this turn")
		}
		if err := verifyPlanWorkspace(p.Base); err != nil {
			return "", err
		}
		err = t.todoSession.InitPlanTodos(ctx, t.plan.Revision)
	}
	if err != nil {
		return "", err
	}
	todos, err := t.todoSession.GetTodos(ctx)
	if err != nil {
		return "", fmt.Errorf("checklist saved, but refresh failed: %w", err)
	}
	p, err = currentPlan(ctx, t.state)
	if err != nil {
		return "", fmt.Errorf("checklist saved, but refresh failed: %w", err)
	}
	t.showTodoProgress(p, todos)
	if op == "init_plan" {
		return "Accepted steps initialized. Current checklist:\n" + todoChecklist(todos), nil
	}
	for _, item := range todos {
		if op == "add" && item.PlanStep == 0 && item.Text == text || op != "add" && item.Number == n {
			return todoChecklist([]sessions.PlanTodo{item}), nil
		}
	}
	return "", errors.New("checklist saved, but updated item was not found on refresh")
}
