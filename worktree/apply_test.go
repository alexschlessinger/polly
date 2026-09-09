package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestApplyPlanReconcileAndPathPreconditions(t *testing.T) {
	m, root := fixture(t)
	ctx := context.Background()
	base, err := m.Capture(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	copy, err := m.Create(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	writeTest(t, filepath.Join(copy.Path, "a.txt"), "new\n")
	writeTest(t, filepath.Join(copy.Path, "added.txt"), "new\n")
	snapshot, err := m.Capture(ctx, copy.Path)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := m.BuildApplyPlan(ctx, "candidate", base, snapshot, "paths")
	if err != nil {
		t.Fatal(err)
	}
	check := func(want string) {
		t.Helper()
		got, err := m.ReconcileApply(ctx, plan)
		if err != nil || got != want {
			t.Fatalf("reconcile=%s %v want %s", got, err, want)
		}
	}
	check("not_applied")
	writeTest(t, filepath.Join(root, "unrelated.txt"), "preserve\n")
	if _, err := m.PreflightApply(ctx, plan); err != nil {
		t.Fatal(err)
	}
	strict := plan
	strict.Drift = "tree"
	if _, err := m.PreflightApply(ctx, strict); !errors.Is(err, ErrParentChanged) {
		t.Fatal("strict drift accepted", err)
	}
	writeTest(t, filepath.Join(root, "added.txt"), "new\n")
	check("recovery_required")
	if _, err := m.PreflightApply(ctx, plan); !errors.Is(err, ErrParentChanged) {
		t.Fatal("path drift accepted", err)
	}
	if err := os.Remove(filepath.Join(root, "added.txt")); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteApply(ctx, plan); err != nil {
		t.Fatal(err)
	}
	check("applied")
	if data, _ := os.ReadFile(filepath.Join(root, "unrelated.txt")); string(data) != "preserve\n" {
		t.Fatal("unrelated file lost")
	}
	empty, err := m.BuildApplyPlan(ctx, "empty", base, base, "paths")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.WriteApply(ctx, empty); err != nil {
		t.Fatal(err)
	}
	if status, err := m.ReconcileApply(ctx, empty); err != nil || status != "applied" {
		t.Fatal(status, err)
	}
}

func TestApplyRejectsAncestorSymlinkAndDirectoryCollision(t *testing.T) {
	m, root := fixture(t)
	ctx := context.Background()
	base, err := m.Capture(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	c, err := m.Create(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(c.Path, "new"), 0700); err != nil {
		t.Fatal(err)
	}
	writeTest(t, filepath.Join(c.Path, "new", "file"), "new\n")
	merged, err := m.Capture(ctx, c.Path)
	if err != nil {
		t.Fatal(err)
	}
	p, err := m.BuildApplyPlan(ctx, "new", base, merged, "paths")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "new")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.PreflightApply(ctx, p); !errors.Is(err, ErrParentChanged) {
		t.Fatal("ancestor symlink accepted", err)
	}
	if err := os.Remove(filepath.Join(root, "new")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "new", "file"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := m.PreflightApply(ctx, p); !errors.Is(err, ErrParentChanged) {
		t.Fatal("directory collision accepted", err)
	}
}
