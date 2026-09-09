package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMergeBinaryConflictsAndRenameDeletionPaths(t *testing.T) {
	m, root := fixture(t)
	ctx := context.Background()
	writeTest(t, filepath.Join(root, "binary.bin"), "\x00base")
	base, err := m.Capture(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	left, err := m.Create(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	right, err := m.Create(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	writeTest(t, filepath.Join(left.Path, "binary.bin"), "\x00ours")
	writeTest(t, filepath.Join(right.Path, "binary.bin"), "\x00theirs")
	ours, err := m.Capture(ctx, left.Path)
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := m.Capture(ctx, right.Path)
	if err != nil {
		t.Fatal(err)
	}
	merged, err := m.Merge(ctx, base, ours, theirs)
	if err != nil {
		t.Fatal(err)
	}
	binary := false
	for _, c := range merged.Conflicts {
		if strings.Contains(c.Type, "binary") {
			binary = true
			if c.Base.ID != base.ID || c.Ours.ID != ours.ID || c.Theirs.ID != theirs.ID {
				t.Fatal("missing binary sources")
			}
		}
	}
	if !binary {
		t.Fatalf("missing binary conflict: %+v", merged.Conflicts)
	}
	if err := os.Rename(filepath.Join(left.Path, "a.txt"), filepath.Join(left.Path, "renamed.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(left.Path, "binary.bin")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(left.Path, "renamed.txt"), 0700); err != nil {
		t.Fatal(err)
	}
	changed, err := m.Capture(ctx, left.Path)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := m.BuildApplyPlan(ctx, "rename", base, changed, "paths")
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]PathChange{}
	for _, p := range plan.Paths {
		paths[p.Path] = p
	}
	if len(paths) != 3 || !paths["a.txt"].Before.Exists || paths["a.txt"].After.Exists || paths["renamed.txt"].Before.Exists || paths["renamed.txt"].After.Mode != "100755" || paths["binary.bin"].After.Exists {
		t.Fatalf("bad path plan: %+v", paths)
	}
	if _, err := m.PreflightApply(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteApply(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if status, err := m.ReconcileApply(ctx, plan); err != nil || status != "applied" {
		t.Fatal(status, err)
	}
}

func TestApplyIgnoredDestinationAndModeDrift(t *testing.T) {
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
	writeTest(t, filepath.Join(c.Path, "new.txt"), "candidate\n")
	writeTest(t, filepath.Join(c.Path, "a.txt"), "candidate\n")
	snapshot, err := m.Capture(ctx, c.Path)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := m.BuildApplyPlan(ctx, "ignored", base, snapshot, "paths")
	if err != nil {
		t.Fatal(err)
	}
	writeTest(t, filepath.Join(root, ".gitignore"), "new.txt\n")
	writeTest(t, filepath.Join(root, "new.txt"), "private\n")
	if _, err := m.PreflightApply(ctx, plan); !errors.Is(err, ErrParentChanged) {
		t.Fatal("ignored file treated as absent", err)
	}
	if err := os.Remove(filepath.Join(root, "new.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "a.txt"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := m.PreflightApply(ctx, plan); !errors.Is(err, ErrParentChanged) {
		t.Fatal("mode drift accepted", err)
	}
}

func TestMergeConflictWithoutFileStages(t *testing.T) {
	tree := strings.Repeat("a", 40)
	out := []byte(tree + "\x00\x001\x00directory\x00CONFLICT (directory rename suggested)\x00resolve directory\x00")
	_, conflicts, err := parseMergeOutput(out, true)
	if err != nil || len(conflicts) != 1 || len(conflicts[0].Entries) != 0 {
		t.Fatal(conflicts, err)
	}
	_, conflicts, err = parseMergeOutput([]byte(tree+"\x00"), true)
	if err != nil || len(conflicts) != 1 {
		t.Fatal("nonzero exit lost", err)
	}
}
