package main

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
)

func acceptPlanForReadTest(t *testing.T, session sessions.Session, snapshot string) string {
	t.Helper()
	ps := session.(sessions.PlanSession)
	base, err := planWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	// An accepted plan can be read without its former draft on disk.
	if err := ps.StartPlan(t.Context(), "nonexistent.md", base); err != nil {
		t.Fatal(err)
	}
	revision := sessions.PlanRevision(snapshot)
	if started, err := ps.AcceptPlan(t.Context(), revision, snapshot); err != nil || !started {
		t.Fatalf("accept = %v, %v", started, err)
	}
	return revision
}

func bindPlanForReadTest(t *testing.T, state *conversationState) (*turnExecution, func()) {
	t.Helper()
	turn := &turnExecution{ctx: t.Context(), state: state}
	release, err := turn.bindPlan()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	return turn, release
}

func TestLargeAcceptedPlanRequestAndReadOnlyDispatch(t *testing.T) {
	const firstLine = "# Exact approved heading\r\n"
	snapshot := firstLine + strings.Repeat("large document body\r\n", 30_000) + "## Steps\r\n- Verify the exact result\r\n"
	phase := 0
	calls := []messages.ChatMessageToolCall{
		{ID: "page", Name: readAcceptedPlanToolName, Arguments: `{}`},
		{ID: "inspection", Name: "inspect_config", Arguments: `{}`},
	}
	model := &compactTestLLM{reply: func(_ context.Context, req *llm.CompletionRequest) (messages.ChatMessage, error) {
		phase++
		if phase == 1 {
			prompt := req.Messages[0].Content
			if strings.Contains(prompt, firstLine) || !strings.Contains(prompt, readAcceptedPlanToolName) || !strings.Contains(prompt, "init_plan") || !strings.Contains(prompt, "only at the user's request") {
				return messages.ChatMessage{}, fmt.Errorf("wrong guidance in %d-byte system prompt", len(prompt))
			}
			if !slices.ContainsFunc(req.Tools, func(tool tools.Tool) bool {
				return tool.GetName() == readAcceptedPlanToolName && tools.ReadsOnly(tool)
			}) {
				return messages.ChatMessage{}, fmt.Errorf("accepted plan reader was not advertised as a normal read")
			}
			return messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: calls}, nil
		}
		for _, m := range req.Messages {
			if m.ToolCallID == "page" && strings.Contains(m.Content, "\n"+firstLine) && strings.Contains(m.Content, "next byte_offset=") && len(m.Content) <= 4000 {
				return messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonEndTurn, Content: "Read the approved plan."}, nil
			}
		}
		return messages.ChatMessage{}, fmt.Errorf("provider did not receive a bounded raw plan page")
	}}
	inspected := false
	state, session := newInterruptedTurnState(t, model, &tools.Func{Name: "inspect_config", ReadOnly: true, Run: func(ctx context.Context, _ tools.Args) (string, error) {
		if ctx.Value(acceptedPlanReadKey{}) != nil {
			return "", fmt.Errorf("accepted plan read authority leaked to another call")
		}
		inspected = true
		return "normal read", nil
	}})
	state.settings.MaxHistoryTokens = 8_192
	state.settings.SystemPrompt = "Test"
	revision := acceptPlanForReadTest(t, session, snapshot)
	code, err := executeTurnWithUserMessage(t.Context(), &Config{}, state, messages.ChatMessage{
		Role: messages.MessageRoleUser, Content: "Read the approved plan before proceeding.",
	}, nil, nil, &collectingTurnUI{}, false)
	if err != nil || code != 0 || phase != 2 || !inspected {
		t.Fatalf("accepted turn: code=%d calls=%d inspected=%t err=%v", code, phase, inspected, err)
	}
	p, err := session.(sessions.PlanSession).GetPlan(t.Context())
	if err != nil || p == nil || p.Snapshot != snapshot || p.Revision != revision || len(p.Todos) != 0 {
		t.Fatalf("reading changed the approved plan or its progress: %v", err)
	}
}

