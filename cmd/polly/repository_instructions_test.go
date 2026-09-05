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

func TestRepositoryInstructionsScopeAndOrder(t *testing.T) {
	outer := t.TempDir()
	root := filepath.Join(outer, "repo")
	cwd := filepath.Join(root, "src", "feature")
	writeRepositoryTestFile(t, filepath.Join(outer, "AGENTS.md"), "outside-repository")
	writeRepositoryTestFile(t, filepath.Join(root, ".git", "HEAD"), "ref: refs/heads/main")
	writeRepositoryTestFile(t, filepath.Join(root, "AGENTS.md"), "root-guidance")
	writeRepositoryTestFile(t, filepath.Join(root, "src", "AGENTS.md"), "source-guidance")
	writeRepositoryTestFile(t, filepath.Join(cwd, "AGENTS.md"), "feature-guidance <file> & more")
	writeRepositoryTestFile(t, filepath.Join(cwd, "deeper", "AGENTS.md"), "descendant-guidance")
	writeRepositoryTestFile(t, filepath.Join(root, "sibling", "AGENTS.md"), "sibling-guidance")
	t.Chdir(cwd)

	got, err := loadRepositoryInstructions(nil)
	if err != nil {
		t.Fatal(err)
	}
	last := -1
	for _, want := range []string{"root-guidance", "source-guidance", "feature-guidance &lt;file&gt; &amp; more"} {
		idx := strings.Index(got, want)
		if idx <= last {
			t.Fatalf("missing or out-of-order %q in %q", want, got)
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
			cwd := filepath.Join(outer, "work")
			writeRepositoryTestFile(t, filepath.Join(outer, "AGENTS.md"), "parent-guidance")
			writeRepositoryTestFile(t, filepath.Join(cwd, "AGENTS.md"), "current-guidance")
			if gitMarker {
				writeRepositoryTestFile(t, filepath.Join(outer, ".git", "HEAD"), "outer-repository")
				writeRepositoryTestFile(t, filepath.Join(cwd, ".git"), "gitdir: elsewhere")
			}
			t.Chdir(cwd)
			got, err := loadRepositoryInstructions(nil)
			if err != nil || !strings.Contains(got, "current-guidance") || strings.Contains(got, "parent-guidance") {
				t.Fatalf("instructions = %q, %v", got, err)
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
			registry := tools.NewToolRegistry(nil, tools.WithSandboxFactory(func(sandbox.Config) (sandbox.Sandbox, error) {
				t.Fatal("instruction loading must not start a process")
				return nil, nil
			}, sandbox.Config{DenyPaths: []string{denied}}))
			defer registry.Close()
			t.Chdir(cwd)
			got, err := loadRepositoryInstructions(registry)
			if err == nil || !strings.Contains(err.Error(), "blocked from reads") || got != "" {
				t.Fatalf("denied instructions = %q, %v", got, err)
			}
		})
	}
}

func TestRepositoryInstructionsRejectInvalidFiles(t *testing.T) {
	for _, tc := range []struct {
		name, content, wantError string
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
			if got, err := loadRepositoryInstructions(nil); err == nil || !strings.Contains(err.Error(), tc.wantError) || got != "" {
				t.Fatalf("invalid instructions = %q, %v", got, err)
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
	if got, err := loadRepositoryInstructions(nil); err == nil || !strings.Contains(err.Error(), "bytes in total") || got != "" {
		t.Fatalf("oversized combined instructions = %q, %v", got, err)
	}
}
