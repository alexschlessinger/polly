package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

func planCommandFixture(t *testing.T) (*replCommandContext, func(string) string, *[]string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	state := &conversationState{session: testAcquireSession(t, testOpenMemoryStore(t, nil), "plan-commands")}
	var turns []string
	c := &replCommandContext{state: state, startTurn: func(_ string, msg messages.ChatMessage) error { turns = append(turns, msg.GetContent()); return nil }}
	return c, func(line string) string {
		t.Helper()
		return strings.Join(dispatchDefaultCommandForTest(t, line, c), "\n")
	}, &turns
}
func mustReadCommandPlan(t *testing.T, c *replCommandContext) *sessions.Plan {
	t.Helper()
	p, e := currentPlan(context.Background(), c.state)
	if e != nil || p == nil {
		t.Fatalf("plan: %+v %v", p, e)
	}
	return p
}
func editPlanInTest(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestPlanCommandsReviewCutoffAndResume(t *testing.T) {
	c, run, turns := planCommandFixture(t)
	got := run("/plan rank  exact matches first")
	p := mustReadCommandPlan(t, c)
	if p.Mode != sessions.PlanDraft || len(*turns) != 1 || (*turns)[0] != "rank  exact matches first" || !strings.Contains(got, p.Path) || strings.Contains(got, "/plan edit") {
		t.Fatalf("start: %q %+v %v", got, p, *turns)
	}
	opened := 0
	c.openPlanEditor = func(path string) error {
		opened++
		if path != p.Path {
			t.Fatalf("editor path: %q", path)
		}
		return nil
	}
	run("/plan edit")
	first := "# Design\nNotes A\n## Steps\n- Verify ranking\n"
	editPlanInTest(t, p.Path, first)
	if out := run("/plan review"); !strings.Contains(out, "Verify ranking") || !strings.Contains(out, sessions.PlanRevision(first)[:12]) {
		t.Fatal(out)
	}
	// Even a prose-only change invalidates approval.
	second := strings.Replace(first, "Notes A", "Notes B", 1)
	editPlanInTest(t, p.Path, second)
	if out := run("/plan accept"); !strings.Contains(out, "changed since review") || !strings.Contains(out, "Notes B") || len(*turns) != 1 || mustReadCommandPlan(t, c).Mode != sessions.PlanDraft {
		t.Fatalf("stale review: %q, %v", out, *turns)
	}
	run("/plan accept")
	run("/plan accept")
	run("/plan edit")
	run("/plan another goal")
	p = mustReadCommandPlan(t, c)
	if p.Mode != sessions.PlanAccepted || p.Snapshot != second || len(*turns) != 2 || opened != 1 || len(p.Todos) != 0 {
		t.Fatalf("accept: %+v, turns %v, editor opened %d times", p, *turns, opened)
	}
	editPlanInTest(t, p.Path, "## Steps\n- Late editor save\n")
	if err := os.Remove(p.Path); err != nil {
		t.Fatal(err)
	}
	// A fresh frontend on the accepted session can inspect or explicitly
	// continue, without importing or even needing the old draft.
	c.state = &conversationState{session: c.state.session}
	if out := run("/plan view"); !strings.Contains(out, "Notes B") || strings.Contains(out, "Late editor") {
		t.Fatal(out)
	}
	if out := run("/todo"); !strings.Contains(out, "hasn’t been started") || !strings.Contains(out, "/plan view") {
		t.Fatal(out)
	}
	if len(*turns) != 2 {
		t.Fatalf("view scheduled work: %v", *turns)
	}
	run("/plan continue") // an accepted but uninitialized plan is not complete
	if len(*turns) != 3 {
		t.Fatalf("could not continue uninitialized plan: %v", *turns)
	}
	if err := c.state.session.(sessions.TodoSession).InitPlanTodos(context.Background(), p.Revision); err != nil {
		t.Fatal(err)
	}
	if out := run("/todo"); !strings.Contains(out, "Pending · Verify ranking (step 1)") || !strings.Contains(out, "/plan view") {
		t.Fatal(out)
	}
	if len(*turns) != 3 {
		t.Fatalf("unexpected restart: %v", *turns)
	}
	run("/plan continue")
	if len(*turns) != 4 || !strings.Contains((*turns)[3], "do not replay") {
		t.Fatalf("continue: %v", *turns)
	}
}

func TestPlanCommandsBusyExitAndFreshReview(t *testing.T) {
	c, run, turns := planCommandFixture(t)
	c.state.planWork.Add(1)
	if out := run("/plan goal"); !strings.Contains(out, "still running") {
		t.Fatal(out)
	}
	c.state.planWork.Add(-1)
	if p, _ := currentPlan(context.Background(), c.state); p != nil {
		t.Fatal(p)
	}
	run("/plan")
	p := mustReadCommandPlan(t, c)
	editPlanInTest(t, p.Path, "## Steps\n- Deliver result\n")
	for _, line := range []string{"/spawn inspect repository", "/workflow write.js input.json", "/sandbox try touch new", "/sandbox-init", "/add-dir /tmp", "/tools restart server", "/screenshot project.png"} {
		if out := run(line); !strings.Contains(out, "disabled while planning") {
			t.Fatalf("%s: %s", line, out)
		}
	}
	run("/plan review")
	c.planBusy = func() bool { return true }
	run("/plan accept")
	run("/plan exit")
	if mustReadCommandPlan(t, c).Mode != sessions.PlanDraft {
		t.Fatal("busy transition took effect")
	}
	c.planBusy = nil
	run("/plan exit")
	if p := mustReadCommandPlan(t, c); p.Mode != sessions.PlanExited {
		t.Fatal(p)
	}
	run("/plan")
	if got := mustReadCommandPlan(t, c).Path; got != p.Path {
		t.Fatalf("draft redirected to %s", got)
	}
	// A resumed draft must be shown, not accepted based on some previous UI's review.
	c.state = &conversationState{session: c.state.session}
	out := run("/plan accept")
	if !strings.Contains(out, "Deliver result") || len(*turns) != 0 || mustReadCommandPlan(t, c).Mode != sessions.PlanDraft {
		t.Fatalf("resume: %q %v", out, *turns)
	}
	editPlanInTest(t, p.Path, "## Steps\n")
	run("/plan accept")
	if c.state.planReview != "" || len(*turns) != 0 {
		t.Fatal("invalid review survived malformed edit")
	}
}

func TestTodoViewsOrdinaryAndLinkedItems(t *testing.T) {
	c, run, turns := planCommandFixture(t)
	ctx := context.Background()
	store := c.state.session.(sessions.TodoSession)
	if got := run("/todo"); !strings.Contains(got, "No todos yet") {
		t.Fatal(got)
	}
	if err := store.AddTodo(ctx, "Send notes"); err != nil {
		t.Fatal(err)
	}
	if err := store.RenameTodo(ctx, 1, "Publish notes"); err != nil {
		t.Fatal(err)
	}
	if got := run("/todo done 1"); !strings.Contains(got, "read-only") {
		t.Fatal(got)
	}
	check := func(want ...string) {
		t.Helper()
		out := run("/todo")
		for _, s := range want {
			if !strings.Contains(out, s) {
				t.Fatalf("missing %q from %q", s, out)
			}
		}
	}
	check("Session tasks", "#1  Pending · Publish notes")
	run("/plan")
	p := mustReadCommandPlan(t, c)
	check("Publish notes", p.Path, "/plan review")
	run("/plan exit")
	check("Publish notes")
	run("/plan")
	editPlanInTest(t, p.Path, "## Steps\n- Check ranking\n")
	run("/plan review")
	run("/plan accept")
	check("Publish notes", "hasn’t been started")
	if err := store.InitPlanTodos(ctx, mustReadCommandPlan(t, c).Revision); err != nil {
		t.Fatal(err)
	}
	check("0/2 done", "Session tasks", "#1  Pending · Publish notes", "Approved outcomes", "#2  Pending · Check ranking (step 1)", "/plan view")
	if err := store.SetTodoState(ctx, 2, sessions.TodoActive, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.SetTodoState(ctx, 2, sessions.TodoDone, ""); err != nil {
		t.Fatal(err)
	}
	if out := run("/plan continue"); !strings.Contains(out, "All approved outcomes are done") || len(*turns) != 1 {
		t.Fatalf("ordinary task resumed completed plan: %q %v", out, *turns)
	}
}

func TestPlanApprovalStaysInReviewedWorkspace(t *testing.T) {
	c, run, turns := planCommandFixture(t)
	run("/plan")
	editPlanInTest(t, mustReadCommandPlan(t, c).Path, "## Steps\n- Verify results\n")
	run("/plan review")
	t.Chdir(t.TempDir())
	if out := run("/plan accept"); !strings.Contains(out, "plan workspace") || len(*turns) != 0 || mustReadCommandPlan(t, c).Mode != sessions.PlanDraft {
		t.Fatalf("accepted in the wrong directory: %q %v", out, *turns)
	}
}

func TestPlanCommandsCommitBeforeDispatchFailure(t *testing.T) {
	c, run, turns := planCommandFixture(t)
	run("/plan")
	p := mustReadCommandPlan(t, c)
	editPlanInTest(t, p.Path, "## Steps\n- Do not replay this action\n")
	run("/plan review")
	attempts := 0
	c.startTurn = func(_ string, _ messages.ChatMessage) error {
		attempts++
		if mustReadCommandPlan(t, c).Mode != sessions.PlanAccepted {
			t.Fatal("turn before commit")
		}
		return errors.New("turn queue unavailable")
	}
	if out := run("/plan accept"); !strings.Contains(out, "turn queue unavailable") {
		t.Fatal(out)
	}
	run("/plan accept")
	if attempts != 1 || len(*turns) != 0 {
		t.Fatalf("retried dispatch: attempts %d", attempts)
	}
}

func TestPlanTUIBusySubmissionNeverQueues(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "plan-busy")
	m := r.model
	m.busy = true
	m.ed.setText("/plan investigate")
	if r.submitComposerLocked() || len(m.queue) > 0 {
		t.Fatalf("queued unsafe command: %+v", m.queue)
	}
	if p, _ := currentPlan(context.Background(), r.state); p != nil {
		t.Fatal(p)
	}
	m.busy = false
	m.ed.setText("/plan")
	r.submitComposerLocked()
	if p, _ := currentPlan(context.Background(), r.state); p == nil || p.Mode != sessions.PlanDraft {
		t.Fatal(p)
	}
}