func TestAcceptedPlanReadPagesRecoverExactBytes(t *testing.T) {
	state, session := newInterruptedTurnState(t, &scriptedStreamLLM{}, nil)
	// Include a BOM, CRLF, a long physical line and no final newline.
	snapshot := "\uFEFF# Exact approval\r\n" + strings.Repeat("π界🙂\t ", 1000) + "\r\n\r\n## Steps\r\n- Verify output"
	revision := acceptPlanForReadTest(t, session, snapshot)
	// PlanSession is independent of TodoSession. This fallback contract must
	// also bound large accepted snapshots and make their contents available.
	state.session = struct {
		sessions.Session
		sessions.PlanSession
	}{Session: session, PlanSession: session.(sessions.PlanSession)}
	turn, _ := bindPlanForReadTest(t, state)
	if got := turn.planContract(); len(got) > 512 || !strings.Contains(got, readAcceptedPlanToolName) || !strings.Contains(got, revision) {
		t.Fatalf("large fallback contract is %d bytes or lacks reader guidance", len(got))
	}
	reader, ok := turn.runAgent.ToolRegistry().Get(readAcceptedPlanToolName)
	if !ok {
		t.Fatal("missing reader without TodoSession")
	}
	const capBytes = 513
	ctx := turn.bindAcceptedPlanRead(tools.WithPageBytes(t.Context(), capBytes), messages.ChatMessageToolCall{Name: readAcceptedPlanToolName})
	var restored strings.Builder
	for offset := 0; offset < len(snapshot); {
		page, err := reader.Execute(ctx, map[string]any{"byte_offset": float64(offset)})
		if err != nil || len(page) > capBytes || !utf8.ValidString(page) {
			t.Fatalf("page at offset %d: %d bytes, err=%v", offset, len(page), err)
		}
		header, raw, _ := strings.Cut(page, "\n")
		var start, end, size int
		if _, err := fmt.Sscanf(header, "[accepted plan "+revision+"; bytes %d-%d of %d; raw window]", &start, &end, &size); err != nil || start != offset || end <= start || end > len(snapshot) || size != len(snapshot) {
			t.Fatalf("bad page header at %d: %q (%v)", offset, header, err)
		}
		if end < size {
			var ok bool
			raw, ok = strings.CutSuffix(raw, fmt.Sprintf("\n[accepted plan continues; next byte_offset=%d]", end))
			if !ok {
				t.Fatalf("no exact continuation at %d", offset)
			}
		}
		if raw != snapshot[start:end] {
			t.Fatalf("page %d-%d modified the approved bytes", start, end)
		}
		restored.WriteString(raw)
		offset = end
	}
	if got := restored.String(); got != snapshot || sessions.PlanRevision(got) != revision {
		t.Fatal("paging did not recover the approved revision byte for byte")
	}
}

