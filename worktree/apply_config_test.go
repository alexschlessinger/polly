package worktree

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestApplyPinsPatchPathsDespiteDiffConfiguration(t *testing.T) {
	for _, config := range []string{"diff.noprefix", "diff.mnemonicPrefix"} {
		t.Run(config, func(t *testing.T) {
			m, root := fixture(t)
			ctx := context.Background()
			if err := os.Mkdir(filepath.Join(root, "nested"), 0700); err != nil {
				t.Fatal(err)
			}
			writeTest(t, filepath.Join(root, "nested", "file.txt"), "original\n")
			writeTest(t, filepath.Join(root, "file.txt"), "original\n")
			gitTest(t, root, "add", ".")
			gitTest(t, root, "commit", "-qm", "nested fixture")
			gitTest(t, root, "config", config, "true")
			base, err := m.Capture(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			child, err := m.Create(ctx, base)
			if err != nil {
				t.Fatal(err)
			}
			writeTest(t, filepath.Join(child.Path, "nested", "file.txt"), "member edit\n")
			candidate, err := m.Capture(ctx, child.Path)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := m.BuildApplyPlan(ctx, "fixture", base, candidate, "paths")
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.Paths) != 1 || plan.Paths[0].Path != "nested/file.txt" {
				t.Fatalf("unexpected plan: %+v", plan.Paths)
			}
			if _, err := m.PreflightApply(ctx, plan); err != nil {
				t.Fatalf("preflight: %v", err)
			}
			if err := m.WriteApply(ctx, plan); err != nil {
				t.Fatalf("apply: %v", err)
			}
			expected, _ := os.ReadFile(filepath.Join(root, "nested", "file.txt"))
			untouched, _ := os.ReadFile(filepath.Join(root, "file.txt"))
			if string(expected) != "member edit\n" || string(untouched) != "original\n" {
				t.Fatalf("apply wrote wrong path: planned=%q unrelated=%q", expected, untouched)
			}
		})
	}
}
