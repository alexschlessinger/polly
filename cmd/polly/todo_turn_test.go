package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
)

type recordingTodoUI struct {
	collectingTurnUI
	plan  *sessions.Plan
	todos []sessions.PlanTodo
}

func (ui *recordingTodoUI) TodoProgress(p *sessions.Plan, todos []sessions.PlanTodo) {
	ui.plan, ui.todos = p, slices.Clone(todos)
}

func TestOrdinaryTodoAgentTurns(t *testing.T) {
	ctx := t.Context()
	var session sessions.Session
	phase := 0
	model := &compactTestLLM{reply: func(_ context.Context, req *llm.CompletionRequest) (messages.ChatMessage, error) {
		phase++
		var name, args string
		switch phase {
		case 1:
			history, err := session.GetHistory(ctx)
			if err != nil || len(history) != 1 || history[0].Content != "Ship a change" || history[0].Role != messages.MessageRoleUser {
				return messages.ChatMessage{}, fmt.Errorf("user not persisted at first request: %+v, %w", history, err)
			}
			for _, required := range []string{todoToolName, "test_changes", llm.BuiltinReadTranscript} {
				if !slices.ContainsFunc(req.Tools, func(tool tools.Tool) bool { return tool.GetName() == required }) {
					return messages.ChatMessage{}, fmt.Errorf("missing %s", required)
				}
			}
			contract := req.Messages[0].Content
			for _, guidance := range []string{
				"user asks for a checklist",
				"repair several independent failing checks",
				"if a simple request turns out to be larger",
				"small fix with routine verification",
				"Mark each item active before working on it",
				"Reuse relevant open items",
			} {
				if !strings.Contains(contract, guidance) {
					return messages.ChatMessage{}, fmt.Errorf("ordinary guidance missing %q: %s", guidance, contract)
				}
			}
			if strings.Contains(contract, "Accepted Markdown:") {
				return messages.ChatMessage{}, fmt.Errorf("accepted plan guidance on an ordinary turn: %s", contract)
			}
			name, args = todoToolName, `{"operation":"add","text":"Test the change"}`
		case 2:
			name, args = todoToolName, `{"operation":"rename","number":1,"text":"Verify round trip"}`
		case 3:
			name, args = todoToolName, `{"operation":"status","number":1,"state":"active"}`
		case 4:
			name, args = "test_changes", `{}`
		case 5:
			name, args = todoToolName, `{"operation":"status","number":1,"state":"done"}`
		case 7:
			if !strings.Contains(req.Messages[0].Content, `#1 [done] "Verify round trip"`) {
				return messages.ChatMessage{}, fmt.Errorf("next turn lacks saved todo: %s", req.Messages[0].Content)
			}
		}
		if name == "" {
			return messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "Verified.", StopReason: messages.StopReasonEndTurn}, nil
		}
		return messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: fmt.Sprint(phase), Name: name, Arguments: args}}}, nil
	}}
	checks := 0
	state, s := newInterruptedTurnState(t, model, &tools.Func{Name: "test_changes", Run: func(callCtx context.Context, _ tools.Args) (string, error) {
		if callCtx.Value(todoTurnKey{}) != nil {
			return "", fmt.Errorf("todo authority leaked to another tool")
		}
		checks++
		return "verified", nil
	}})
	session = s
	ui := &recordingTodoUI{}
	for _, prompt := range []string{"Ship a change", "What was verified?"} {
		code, err := executeTurnWithUserMessage(ctx, &Config{}, state, messages.ChatMessage{Role: messages.MessageRoleUser, Content: prompt}, nil, nil, ui, false)
		if code != 0 || err != nil {
			t.Fatalf("%q: code=%d err=%v", prompt, code, err)
		}
	}
	if phase != 7 || checks != 1 || ui.plan != nil || len(ui.todos) != 1 || ui.todos[0].Text != "Verify round trip" || ui.todos[0].State != sessions.TodoDone || ui.todos[0].PlanStep != 0 {
		t.Fatalf("calls=%d checks=%d plan=%+v todos=%+v", phase, checks, ui.plan, ui.todos)
	}
}