func TestAcceptedPlanReadAuthorizationAndOffsets(t *testing.T) {
	state, session := newInterruptedTurnState(t, &scriptedStreamLLM{}, nil)
	const snapshot = "🙂\n## Steps\n- Verify result\n"
	acceptPlanForReadTest(t, session, snapshot)
	turn, release := bindPlanForReadTest(t, state)
	reader, ok := turn.runAgent.ToolRegistry().Get(readAcceptedPlanToolName)
	if !ok {
		t.Fatal("missing accepted reader")
	}
	if _, ok := state.toolRegistry.Get(readAcceptedPlanToolName); ok {
		t.Fatal("accepted reader leaked into the worker tool source")
	}
	call := messages.ChatMessageToolCall{Name: readAcceptedPlanToolName}
	ctx := t.Context()
	bound := turn.bindAcceptedPlanRead(ctx, call)
	other, otherSession := newInterruptedTurnState(t, &scriptedStreamLLM{}, nil)
	ordinary, _ := bindPlanForReadTest(t, other)
	if _, ok := ordinary.runAgent.ToolRegistry().Get(readAcceptedPlanToolName); ok {
		t.Fatal("ordinary turn advertised the accepted reader")
	}
	if err := otherSession.(sessions.PlanSession).StartPlan(ctx, "nonexistent.md", turn.plan.Base); err != nil {
		t.Fatal(err)
	}
	draft, _ := bindPlanForReadTest(t, other)
	if _, ok := draft.runAgent.ToolRegistry().Get(readAcceptedPlanToolName); ok {
		t.Fatal("draft turn advertised the accepted reader")
	}
	acceptPlanForReadTest(t, otherSession, "## Steps\n- Other session\n")
	otherTurn, _ := bindPlanForReadTest(t, other)
	for _, wrong := range []context.Context{
		ctx,
		withToolCall(ctx, call),
		turn.bindAcceptedPlanRead(ctx, messages.ChatMessageToolCall{Name: "start_worker"}),
		turn.bindAcceptedPlanRead(withAskingMember(ctx, "worker"), call),
		withAskingMember(bound, "worker"),
		ordinary.bindAcceptedPlanRead(ctx, call),
		draft.bindAcceptedPlanRead(ctx, call),
		otherTurn.bindAcceptedPlanRead(ctx, call),
	} {
		if got, err := reader.Execute(wrong, nil); err == nil || got != "" {
			t.Fatal("unbound, worker or cross-session call read an approved plan")
		}
	}
	for _, invalid := range []map[string]any{
		{"session": "other"}, {"path": "nonexistent.md"}, {"revision": "arbitrary"},
		{"byte_offset": 0, "limit": 1}, {"byte_offset": nil}, {"byte_offset": "0"},
		{"byte_offset": -1}, {"byte_offset": 0.5}, {"byte_offset": math.Inf(1)}, {"byte_offset": int64(math.MaxInt64)},
		{"byte_offset": 1}, {"byte_offset": len(snapshot) + 1},
	} {
		if got, err := reader.Execute(bound, invalid); err == nil || got != "" {
			t.Fatalf("invalid argument accepted: %v", invalid)
		}
	}
	if got, err := reader.Execute(tools.WithPageBytes(bound, 10), nil); err == nil || got != "" {
		t.Fatal("a tiny page cap returned a clipped, non-advancing page")
	}
	if got, err := reader.Execute(bound, nil); err != nil || !strings.HasSuffix(got, "\n"+snapshot) {
		t.Fatalf("default offset: %q, %v", got, err)
	}
	if got, err := reader.Execute(bound, map[string]any{"byte_offset": len(snapshot)}); err != nil || strings.Count(got, "\n") != 1 || strings.Contains(got, "next byte_offset=") {
		t.Fatalf("EOF: %q, %v", got, err)
	}
	release()
	if got, err := reader.Execute(context.WithoutCancel(bound), nil); err == nil || got != "" {
		t.Fatal("retained dispatch context still reads after release")
	}
	if _, ok := turn.runAgent.ToolRegistry().Get(readAcceptedPlanToolName); ok {
		t.Fatal("accepted reader remained advertised after release")
	}

	todos := session.(sessions.TodoSession)
	if err := todos.InitPlanTodos(ctx, turn.plan.Revision); err != nil {
		t.Fatal(err)
	}
	for _, status := range []sessions.TodoState{sessions.TodoActive, sessions.TodoDone} {
		if err := todos.SetTodoState(ctx, 1, status, ""); err != nil {
			t.Fatal(err)
		}
	}
	completed, _ := bindPlanForReadTest(t, state)
	if strings.Contains(completed.planContract(), "Accepted Markdown") {
		t.Fatal("completed plan injected implementation guidance")
	}
	current, ok := completed.runAgent.ToolRegistry().Get(readAcceptedPlanToolName)
	if !ok {
		t.Fatal("cannot reference a completed plan in a later turn")
	}
	bound = completed.bindAcceptedPlanRead(ctx, call)
	if got, err := reader.Execute(bound, nil); err == nil || got != "" {
		t.Fatal("a later turn's capability authorized an old reader")
	}
	if got, err := current.Execute(bound, nil); err != nil || !strings.HasSuffix(got, "\n"+snapshot) {
		t.Fatalf("read completed plan: %q, %v", got, err)
	}
}
