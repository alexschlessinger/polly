package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

func writeRepositoryTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// skipInsideRepository skips a test whose fixture must sit outside any Git
// checkout, as a temp dir under a workspace-local TMPDIR does not.
func skipInsideRepository(t *testing.T, dir string) {
	t.Helper()
	for {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			t.Skipf("%s is inside a Git checkout", dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return
		}
		dir = parent
	}
}

func testRepositoryReadPolicy(t *testing.T, denied ...string) *tools.ToolRegistry {
	t.Helper()
	registry := tools.NewToolRegistry(nil, tools.WithSandboxFactory(func(sandbox.Config) (sandbox.Sandbox, error) {
		t.Fatal("instruction loading must not start a process")
		return nil, nil
	}, sandbox.Config{DenyPaths: denied}))
	t.Cleanup(func() { registry.Close() })
	return registry
}

func TestRepositoryInstructionsScopeAndOrder(t *testing.T) {
	outer := t.TempDir()
	root := filepath.Join(outer, "repo")
	cwd := filepath.Join(root, "src", "feature")
	writeRepositoryTestFile(t, filepath.Join(outer, "AGENTS.md"), "outside-repository")
	writeRepositoryTestFile(t, filepath.Join(root, ".git", "HEAD"), "ref: refs/heads/main")
	writeRepositoryTestFile(t, filepath.Join(root, "AGENTS.md"), "root-guidance")
	writeRepositoryTestFile(t, filepath.Join(root, "src", "AGENTS.md"), "source-guidance")
	writeRepositoryTestFile(t, filepath.Join(cwd, "AGENTS.md"), "feature-guidance: run `go test ./... 2>&1 | tail` & don't skip <T>")
	writeRepositoryTestFile(t, filepath.Join(cwd, "deeper", "AGENTS.md"), "descendant-guidance")
	writeRepositoryTestFile(t, filepath.Join(root, "sibling", "AGENTS.md"), "sibling-guidance")
	t.Chdir(cwd)

	got, warnings := loadRepositoryInstructions(nil)
	if len(warnings) != 0 {
		t.Fatal(warnings)
	}
	if !strings.HasPrefix(got, "Working directory: "+cwd+"\n") {
		t.Fatalf("working directory line missing or quoted: %q", got)
	}
	last := -1
	for _, want := range []string{"root-guidance", "source-guidance", "feature-guidance: run `go test ./... 2>&1 | tail` & don't skip <T>"} {
		idx := strings.Index(got, want)
		if idx <= last {
			t.Fatalf("missing, mangled, or out-of-order %q in %q", want, got)
		}
		last = idx
	}
	for _, unwanted := range []string{"outside-repository", "descendant-guidance", "sibling-guidance"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("loaded instructions outside the current scope: %q", got)
		}
	}
}

func TestRepositoryInstructionsNearestRootAndNonRepository(t *testing.T) {
	for _, gitMarker := range []bool{false, true} {
		t.Run(map[bool]string{false: "no repository", true: "nested worktree"}[gitMarker], func(t *testing.T) {
			outer := t.TempDir()
			if !gitMarker {
				skipInsideRepository(t, outer)
			}
			cwd := filepath.Join(outer, "work")
			writeRepositoryTestFile(t, filepath.Join(outer, "AGENTS.md"), "parent-guidance")
			writeRepositoryTestFile(t, filepath.Join(cwd, "AGENTS.md"), "current-guidance")
			if gitMarker {
				writeRepositoryTestFile(t, filepath.Join(outer, ".git", "HEAD"), "outer-repository")
				writeRepositoryTestFile(t, filepath.Join(cwd, ".git"), "gitdir: elsewhere")
			}
			t.Chdir(cwd)
			got, warnings := loadRepositoryInstructions(nil)
			if len(warnings) != 0 || !strings.Contains(got, "current-guidance") || strings.Contains(got, "parent-guidance") {
				t.Fatalf("instructions = %q, %v", got, warnings)
			}
		})
	}
}

func TestRepositoryInstructionsEnforceReadPolicy(t *testing.T) {
	for _, viaSymlink := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "symlink"}[viaSymlink], func(t *testing.T) {
			if viaSymlink {
				skipIfWindows(t)
			}
			cwd := t.TempDir()
			path := filepath.Join(cwd, "AGENTS.md")
			denied := path
			if viaSymlink {
				denied = filepath.Join(t.TempDir(), "private.md")
			}
			writeRepositoryTestFile(t, denied, "private-instructions")
			if viaSymlink {
				if err := os.Symlink(denied, path); err != nil {
					t.Fatal(err)
				}
			}
			t.Chdir(cwd)
			got, warnings := loadRepositoryInstructions(testRepositoryReadPolicy(t, denied))
			if strings.Contains(got, "private-instructions") || len(warnings) != 1 || !strings.Contains(warnings[0], "blocked from reads") {
				t.Fatalf("denied instructions = %q, %v", got, warnings)
			}
		})
	}
}

func TestRepositoryInstructionsTolerateDeniedPathsWithoutInstructions(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "src")
	writeRepositoryTestFile(t, filepath.Join(root, ".git", "HEAD"), "ref: refs/heads/main")
	writeRepositoryTestFile(t, filepath.Join(root, "AGENTS.md"), "root-guidance")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)
	// Denying the repository internals, or the working directory itself,
	// must neither fail the turn nor hide the root's instructions.
	for _, denied := range []string{filepath.Join(root, ".git"), cwd} {
		got, warnings := loadRepositoryInstructions(testRepositoryReadPolicy(t, denied))
		if len(warnings) != 0 || !strings.Contains(got, "root-guidance") {
			t.Fatalf("deny %s: instructions = %q, %v", denied, got, warnings)
		}
	}
}

func TestRepositoryInstructionsSkipInvalidFiles(t *testing.T) {
	for _, tc := range []struct {
		name, content, wantWarning string
	}{
		{"oversized", strings.Repeat("x", maxRepositoryInstructionBytes+1), "file exceeds"},
		{"invalid UTF-8", "\xff", "UTF-8"},
		{"binary", "hello\x00world", "NUL"},
		{"directory", "", "directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cwd := t.TempDir()
			path := filepath.Join(cwd, "AGENTS.md")
			if tc.name == "directory" {
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
			} else {
				writeRepositoryTestFile(t, path, tc.content)
			}
			t.Chdir(cwd)
			got, warnings := loadRepositoryInstructions(nil)
			if strings.Contains(got, "<file ") || len(warnings) != 1 || !strings.Contains(warnings[0], tc.wantWarning) || !strings.Contains(warnings[0], path) {
				t.Fatalf("invalid instructions = %q, %v", got, warnings)
			}
		})
	}
}

func TestRepositoryInstructionsBoundCombinedSize(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "src", "feature")
	writeRepositoryTestFile(t, filepath.Join(root, ".git"), "gitdir: elsewhere")
	for _, dir := range []string{root, filepath.Dir(cwd), cwd} {
		writeRepositoryTestFile(t, filepath.Join(dir, "AGENTS.md"), strings.Repeat("x", maxRepositoryInstructionBytes))
	}
	t.Chdir(cwd)
	got, warnings := loadRepositoryInstructions(nil)
	if strings.Count(got, "<file ") != 2 || len(warnings) != 1 || !strings.Contains(warnings[0], "bytes in total") || !strings.Contains(warnings[0], filepath.Join(cwd, "AGENTS.md")) {
		t.Fatalf("oversized combined instructions: %d files, %v", strings.Count(got, "<file "), warnings)
	}
}