func TestTodoBatchWithIndependentTool(t *testing.T) {
	requests := 0
	model := &compactTestLLM{reply: func(_ context.Context, _ *llm.CompletionRequest) (messages.ChatMessage, error) {
		requests++
		if requests == 1 {
			return messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{
				{ID: "a", Name: todoToolName, Arguments: `{"operation":"add","text":"Implement the change"}`},
				{ID: "b", Name: todoToolName, Arguments: `{"operation":"add","text":"Verify the change"}`},
				{ID: "c", Name: "inspect_config", Arguments: `{}`},
			}}, nil
		}
		return messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "Inspected.", StopReason: messages.StopReasonEndTurn}, nil
	}}
	inspections := 0
	state, session := newInterruptedTurnState(t, model, &tools.Func{Name: "inspect_config", ReadOnly: true, Run: func(context.Context, tools.Args) (string, error) {
		inspections++
		return "config", nil
	}})
	ui := &recordingTodoUI{}
	ctx := t.Context()
	code, err := executeTurnWithUserMessage(ctx, &Config{}, state, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "Add a tier"}, nil, nil, ui, false)
	if code != 0 || err != nil {
		t.Fatalf("mixed batch: code=%d err=%v", code, err)
	}
	saved, err := session.(sessions.TodoSession).GetTodos(ctx)
	if err != nil || len(saved) != 2 || !slices.Equal(saved, ui.todos) || inspections != 1 || requests != 2 {
		t.Fatalf("saved=%+v ui=%+v inspections=%d requests=%d err=%v", saved, ui.todos, inspections, requests, err)
	}
}

func TestTodoAuthorityAndValidation(t *testing.T) {
	ctx := t.Context()
	state, s := newInterruptedTurnState(t, &scriptedStreamLLM{}, nil)
	turn := &turnExecution{ctx: ctx, state: state, turnUI: &recordingTodoUI{}}
	release, err := turn.bindPlan()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	update, ok := turn.runAgent.ToolRegistry().Get(todoToolName)
	if !ok {
		t.Fatal("no session todo tool")
	}
	if _, ok := state.toolRegistry.Get(todoToolName); ok {
		t.Fatal("worker source contains todo tool")
	}
	other, _ := newInterruptedTurnState(t, &scriptedStreamLLM{}, nil)
	otherTurn := &turnExecution{ctx: ctx, state: other, turnUI: &recordingTodoUI{}}
	closeOther, err := otherTurn.bindPlan()
	if err != nil {
		t.Fatal(err)
	}
	defer closeOther()
	add := map[string]any{"operation": "add", "text": "Forbidden"}
	for _, wrong := range []context.Context{ctx, context.WithValue(ctx, todoTurnKey{}, otherTurn)} {
		if _, err := update.Execute(wrong, add); err == nil {
			t.Fatal("unbound or cross-session call succeeded")
		}
	}
	bound := context.WithValue(ctx, todoTurnKey{}, turn)
	for _, invalid := range []map[string]any{
		{"operation": "init_plan"}, {"operation": "init_plan", "path": "/tmp/plan"}, {"operation": "add", "text": "Extra field", "number": 42}, {"operation": "add", "text": nil}, {"operation": "rename", "number": 1.5, "text": "Fraction"}, {"operation": "status", "number": 1, "state": "pending"}, {"operation": "status", "number": 1, "state": "blocked", "reason": 123},
	} {
		if _, err := update.Execute(bound, invalid); err == nil {
			t.Fatalf("allowed invalid call: %v", invalid)
		}
	}
	// An earlier ordinary handle cannot be used once this session enters a draft.
	if err := s.(sessions.PlanSession).StartPlan(ctx, "unused.md", "unused"); err != nil {
		t.Fatal(err)
	}
	if _, err := update.Execute(bound, add); err == nil {
		t.Fatal("stale handle edited a draft's checklist")
	}
	if all, err := s.(sessions.TodoSession).GetTodos(ctx); err != nil || len(all) != 0 {
		t.Fatalf("changed todos: %+v, %v", all, err)
	}
}

