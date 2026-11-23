package main

import (
	"bufio"
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/sessions"
)

func TestLinePlanResumeIsDisplayOnly(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	state := &conversationState{session: testAcquireSession(t, store, "line-plan")}
	s := state.session.(sessions.PlanSession)
	ctx := context.Background()
	const path = "/private/path/draft.md"
	if err := s.StartPlan(ctx, path, "test"); err != nil {
		t.Fatal(err)
	}
	run := func(input string) string {
		t.Helper()
		var out bytes.Buffer
		command := newWriterReplCommandContext(&Config{}, state, &out)
		if err := runREPLLoopWithCommands(ctx, bufio.NewReader(strings.NewReader(input)), &out, command, func(string) error {
			t.Fatal("merely reopening a plan or asking for todos started a turn")
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	if out := run("/exit\n"); !strings.Contains(out, path) || !strings.Contains(out, "PLAN> ") {
		t.Fatalf("resumed draft prompt: %q", out)
	}
	const text = "# Test\n\n## Steps\n- Match titles\n- Verify results\n"
	rev := sessions.PlanRevision(text)
	if started, err := s.AcceptPlan(ctx, rev, text); err != nil || !started {
		t.Fatalf("accept plan: started=%v err=%v", started, err)
	}
	todos := state.session.(sessions.TodoSession)
	if out := run("/todo\n/exit\n"); !strings.Contains(out, "checklist hasn’t been started") {
		t.Fatal(out)
	}
	if rows, err := todos.GetTodos(ctx); err != nil || len(rows) != 0 {
		t.Fatalf("view initialized todos: %+v %v", rows, err)
	}
	if err := todos.InitPlanTodos(ctx, rev); err != nil {
		t.Fatal(err)
	}
	for _, u := range []struct {
		num    int
		state  sessions.TodoState
		reason string
	}{{1, sessions.TodoActive, ""}, {1, sessions.TodoDone, ""}, {2, sessions.TodoActive, ""}, {2, sessions.TodoBlocked, "ordering failed"}} {
		if err := todos.SetTodoState(ctx, u.num, u.state, u.reason); err != nil {
			t.Fatal(err)
		}
	}
	out := run("/todo\n/exit\n")
	for _, want := range []string{"TODO 1/2 done", "Blocked: ordering failed — Verify results", "ordering failed", "Match titles", "/todo"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q from %q", want, out)
		}
	}
	wantChecklist := strings.Join([]string{
		"1/2 done",
		"Approved outcomes",
		"✓ #1  Done · Match titles (step 1)",
		"! #2  Blocked · Verify results (step 2)",
		"    Reason: ordering failed",
		"View accepted plan (read-only): /plan view",
	}, "\n")
	if !strings.Contains(out, wantChecklist) {
		t.Fatalf("line-mode checklist changed: %q", out)
	}
	if strings.Contains(out, "PLAN> ") || strings.Contains(out, path) {
		t.Fatalf("accepted plan advertised editable draft: %q", out)
	}
}

func TestLinePlanProgressUsesStderr(t *testing.T) {
	var output, status bytes.Buffer
	ui := newLineTurnUI(&Config{}, nil)
	ui.interactive = true
	ui.writer, ui.errWriter = &output, &status
	items := []sessions.PlanTodo{{Number: 1, State: sessions.TodoActive, Text: "Check results"}}
	ui.TodoProgress(nil, items)
	if output.Len() != 0 || !strings.Contains(status.String(), "TODO 0/1 done · Active: Check results") {
		t.Fatalf("output=%q status=%q", output.String(), status.String())
	}
	status.Reset()
	ui.config.Quiet = true
	ui.TodoProgress(nil, items)
	if status.Len() != 0 {
		t.Fatalf("quiet progress produced %q", status.String())
	}
}
