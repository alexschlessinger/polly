package sessions

import (
	"context"
	"database/sql"
	"reflect"
	"strings"
	"testing"
)

func mustGetTodos(t *testing.T, session TodoSession) []PlanTodo {
	t.Helper()
	got, err := session.GetTodos(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestOrdinaryTodosNeedNoPlanOrConversation(t *testing.T) {
	ctx := context.Background()
	store, path := openTestStore(t, ModeDisk, nil, 0)
	s, err := store.Acquire(ctx, "work", AcquireOptions{Auto: true})
	if err != nil {
		t.Fatal(err)
	}
	todos := s.(TodoSession)
	if err := todos.InitPlanTodos(ctx, ""); err == nil {
		t.Fatal("initialized without a plan")
	}
	for _, text := range []string{"", "  ", "a\nb", "a\x00b", "\xff", strings.Repeat("x", 4097)} {
		if err := todos.AddTodo(ctx, text); err == nil {
			t.Fatal("invalid title accepted")
		}
	}
	if err := todos.AddTodo(ctx, "  Investigate\t"); err != nil {
		t.Fatal(err)
	}
	if err := todos.AddTodo(ctx, "Follow up"); err != nil {
		t.Fatal(err)
	}
	if err := todos.SetTodoState(ctx, 1, TodoActive, ""); err != nil {
		t.Fatal(err)
	}
	if err := todos.SetTodoState(ctx, 1, TodoBlocked, "waiting for review"); err != nil {
		t.Fatal(err)
	}
	if err := todos.AddTodo(ctx, "Investigate"); err != nil {
		t.Fatal(err)
	}
	if err := todos.RenameTodo(ctx, 2, "Investigate"); err == nil {
		t.Fatal("renamed two ordinary todos to the same title")
	}
	if err := todos.RenameTodo(ctx, 1, "Request feedback"); err != nil {
		t.Fatal(err)
	}
	want := []PlanTodo{{1, "Request feedback", TodoBlocked, "waiting for review", 0}, {2, "Follow up", TodoPending, "", 0}}
	if got := mustGetTodos(t, todos); !reflect.DeepEqual(got, want) {
		t.Fatalf("todos = %+v, want %+v", got, want)
	}
	if p := mustGetPlan(t, s.(PlanSession)); p != nil {
		t.Fatalf("ordinary todos created a plan: %+v", p)
	}
	if err := s.Clear(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := todos.AddTodo(ctx, "too late"); err == nil {
		t.Fatal("closed handle still modifies todos")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(StoreConfig{Mode: ModeDisk, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s, err = store.Acquire(ctx, "work", AcquireOptions{ExistingOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := mustGetTodos(t, s.(TodoSession)); !reflect.DeepEqual(got, want) {
		t.Fatalf("reopened todos = %+v, want %+v", got, want)
	}
}

func TestPlanTodoInitializationIsAtomicAndIndependent(t *testing.T) {
	ctx := context.Background()
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	s := acquireNamed(t, store, "mixed")
	p, todos := s.(PlanSession), s.(TodoSession)
	if err := todos.AddTodo(ctx, "Ship it"); err != nil {
		t.Fatal(err)
	}
	if err := todos.SetTodoState(ctx, 1, TodoActive, ""); err != nil {
		t.Fatal(err)
	}
	if err := p.StartPlan(ctx, "draft.md", ""); err != nil {
		t.Fatal(err)
	}
	rev := PlanRevision(samplePlan)
	if err := todos.InitPlanTodos(ctx, rev); err == nil {
		t.Fatal("initialized before acceptance")
	}
	if _, err := store.db.Exec(`CREATE TRIGGER fail_second BEFORE INSERT ON session_todos WHEN NEW.plan_step=2 BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	if start, err := p.AcceptPlan(ctx, rev, samplePlan); !start || err != nil {
		t.Fatalf("acceptance with todo insert disabled: %v, %v", start, err)
	}
	if err := todos.InitPlanTodos(ctx, rev); err == nil {
		t.Fatal("initialization ignored injected error")
	}
	ordinary := PlanTodo{1, "Ship it", TodoActive, "", 0}
	if got := mustGetTodos(t, todos); !reflect.DeepEqual(got, []PlanTodo{ordinary}) {
		t.Fatalf("partial initialization: %+v", got)
	}
	if _, err := store.db.Exec(`DROP TRIGGER fail_second`); err != nil {
		t.Fatal(err)
	}
	if err := todos.InitPlanTodos(ctx, rev); err != nil {
		t.Fatal(err)
	}
	if err := todos.RenameTodo(ctx, 2, "unreviewed work"); err == nil {
		t.Fatal("renamed a linked step")
	}
	if err := todos.SetTodoState(ctx, 2, TodoActive, ""); err != nil {
		t.Fatal(err)
	}
	if err := todos.SetTodoState(ctx, 2, TodoDone, ""); err != nil {
		t.Fatal(err)
	}
	if err := todos.AddTodo(ctx, "Verify it"); err != nil {
		t.Fatal(err)
	}
	if err := todos.InitPlanTodos(ctx, rev); err != nil {
		t.Fatal(err)
	}
	want := []PlanTodo{ordinary, {2, "Ship it", TodoDone, "", 1}, {3, "Verify it", TodoPending, "", 2}, {4, "Verify it", TodoPending, "", 0}}
	if got := mustGetTodos(t, todos); !reflect.DeepEqual(got, want) {
		t.Fatalf("todos = %+v, want %+v", got, want)
	}
	if got := mustGetPlan(t, p); got.Snapshot != samplePlan || !reflect.DeepEqual(got.Todos, want[1:3]) {
		t.Fatalf("plan doesn't project only frozen steps: %+v", got)
	}
}

func TestMigratePlanTodosV10(t *testing.T) {
	ctx := context.Background()
	store, path := openTestStore(t, ModeDisk, nil, 0)
	s := acquireNamed(t, store, "legacy")
	p := s.(PlanSession)
	if err := p.StartPlan(ctx, "plan.md", ""); err != nil {
		t.Fatal(err)
	}
	rev := PlanRevision(samplePlan)
	if started, err := p.AcceptPlan(ctx, rev, samplePlan); !started || err != nil {
		t.Fatalf("accept: %v, %v", started, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// Put a populated v10 todo table on disk, with no v11 schema present.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := applySchemaV10(ctx, conn); err != nil {
		t.Fatal(err)
	}
	if err := execAll(ctx, conn, `DROP TABLE session_todos`,
		`INSERT INTO plan_todos SELECT id,1,'Ship it','done','' FROM sessions WHERE name='legacy'`,
		`INSERT INTO plan_todos SELECT id,2,'Verify it','blocked','review needed' FROM sessions WHERE name='legacy'`,
		`PRAGMA user_version=10`); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = OpenStore(StoreConfig{Mode: ModeDisk, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s = acquireNamed(t, store, "legacy")
	todos := s.(TodoSession)
	want := []PlanTodo{{1, "Ship it", TodoDone, "", 1}, {2, "Verify it", TodoBlocked, "review needed", 2}}
	if got := mustGetPlan(t, s.(PlanSession)); got.Revision != rev || got.Snapshot != samplePlan || !reflect.DeepEqual(got.Todos, want) {
		t.Fatalf("migrated plan = %+v", got)
	}
	if err := todos.InitPlanTodos(ctx, rev); err != nil {
		t.Fatal(err)
	}
	if err := todos.AddTodo(ctx, "Ship it"); err != nil {
		t.Fatal(err)
	}
	want = append(want, PlanTodo{3, "Ship it", TodoPending, "", 0})
	if got := mustGetTodos(t, todos); !reflect.DeepEqual(got, want) {
		t.Fatalf("migrated todos = %+v, want %+v", got, want)
	}
}