func TestUnsupportedSessionGetsNoTodoContractOrTool(t *testing.T) {
	state, s := newInterruptedTurnState(t, &scriptedStreamLLM{}, nil)
	state.session = struct{ sessions.Session }{s}
	turn := &turnExecution{ctx: t.Context(), state: state}
	release, err := turn.bindPlan()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, ok := turn.runAgent.ToolRegistry().Get(todoToolName); ok || turn.planContract() != "" {
		t.Fatal("unsupported session acquired the todo surface")
	}
}

func TestBoundedTodoChecklistPrefersOpenWork(t *testing.T) {
	var all []sessions.PlanTodo
	for i := 1; i <= 100; i++ {
		all = append(all, sessions.PlanTodo{Number: i, Text: strings.Repeat("終", 500), State: sessions.TodoDone})
	}
	all = append(all, sessions.PlanTodo{Number: 101, Text: "Run smoke test", Reason: "missing fixture\nnot policy", State: sessions.TodoBlocked, PlanStep: 2})
	got := todoChecklist(all)
	if len(got) > 6000 || !strings.Contains(got, `#101 [blocked] "Run smoke test" (plan step 2) — "missing fixture\nnot policy"`) || !strings.Contains(got, "77 more items") {
		t.Fatalf("unexpected checklist (%d bytes): %s", len(got), got)
	}
}

func TestListTodosPagesWithoutMutation(t *testing.T) {
	state, s := newInterruptedTurnState(t, &scriptedStreamLLM{}, nil)
	ctx := t.Context()
	session := s.(sessions.TodoSession)
	for i := 1; i <= todoPageSize+1; i++ {
		if err := session.AddTodo(ctx, fmt.Sprintf("Outcome %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	ui := &recordingTodoUI{}
	turn := &turnExecution{ctx: ctx, state: state, turnUI: ui}
	release, err := turn.bindPlan()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if strings.Contains(turn.planContract(), "Outcome 25") {
		t.Fatal("fixture item not hidden")
	}
	bound := context.WithValue(ctx, todoTurnKey{}, turn)
	tool, _ := turn.runAgent.ToolRegistry().Get(todoToolName)
	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"operation": "list"}, "after=24"},
		{map[string]any{"operation": "list", "after": 24}, `#25 [pending] "Outcome 25"`},
	} {
		page, err := tool.Execute(bound, tc.args)
		if err != nil || !strings.Contains(page, tc.want) {
			t.Fatalf("%v: %q %v", tc.args, page, err)
		}
	}
	all, err := session.GetTodos(ctx)
	if err != nil || !slices.Equal(all, turn.todos) || ui.todos != nil {
		t.Fatalf("list mutated progress: %v %v", all, err)
	}
	release()
	if _, err := tool.Execute(bound, map[string]any{"operation": "add", "text": "stale"}); err == nil {
		t.Fatal("retained context can write after turn release")
	}
}

func TestUninitializedPlanStillChecksWorkspace(t *testing.T) {
	state, s := newInterruptedTurnState(t, &scriptedStreamLLM{}, nil)
	ps := s.(sessions.PlanSession)
	ctx := t.Context()
	if err := ps.StartPlan(ctx, "unused.md", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	const draft = "## Steps\n- Verify result\n"
	if started, err := ps.AcceptPlan(ctx, sessions.PlanRevision(draft), draft); !started || err != nil {
		t.Fatalf("accept=%v, %v", started, err)
	}
	turn := &turnExecution{ctx: ctx, state: state}
	if release, err := turn.bindPlan(); err == nil {
		release()
		t.Fatal("uninitialized plan ran in another workspace")
	}
}
