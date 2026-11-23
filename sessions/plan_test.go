package sessions

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

const samplePlan = "# Plan\r\n\r\n## Steps\r\n- [x] Ship it\r\n- Verify it\r\n\r\n## Notes\r\nKeep this exactly.\r\n"

func mustGetPlan(t *testing.T, s PlanSession) *Plan {
	t.Helper()
	p, err := s.GetPlan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPlanDurabilityAndCutoff(t *testing.T) {
	ctx := context.Background()
	store, path := openTestStore(t, ModeDisk, nil, 0)
	s, err := store.Acquire(ctx, "auto", AcquireOptions{Auto: true})
	if err != nil {
		t.Fatal(err)
	}
	ps := s.(PlanSession)
	if p := mustGetPlan(t, ps); p != nil {
		t.Fatalf("new session already has a plan: %+v", p)
	}
	if err := ps.StartPlan(ctx, "/draft/plan.md", "/base"); err != nil {
		t.Fatal(err)
	}
	// No messages yet: this automatic session still has useful state.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = store.Acquire(ctx, "auto", AcquireOptions{ExistingOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	ps = s.(PlanSession)
	wantDraft := &Plan{Mode: PlanDraft, Path: "/draft/plan.md", Base: "/base"}
	if got := mustGetPlan(t, ps); !reflect.DeepEqual(got, wantDraft) {
		t.Fatalf("resumed draft = %+v", got)
	}
	if err := ps.StartPlan(ctx, "/other", "/base"); err == nil {
		t.Fatal("changed draft path")
	}
	if err := ps.ExitPlan(ctx); err != nil {
		t.Fatal(err)
	}
	if got := mustGetPlan(t, ps); got.Mode != PlanExited || got.Path != wantDraft.Path {
		t.Fatalf("exited plan = %+v", got)
	}
	rev := PlanRevision(samplePlan)
	if start, err := ps.AcceptPlan(ctx, rev, samplePlan); start || err == nil {
		t.Fatalf("accepted while exited: %v, %v", start, err)
	}
	if err := ps.StartPlan(ctx, wantDraft.Path, wantDraft.Base); err != nil {
		t.Fatal(err)
	}
	if start, err := ps.AcceptPlan(ctx, rev, samplePlan+"\n"); start || !errors.Is(err, ErrPlanRevision) {
		t.Fatalf("accepted stale review: %v, %v", start, err)
	}
	if got := mustGetPlan(t, ps); !reflect.DeepEqual(got, wantDraft) {
		t.Fatalf("failed acceptance changed draft: %+v", got)
	}
	if start, err := ps.AcceptPlan(ctx, rev, samplePlan); !start || err != nil {
		t.Fatalf("first acceptance: %v, %v", start, err)
	}
	want := &Plan{Mode: PlanAccepted, Path: wantDraft.Path, Base: wantDraft.Base, Revision: rev, Snapshot: samplePlan}
	if got := mustGetPlan(t, ps); !reflect.DeepEqual(got, want) {
		t.Fatalf("accepted plan = %+v, want %+v", got, want)
	}
	if err := ps.StartPlan(ctx, want.Path, want.Base); err == nil {
		t.Fatal("reopened a sealed plan")
	}
	if err := ps.ExitPlan(ctx); err == nil {
		t.Fatal("exited a sealed plan")
	}
	changed := samplePlan + "Changed after acceptance."
	if start, err := ps.AcceptPlan(ctx, PlanRevision(changed), changed); start || !errors.Is(err, ErrPlanRevision) {
		t.Fatalf("accepted replacement: %v, %v", start, err)
	}
	todos := s.(TodoSession)
	if err := todos.InitPlanTodos(ctx, rev); err != nil {
		t.Fatal(err)
	}
	want.Todos = []PlanTodo{{1, "Ship it", TodoPending, "", 1}, {2, "Verify it", TodoPending, "", 2}}
	if got := mustGetPlan(t, ps); !reflect.DeepEqual(got, want) {
		t.Fatalf("initialized plan = %+v, want %+v", got, want)
	}
	for _, update := range []PlanTodo{{Number: 1, State: TodoActive}, {Number: 1, State: TodoDone}, {Number: 2, State: TodoActive}, {Number: 2, State: TodoBlocked, Reason: "check failed"}} {
		if err := todos.SetTodoState(ctx, update.Number, update.State, update.Reason); err != nil {
			t.Fatal(err)
		}
		want.Todos[update.Number-1].State = update.State
		want.Todos[update.Number-1].Reason = update.Reason
	}
	if start, err := ps.AcceptPlan(ctx, rev, samplePlan); start || err != nil {
		t.Fatalf("duplicate acceptance: %v, %v", start, err)
	}
	if err := s.Clear(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Rename(ctx, "renamed"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(StoreConfig{Mode: ModeDisk, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s = acquireNamed(t, store, "renamed")
	if got := mustGetPlan(t, s.(PlanSession)); !reflect.DeepEqual(got, want) {
		t.Fatalf("reopened plan = %+v; want %+v", got, want)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "renamed"); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"session_plans", "session_todos"} {
		var count int
		if err := store.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s count = %d, %v", table, count, err)
		}
	}
	if got := mustGetPlan(t, acquireNamed(t, store, "renamed").(PlanSession)); got != nil {
		t.Fatalf("name reuse inherited plan: %+v", got)
	}
}

func TestPlanConcurrentAcceptanceAndProgress(t *testing.T) {
	ctx := context.Background()
	store, _ := openTestStore(t, ModeDisk, nil, 0)
	p := acquireNamed(t, store, "plan").(PlanSession)
	if err := p.StartPlan(ctx, "draft.md", ""); err != nil {
		t.Fatal(err)
	}
	rev := PlanRevision(samplePlan)
	type result struct {
		start bool
		err   error
	}
	gate := make(chan struct{})
	results := make(chan result, 16)
	for range cap(results) {
		go func() { <-gate; start, err := p.AcceptPlan(ctx, rev, samplePlan); results <- result{start, err} }()
	}
	close(gate)
	starts := 0
	for range cap(results) {
		r := <-results
		if r.err != nil {
			t.Fatal(r.err)
		}
		if r.start {
			starts++
		}
	}
	if starts != 1 {
		t.Fatalf("authorized starts = %d, want exactly one", starts)
	}
	todos := p.(TodoSession)
	if err := todos.InitPlanTodos(ctx, "stale"); !errors.Is(err, ErrPlanRevision) {
		t.Fatalf("stale initialization: %v", err)
	}
	if err := todos.InitPlanTodos(ctx, rev); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		n      int
		state  TodoState
		reason string
	}{
		{1, TodoDone, ""}, {1, TodoBlocked, "reason"}, {1, TodoActive, "spurious"}, {1, "skipped", ""}, {3, TodoActive, ""},
	} {
		if err := todos.SetTodoState(ctx, tc.n, tc.state, tc.reason); err == nil {
			t.Fatalf("accepted invalid update: %+v", tc)
		}
	}
	if err := todos.SetTodoState(ctx, 1, TodoActive, ""); err != nil {
		t.Fatal(err)
	}
	if err := todos.SetTodoState(ctx, 1, TodoBlocked, "   "); err == nil {
		t.Fatal("blocked with blank reason")
	}
	// Two competing reports from active: only one may win. A lost race
	// cannot overwrite done, or silently complete a blocked step.
	done := make(chan error, 2)
	go func() { done <- todos.SetTodoState(ctx, 1, TodoDone, "") }()
	go func() { done <- todos.SetTodoState(ctx, 1, TodoBlocked, "test failed") }()
	a, b := <-done, <-done
	if (a == nil) == (b == nil) {
		t.Fatalf("competing updates: %v / %v", a, b)
	}
	if got := mustGetPlan(t, p).Todos[0]; got.State == TodoBlocked {
		if got.Reason != "test failed" {
			t.Fatalf("reason lost: %+v", got)
		}
		if err := todos.SetTodoState(ctx, 1, TodoActive, ""); err != nil {
			t.Fatal(err)
		}
		if got := mustGetPlan(t, p).Todos[0]; got.Reason != "" {
			t.Fatalf("retry kept reason: %+v", got)
		}
		if err := todos.SetTodoState(ctx, 1, TodoDone, ""); err != nil {
			t.Fatal(err)
		}
	} else if got.State != TodoDone {
		t.Fatalf("unexpected winning update: %+v", got)
	}
	if err := todos.SetTodoState(ctx, 1, TodoActive, ""); err == nil {
		t.Fatal("reopened completed step")
	}
}

func TestPlanPromotesWithProgress(t *testing.T) {
	ctx := context.Background()
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	p := acquireNamed(t, store, "promoted").(PlanSession)
	todos := p.(TodoSession)
	if err := todos.AddTodo(ctx, "ordinary"); err != nil {
		t.Fatal(err)
	}
	if err := p.StartPlan(ctx, "draft.md", ""); err != nil {
		t.Fatal(err)
	}
	rev := PlanRevision(samplePlan)
	if started, err := p.AcceptPlan(ctx, rev, samplePlan); !started || err != nil {
		t.Fatalf("accept: %v, %v", started, err)
	}
	if err := todos.InitPlanTodos(ctx, rev); err != nil {
		t.Fatal(err)
	}
	if err := todos.SetTodoState(ctx, 2, TodoActive, ""); err != nil {
		t.Fatal(err)
	}
	before := mustGetPlan(t, p)
	items := mustGetTodos(t, todos)
	if err := store.Promote(ctx, filepath.Join(t.TempDir(), "sessions.db")); err != nil {
		t.Fatal(err)
	}
	if got := mustGetPlan(t, p); !reflect.DeepEqual(got, before) {
		t.Fatalf("promotion changed plan: %+v, want %+v", got, before)
	}
	if got := mustGetTodos(t, todos); !reflect.DeepEqual(got, items) {
		t.Fatalf("promotion changed todos: %+v, want %+v", got, items)
	}
}

func TestPlanAcceptanceRollback(t *testing.T) {
	ctx := context.Background()
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	p := acquireNamed(t, store, "rollback").(PlanSession)
	if err := p.StartPlan(ctx, "draft.md", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER fail_accept BEFORE UPDATE OF mode ON session_plans WHEN NEW.mode='accepted' BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	if start, err := p.AcceptPlan(ctx, PlanRevision(samplePlan), samplePlan); start || err == nil {
		t.Fatalf("failed commit authorized a start: %v, %v", start, err)
	}
	if got := mustGetPlan(t, p); got.Mode != PlanDraft || len(got.Todos) != 0 || got.Snapshot != "" {
		t.Fatalf("failed acceptance left partial state: %+v", got)
	}
	if _, err := store.db.Exec(`DROP TRIGGER fail_accept`); err != nil {
		t.Fatal(err)
	}
	if start, err := p.AcceptPlan(ctx, PlanRevision(samplePlan), samplePlan); !start || err != nil {
		t.Fatalf("retry after rollback: %v, %v", start, err)
	}
}
