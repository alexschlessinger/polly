package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
)

func testDraft(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	path, err := newPlanDraftPath()
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPlanDraftShortNameAndLegacyResume(t *testing.T) {
	path := testDraft(t)
	if name := filepath.Base(path); !strings.Contains(name, "-") || len(name) > 20 {
		t.Fatalf("expected a short, readable draft filename, got %q", name)
	}
	legacy := filepath.Join(filepath.Dir(path), strings.Repeat("a", 32)+".md")
	if err := os.Rename(path, legacy); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readPlanDraft(legacy); err != nil {
		t.Fatalf("cannot reopen legacy draft: %v", err)
	}
}

func TestPlanDraftCAS(t *testing.T) {
	path := testDraft(t)
	_, rev, err := readPlanDraft(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("editor change"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := writePlanDraft(path, rev, "stale"); err == nil {
		t.Fatal("stale revision overwrote external edit")
	}
	text, rev, err := readPlanDraft(path)
	if err != nil || text != "editor change" {
		t.Fatalf("external edit lost: %q %v", text, err)
	}
	results := make(chan error, 2)
	for _, s := range []string{"winner A", "winner B"} {
		go func() { _, err := writePlanDraft(path, rev, s); results <- err }()
	}
	if (<-results == nil) == (<-results == nil) {
		t.Fatal("competing writes from one revision should have one winner")
	}
	if _, err := writePlanDraft(path, missingPlanDraftRevision, "new"); err == nil {
		t.Fatal("create clobbered existing plan")
	}
}

func TestPlanDraftNoEscapes(t *testing.T) {
	path := testDraft(t)
	victim := filepath.Join(t.TempDir(), filepath.Base(path))
	const original = "not a plan"
	if err := os.WriteFile(victim, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	rev := sessions.PlanRevision(original)
	if _, err := writePlanDraft(victim, rev, "changed"); err == nil {
		t.Fatal("wrote outside managed root")
	}
	for _, tc := range []struct {
		name string
		link func(string, string) error
	}{{"symlink", os.Symlink}, {"hardlink", os.Link}} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := tc.link(victim, path); err != nil {
				t.Skip(err)
			}
			if _, _, err := readPlanDraft(path); err == nil {
				t.Fatal("read linked target")
			}
			if _, err := writePlanDraft(path, rev, "changed"); err == nil {
				t.Fatal("updated linked target")
			}
		})
	}
	if data, err := os.ReadFile(victim); err != nil || string(data) != original {
		t.Fatalf("target changed: %q %v", data, err)
	}
}

func TestPlanDraftRefusesLinkedStorage(t *testing.T) {
	path := testDraft(t)
	plans := filepath.Dir(path)
	elsewhere := t.TempDir()
	if err := os.Rename(plans, plans+"-real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, plans); err != nil {
		t.Skip(err)
	}
	if _, err := newPlanDraftPath(); err == nil {
		t.Fatal("allocated through redirected root")
	}
	if _, err := writePlanDraft(path, missingPlanDraftRevision, "outside"); err == nil {
		t.Fatal("created through redirected root")
	}
	if entries, err := os.ReadDir(elsewhere); err != nil || len(entries) != 0 {
		t.Fatalf("redirected directory = %v: %v", entries, err)
	}
}

func TestPlanTurnPrivateDispatch(t *testing.T) {
	path := testDraft(t)
	ctx := t.Context()
	state, s := newInterruptedTurnState(t, &scriptedStreamLLM{}, nil)
	ps := s.(sessions.PlanSession)
	base, err := planWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	if err := ps.StartPlan(ctx, path, base); err != nil {
		t.Fatal(err)
	}
	spoof := func(name string) {
		state.toolRegistry.Register(&tools.Func{Name: name, ReadOnly: true, Run: func(context.Context, tools.Args) (string, error) {
			t.Fatalf("ran spoofed tool %s", name)
			return "", nil
		}})
		state.toolRegistry.MarkBuiltin(name)
	}
	for _, name := range []string{"read_file", "bash", "start_worker", "restore_artifact", "update_todos", "write_plan_draft"} {
		spoof(name)
	}
	state.toolRegistry.Register(tools.NewListDirTool(state.toolRegistry))
	turn := &turnExecution{ctx: ctx, state: state, turnUI: newLineTurnUI(&Config{}, nil)}
	release, err := turn.bindPlan()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	tools := turn.runAgent.ToolRegistry()
	for _, name := range []string{"read_file", "bash", "start_worker", "restore_artifact", "update_todos"} {
		if _, ok := tools.Get(name); ok {
			t.Fatalf("forged %s dispatchable", name)
		}
	}
	if _, ok := tools.Get("list_dir"); !ok {
		t.Fatal("genuine reader not available")
	}
	r, _ := tools.Get("read_plan_draft")
	w, _ := tools.Get("write_plan_draft")
	args := map[string]any{"revision": sessions.PlanRevision(""), "text": "## Steps\n- Verify output\n"}
	if _, err := w.Execute(ctx, args); err == nil {
		t.Fatal("write before read succeeded")
	}
	if _, err := r.Execute(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Execute(ctx, args); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Execute(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := ps.ExitPlan(ctx); err != nil {
		t.Fatal(err)
	}
	args["revision"] = sessions.PlanRevision(args["text"].(string))
	if _, err := w.Execute(ctx, args); err == nil {
		t.Fatal("stale planning handle remained writable")
	}
}

func TestPlanExecutionUpdatesOnlyAcceptedRecord(t *testing.T) {
	path := testDraft(t)
	state, s := newInterruptedTurnState(t, &scriptedStreamLLM{}, nil)
	ctx := t.Context()
	ps := s.(sessions.PlanSession)
	snapshot := "## Steps\n- Verify A\n"
	base, err := planWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	if err := ps.StartPlan(ctx, path, base); err != nil {
		t.Fatal(err)
	}
	if started, err := ps.AcceptPlan(ctx, sessions.PlanRevision(snapshot), snapshot); err != nil || !started {
		t.Fatalf("accept: %v %v", started, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	turn := &turnExecution{ctx: ctx, state: state, turnUI: newLineTurnUI(&Config{}, nil)}
	release, err := turn.bindPlan()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	update, ok := turn.runAgent.ToolRegistry().Get(todoToolName)
	if !ok || !strings.Contains(turn.planContract(), snapshot) || len(turn.todos) != 0 {
		t.Fatal("expected accepted snapshot, no implicit todo initialization")
	}
	if _, ok := state.toolRegistry.Get(todoToolName); ok {
		t.Fatal("progress leaked into source registry")
	}
	if _, err := update.Execute(ctx, map[string]any{"operation": "init_plan"}); err == nil {
		t.Fatal("unbound run initialized plan steps")
	}
	bound := context.WithValue(ctx, todoTurnKey{}, turn)
	for _, args := range []map[string]any{{"operation": "init_plan"}, {"operation": "status", "number": 1, "state": "active"}, {"operation": "status", "number": 1, "state": "blocked", "reason": "missing fixture"}, {"operation": "init_plan"}} {
		if _, err := update.Execute(bound, args); err != nil {
			t.Fatal(err)
		}
	}
	p, err := ps.GetPlan(ctx)
	if err != nil || p.Todos[0].Reason != "missing fixture" || p.Snapshot != snapshot {
		t.Fatalf("unexpected progress: %+v %v", p, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("progress wrote the document: %v", err)
	}
}
