package sandbox

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestParsePresetBase(t *testing.T) {
	for _, spec := range []string{"", "base"} {
		cfg, err := ParsePreset(spec)
		if err != nil {
			t.Fatalf("ParsePreset(%q) error = %v", spec, err)
		}
		want := DefaultConfig()
		if !slices.Equal(cfg.WritablePaths, want.WritablePaths) || cfg.AllowNetwork || cfg.DenyWrite {
			t.Fatalf("ParsePreset(%q) = %+v, want base config %+v", spec, cfg, want)
		}
	}
}

func TestParsePresetComponents(t *testing.T) {
	cfg, err := ParsePreset("readonly+net")
	if err != nil {
		t.Fatalf("ParsePreset() error = %v", err)
	}
	if !cfg.DenyWrite {
		t.Fatal("readonly should set DenyWrite")
	}
	if !cfg.AllowNetwork {
		t.Fatal("net should set AllowNetwork")
	}
}

func TestParsePresetUnknownName(t *testing.T) {
	for _, spec := range []string{"workspace+typo", "yes", "workspace,net"} {
		if _, err := ParsePreset(spec); err == nil {
			t.Fatalf("ParsePreset(%q) error = nil, want unknown-preset failure", spec)
		}
	}
}

func TestParsePresetWorkspace(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("[core]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	cfg, err := ParsePreset("workspace+net")
	if err != nil {
		t.Fatalf("ParsePreset() error = %v", err)
	}
	// Compare against the same Getwd ParsePreset uses so tmpdir symlinks
	// (macOS /var -> /private/var) don't skew the expectation.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cfg.WritablePaths, cwd) {
		t.Fatalf("WritablePaths = %v, want to include cwd %q", cfg.WritablePaths, cwd)
	}
	if !cfg.AllowNetwork {
		t.Fatal("workspace+net should set AllowNetwork")
	}
	if len(cfg.gitPolicies) != 1 || cfg.gitPolicies[0].workspace != filepath.Clean(cwd) {
		t.Fatalf("workspace Git policy = %+v, want one policy for %q", cfg.gitPolicies, cwd)
	}
	for _, want := range []string{
		mustEvalSymlinks(t, filepath.Join(cwd, ".git")),
	} {
		if !slices.Contains(cfg.DenyWritePaths, want) {
			t.Fatalf("DenyWritePaths = %v, want to include %q", cfg.DenyWritePaths, want)
		}
	}
}

func TestParsePresetWorkspaceWithoutGit(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg, err := ParsePreset("workspace")
	if err != nil {
		t.Fatalf("ParsePreset() error = %v", err)
	}
	if len(cfg.DenyWritePaths) != 0 {
		t.Fatalf("DenyWritePaths = %v, want none without a .git", cfg.DenyWritePaths)
	}
}

func TestParsePresetWorkspaceRejectsHomeBeforeGitScan(t *testing.T) {
	home := t.TempDir()
	protectedDescendant := filepath.Join(home, "Library", "TCC-protected")
	if err := os.MkdirAll(protectedDescendant, 0o755); err != nil {
		t.Fatal(err)
	}
	// If workspace discovery reaches this entry, it fails as invalid Git
	// routing. The broad-workspace check must run first instead of accepting a
	// partial scan when another descendant is unreadable on the host.
	if err := os.WriteFile(filepath.Join(protectedDescendant, ".git"), []byte("not a Git pointer\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", home)
	} else {
		t.Setenv("HOME", home)
	}
	t.Chdir(home)

	_, err := ParsePreset("workspace+net")
	if err == nil {
		t.Fatal("ParsePreset(workspace+net) error = nil, want broad home-directory rejection")
	}
	for _, want := range []string{"home directory", "bounded project directory", "cd into a project directory", "--sandbox base"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("ParsePreset(workspace+net) error = %q, want actionable %q guidance", err, want)
		}
	}
	if strings.Contains(err.Error(), "protect Git metadata") || strings.Contains(err.Error(), "Git routing entry") {
		t.Fatalf("ParsePreset(workspace+net) error = %q, broad workspace was scanned before rejection", err)
	}
}

func TestRejectBroadWorkspaceRejectsFilesystemRoot(t *testing.T) {
	root := filepath.Clean(t.TempDir())
	for filepath.Dir(root) != root {
		root = filepath.Dir(root)
	}

	err := rejectBroadWorkspace(root)
	if err == nil {
		t.Fatalf("rejectBroadWorkspace(%q) error = nil, want filesystem-root rejection", root)
	}
	for _, want := range []string{"filesystem root", "bounded project directory", "--sandbox base"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("rejectBroadWorkspace(%q) error = %q, want actionable %q guidance", root, err, want)
		}
	}
}

func TestGitGuardrailPathsWorktreePointer(t *testing.T) {
	// Layout: main repo at main/.git with a linked worktree at wt/ whose
	// .git file points at main/.git/worktrees/wt, which in turn has a
	// commondir pointer back to main/.git.
	root := t.TempDir()
	mainGit := filepath.Join(root, "main", ".git")
	wtGitDir := filepath.Join(mainGit, "worktrees", "wt")
	wt := filepath.Join(root, "wt")
	for _, d := range []string{filepath.Join(mainGit, "hooks"), wtGitDir, wt} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+wtGitDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtGitDir, "commondir"), []byte("../..\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := gitGuardrailPaths(wt)
	if err != nil {
		t.Fatalf("gitGuardrailPaths() error = %v", err)
	}
	for _, want := range []string{
		mustEvalSymlinks(t, filepath.Join(wt, ".git")),
		mustEvalSymlinks(t, wtGitDir),
		filepath.Join(mustEvalSymlinks(t, wtGitDir), "commondir"),
		mustEvalSymlinks(t, mainGit),
	} {
		if !protectedByAny(got, want) {
			t.Fatalf("gitGuardrailPaths() = %v, want to protect %q", got, want)
		}
	}
}

func TestGitGuardrailPathsFindsNestedRepositoriesAndSubmodules(t *testing.T) {
	root := t.TempDir()
	rootGit := filepath.Join(root, ".git")
	nestedGit := filepath.Join(root, "packages", "nested", ".git")
	moduleGit := filepath.Join(rootGit, "modules", "sub")
	submodule := filepath.Join(root, "vendor", "sub")
	for _, dir := range []string{rootGit, nestedGit, moduleGit, submodule} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(submodule, ".git"), []byte("gitdir: ../../.git/modules/sub\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := gitGuardrailPaths(root)
	if err != nil {
		t.Fatalf("gitGuardrailPaths() error = %v", err)
	}
	for _, want := range []string{
		mustEvalSymlinks(t, rootGit),
		mustEvalSymlinks(t, nestedGit),
		mustEvalSymlinks(t, filepath.Join(submodule, ".git")),
		mustEvalSymlinks(t, moduleGit),
	} {
		if !protectedByAny(got, want) {
			t.Errorf("gitGuardrailPaths() = %v, want to protect %q", got, want)
		}
	}
}

func TestGitGuardrailPathsMatchesCaseFoldedGitEntry(t *testing.T) {
	root := t.TempDir()
	gitDir := filepath.Join(root, ".GIT")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := gitGuardrailPaths(root)
	if err != nil {
		t.Fatalf("gitGuardrailPaths() error = %v", err)
	}
	realGitDir := mustEvalSymlinks(t, gitDir)
	if !slices.Contains(got, realGitDir) {
		t.Fatalf("gitGuardrailPaths() = %v, want case-folded routing entry %q", got, realGitDir)
	}
}

func TestGitGuardrailPathsProtectsConfigWorktreeByProtectingGitDir(t *testing.T) {
	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// config.worktree is deliberately absent. Protecting the containing gitdir
	// must reserve that name instead of relying on a bind of an existing leaf.
	got, err := gitGuardrailPaths(root)
	if err != nil {
		t.Fatalf("gitGuardrailPaths() error = %v", err)
	}
	realGitDir := mustEvalSymlinks(t, gitDir)
	if !slices.Contains(got, realGitDir) {
		t.Fatalf("gitGuardrailPaths() = %v, want containing gitdir %q", got, realGitDir)
	}
}

func TestGitGuardrailPathsRejectsSymlinkedRouting(t *testing.T) {
	root := t.TempDir()
	realGit := filepath.Join(root, "real-git")
	if err := os.MkdirAll(realGit, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realGit, filepath.Join(root, ".git")); err != nil {
		t.Fatal(err)
	}

	if _, err := gitGuardrailPaths(root); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("gitGuardrailPaths() error = %v, want symlink fail-closed error", err)
	}
}

func TestGitGuardrailPathsRejectsPointerThroughWorkspaceSymlink(t *testing.T) {
	root := t.TempDir()
	worktree := filepath.Join(root, "worktree")
	realGit := filepath.Join(root, "real-metadata", "worktree")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(realGit, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(realGit), filepath.Join(root, "metadata-route")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: ../metadata-route/worktree\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := gitGuardrailPaths(root); err == nil || !strings.Contains(err.Error(), "traverses symlink") {
		t.Fatalf("gitGuardrailPaths() error = %v, want intermediate-symlink failure", err)
	}
}

func TestGitGuardrailPathsRejectsPointerThroughExternalSymlink(t *testing.T) {
	root := t.TempDir()
	worktree := filepath.Join(root, "worktree")
	external := t.TempDir()
	realGit := filepath.Join(external, "real-metadata", "worktree")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(realGit, 0o755); err != nil {
		t.Fatal(err)
	}
	route := filepath.Join(external, "metadata-route")
	if err := os.Symlink(filepath.Dir(realGit), route); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: "+filepath.Join(route, "worktree")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := gitGuardrailPaths(root); err == nil || !strings.Contains(err.Error(), "traverses symlink") {
		t.Fatalf("gitGuardrailPaths() error = %v, want external intermediate-symlink failure", err)
	}
}

func TestGitGuardrailPathsRejectsInvalidOrDanglingPointer(t *testing.T) {
	for _, contents := range []string{
		"not-a-git-pointer\n",
		"gitdir: missing\n",
		"gitdir: one\nsecond line\n",
	} {
		t.Run(strings.ReplaceAll(strings.TrimSpace(contents), "\n", "_"), func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, ".git"), []byte(contents), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := gitGuardrailPaths(root); err == nil {
				t.Fatal("gitGuardrailPaths() error = nil, want invalid routing to fail closed")
			}
		})
	}
}

func TestParsePresetWorkspaceRejectsUnsafeGitRouting(t *testing.T) {
	root := t.TempDir()
	realGit := filepath.Join(root, "real-git")
	if err := os.MkdirAll(realGit, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realGit, filepath.Join(root, ".git")); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)

	if _, err := ParsePreset("workspace"); err == nil || !strings.Contains(err.Error(), "cannot be pinned safely") {
		t.Fatalf("ParsePreset(workspace) error = %v, want unsafe Git routing failure", err)
	}
}

func TestGitGuardrailPathsRejectsSymlinkedProtectedMetadata(t *testing.T) {
	for _, tc := range []struct {
		name string
		make func(t *testing.T, gitDir, target string)
	}{
		{
			name: "config",
			make: func(t *testing.T, gitDir, target string) {
				t.Helper()
				if err := os.Symlink(target, filepath.Join(gitDir, "config")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "config.worktree",
			make: func(t *testing.T, gitDir, target string) {
				t.Helper()
				if err := os.Symlink(target, filepath.Join(gitDir, "config.worktree")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "hooks directory",
			make: func(t *testing.T, gitDir, target string) {
				t.Helper()
				if err := os.Symlink(filepath.Dir(target), filepath.Join(gitDir, "hooks")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "hook file",
			make: func(t *testing.T, gitDir, target string) {
				t.Helper()
				hooks := filepath.Join(gitDir, "hooks")
				if err := os.MkdirAll(hooks, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, filepath.Join(hooks, "pre-commit")); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			gitDir := filepath.Join(root, ".git")
			if err := os.MkdirAll(gitDir, 0o755); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(root, "writable-target")
			if err := os.WriteFile(target, []byte("target\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			tc.make(t, gitDir, target)

			if _, err := gitGuardrailPaths(root); err == nil || !strings.Contains(err.Error(), "symlink") {
				t.Fatalf("gitGuardrailPaths() error = %v, want protected-metadata symlink failure", err)
			}
		})
	}
}

func TestGitGuardrailPathsRejectsConfigIndirection(t *testing.T) {
	for _, tc := range []struct {
		name     string
		contents string
		want     string
	}{
		{name: "hooks path", contents: "[core]\n\thooksPath = ../.githooks\n", want: "core.hooksPath"},
		{name: "hooks path with BOM", contents: "\xef\xbb\xbf[core]\n\thooksPath = ../.githooks\n", want: "core.hooksPath"},
		{name: "include", contents: "[include]\n\tpath = ../shared.gitconfig\n", want: "includes another config"},
		{name: "include with BOM", contents: "\xef\xbb\xbf[include]\n\tpath = ../shared.gitconfig\n", want: "includes another config"},
		{name: "conditional include", contents: "[includeIf \"onbranch:main\"]\n\tpath = ../main.gitconfig\n", want: "includes another config"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			gitDir := filepath.Join(root, ".git")
			if err := os.MkdirAll(gitDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(gitDir, "config"), []byte(tc.contents), 0o644); err != nil {
				t.Fatal(err)
			}

			if _, err := gitGuardrailPaths(root); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("gitGuardrailPaths() error = %v, want %q failure", err, tc.want)
			}
		})
	}
}

func TestGitGuardrailPathsRejectsEffectiveGlobalHooksPathInWorkspace(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	hooks := filepath.Join(root, ".githooks")
	if err := os.Mkdir(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooks, "pre-commit"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	setIsolatedGitConfigValue(t, "core.hooksPath", ".githooks")

	if _, err := gitGuardrailPaths(root); err == nil || !strings.Contains(err.Error(), "effective core.hooksPath") {
		t.Fatalf("gitGuardrailPaths() error = %v, want effective global hooks-path failure", err)
	}
}

func TestGitGuardrailPathsRejectsEmptyEffectiveGlobalHooksPath(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Git interprets an empty core.hooksPath as ./, so hooks such as
	// <worktree>/pre-commit would otherwise remain plantable.
	setIsolatedGitConfigValue(t, "core.hooksPath", "")

	if _, err := gitGuardrailPaths(root); err == nil || !strings.Contains(err.Error(), "effective core.hooksPath") {
		t.Fatalf("gitGuardrailPaths() error = %v, want empty global hooks-path failure", err)
	}
}

func TestGitGuardrailPathsAllowsEffectiveHooksPathInsideProtectedMetadata(t *testing.T) {
	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	if err := os.MkdirAll(filepath.Join(gitDir, "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	setIsolatedGitConfigValue(t, "core.hooksPath", ".git/hooks")

	paths, err := gitGuardrailPaths(root)
	if err != nil {
		t.Fatalf("gitGuardrailPaths() error = %v", err)
	}
	if !pathProtectedByGitGuardrail(mustEvalSymlinks(t, filepath.Join(gitDir, "hooks")), paths) {
		t.Fatalf("effective hooks path is not covered by protected metadata: %v", paths)
	}
}

func TestGitGuardrailPathsRejectsAliasedConfiguredHooks(t *testing.T) {
	for _, tc := range []struct {
		alias string
		want  string
	}{{alias: "symlink", want: "symlink"}, {alias: "hardlink", want: "multiple hard links"}} {
		t.Run(tc.alias, func(t *testing.T) {
			root := t.TempDir()
			hooks := filepath.Join(root, ".git", "custom-hooks")
			if err := os.MkdirAll(hooks, 0o755); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(root, "writable-hook-target")
			if err := os.WriteFile(target, []byte("#!/bin/sh\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			hook := filepath.Join(hooks, "pre-commit")
			if tc.alias == "symlink" {
				if err := os.Symlink(target, hook); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Link(target, hook); err != nil {
				t.Skipf("hard links unavailable: %v", err)
			}
			setIsolatedGitConfigValue(t, "core.hooksPath", ".git/custom-hooks")

			if _, err := gitGuardrailPaths(root); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("gitGuardrailPaths() error = %v, want configured-hook %s rejection", err, tc.alias)
			}
		})
	}
}

func TestValidateConfiguredHooksPathRejectsExternalAliases(t *testing.T) {
	for _, tc := range []struct {
		alias string
		want  string
	}{{alias: "symlink", want: "symlink"}, {alias: "hardlink", want: "multiple hard links"}} {
		t.Run(tc.alias, func(t *testing.T) {
			parent := t.TempDir()
			workspace := filepath.Join(parent, "workspace")
			hooks := filepath.Join(parent, "external-hooks")
			if err := os.MkdirAll(workspace, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(hooks, 0o755); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(workspace, "hook-target")
			if err := os.WriteFile(target, []byte("#!/bin/sh\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			hook := filepath.Join(hooks, "pre-commit")
			if tc.alias == "symlink" {
				if err := os.Symlink(target, hook); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Link(target, hook); err != nil {
				t.Skipf("hard links unavailable: %v", err)
			}

			err := validateConfiguredHooksPath(hooks, "effective core.hooksPath", []string{workspace}, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validateConfiguredHooksPath() error = %v, want external-hook %s rejection", err, tc.alias)
			}
		})
	}
}

func TestValidateTrustedGitExecutableAcceptsStandardHomebrewRoute(t *testing.T) {
	selected, systemGit, target, prefix := homebrewGitFixture(t)
	workspace := filepath.Join(filepath.Dir(prefix), "workspace")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	// Homebrew's standard bin directory may be group-writable by the host
	// account. It remains safe here because it is outside every path writable
	// by the sandbox; the resolved executable itself must still be immutable.
	if err := os.Chmod(filepath.Join(prefix, "bin"), 0o775); err != nil {
		t.Fatal(err)
	}

	got, err := validateTrustedGitExecutable(selected, systemGit, []string{workspace}, []string{prefix})
	if err != nil {
		t.Fatalf("validateTrustedGitExecutable() error = %v", err)
	}
	if got != target {
		t.Fatalf("validateTrustedGitExecutable() = %q, want resolved Cellar target %q", got, target)
	}
}

func TestValidateTrustedGitExecutableRejectsUnsafeHomebrewRoutes(t *testing.T) {
	t.Run("writable target", func(t *testing.T) {
		selected, systemGit, target, prefix := homebrewGitFixture(t)
		if err := os.Chmod(target, 0o755); err != nil {
			t.Fatal(err)
		}

		if _, err := validateTrustedGitExecutable(selected, systemGit, nil, []string{prefix}); err == nil || !strings.Contains(err.Error(), "is writable") {
			t.Fatalf("validateTrustedGitExecutable() error = %v, want writable-target rejection", err)
		}
	})

	t.Run("hard-linked target", func(t *testing.T) {
		selected, systemGit, target, prefix := homebrewGitFixture(t)
		if err := os.Link(target, target+"-alias"); err != nil {
			t.Skipf("hard links unavailable: %v", err)
		}

		if _, err := validateTrustedGitExecutable(selected, systemGit, nil, []string{prefix}); err == nil || !strings.Contains(err.Error(), "multiple hard links") {
			t.Fatalf("validateTrustedGitExecutable() error = %v, want hard-link rejection", err)
		}
	})

	t.Run("sandbox-writable route", func(t *testing.T) {
		selected, systemGit, _, prefix := homebrewGitFixture(t)

		if _, err := validateTrustedGitExecutable(selected, systemGit, []string{prefix}, []string{prefix}); err == nil || !strings.Contains(err.Error(), "writable sandbox path") {
			t.Fatalf("validateTrustedGitExecutable() error = %v, want writable-route rejection", err)
		}
	})

	t.Run("sandbox-writable resolved target", func(t *testing.T) {
		selected, systemGit, target, prefix := homebrewGitFixture(t)

		if _, err := validateTrustedGitExecutable(selected, systemGit, []string{filepath.Dir(target)}, []string{prefix}); err == nil || !strings.Contains(err.Error(), "writable sandbox path") {
			t.Fatalf("validateTrustedGitExecutable() error = %v, want writable-target-route rejection", err)
		}
	})

	t.Run("unexpected symlink chain", func(t *testing.T) {
		root := mustEvalSymlinks(t, t.TempDir())
		prefix := filepath.Join(root, "homebrew")
		realCellar := filepath.Join(root, "real-cellar")
		target := filepath.Join(realCellar, "git", "2.52.0", "bin", "git")
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte("git"), 0o555); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(prefix, "bin"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realCellar, filepath.Join(prefix, "Cellar")); err != nil {
			t.Fatal(err)
		}
		selected := filepath.Join(prefix, "bin", "git")
		if err := os.Symlink("../Cellar/git/2.52.0/bin/git", selected); err != nil {
			t.Fatal(err)
		}
		systemGit := filepath.Join(root, "system-git")
		if err := os.WriteFile(systemGit, []byte("git"), 0o555); err != nil {
			t.Fatal(err)
		}

		if _, err := validateTrustedGitExecutable(selected, systemGit, nil, []string{prefix}); err == nil || !strings.Contains(err.Error(), "unexpected symlink") {
			t.Fatalf("validateTrustedGitExecutable() error = %v, want symlink-chain rejection", err)
		}
	})
}

func homebrewGitFixture(t *testing.T) (selected, systemGit, target, prefix string) {
	t.Helper()
	root := mustEvalSymlinks(t, t.TempDir())
	prefix = filepath.Join(root, "homebrew")
	target = filepath.Join(prefix, "Cellar", "git", "2.52.0", "bin", "git")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("git"), 0o555); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(prefix, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	selected = filepath.Join(prefix, "bin", "git")
	if err := os.Symlink("../Cellar/git/2.52.0/bin/git", selected); err != nil {
		t.Fatal(err)
	}
	systemGit = filepath.Join(root, "system-git")
	if err := os.WriteFile(systemGit, []byte("git"), 0o555); err != nil {
		t.Fatal(err)
	}
	return selected, systemGit, target, prefix
}

func TestGitGuardrailPathsDoesNotExecuteWorkspaceGitBinary(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "git-ran")
	fakeGit := filepath.Join(bin, "git")
	if err := os.WriteFile(fakeGit, []byte("#!/bin/sh\ntouch \"$POLLY_TEST_GIT_MARKER\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("POLLY_TEST_GIT_MARKER", marker)
	isolateGitConfig(t)

	if _, err := gitGuardrailPaths(root); err == nil || !strings.Contains(err.Error(), "does not match trusted OS Git") {
		t.Fatalf("gitGuardrailPaths() error = %v, want untrusted PATH Git rejection", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("workspace Git executable ran before containment: %v", err)
	}
}

func TestGitGuardrailPathsRejectsCaseVariedHooksPathOnCaseInsensitiveVolume(t *testing.T) {
	root, caseVariant := caseVariedWorkspace(t)
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Leave the hook directory absent: the policy must recognize a
	// case-varied existing workspace prefix before a tool can create the leaf.
	setIsolatedGitConfigValue(t, "core.hooksPath", filepath.Join(caseVariant, ".githooks"))

	if _, err := gitGuardrailPaths(root); err == nil || !strings.Contains(err.Error(), "effective core.hooksPath") {
		t.Fatalf("gitGuardrailPaths() error = %v, want case-varied hooks-path rejection", err)
	}
}

func TestGitGuardrailPathsDoesNotExecuteCaseVariedWorkspaceGitBinary(t *testing.T) {
	root, caseVariant := caseVariedWorkspace(t)
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "git-ran")
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\ntouch \"$POLLY_TEST_GIT_MARKER\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Join(caseVariant, "bin"))
	t.Setenv("POLLY_TEST_GIT_MARKER", marker)
	isolateGitConfig(t)

	if _, err := gitGuardrailPaths(root); err == nil || !strings.Contains(err.Error(), "does not match trusted OS Git") {
		t.Fatalf("gitGuardrailPaths() error = %v, want case-varied untrusted Git rejection", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("case-varied workspace Git executable ran before containment: %v", err)
	}
}

func TestGitGuardrailPathsRejectsWorkspaceRouteToTrustedGit(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/bin/git", filepath.Join(bin, "git")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	isolateGitConfig(t)

	if _, err := gitGuardrailPaths(root); err == nil || !strings.Contains(err.Error(), "cannot be trusted persistently") {
		t.Fatalf("gitGuardrailPaths() error = %v, want writable PATH route rejection", err)
	}
}

func TestGitGuardrailPathsRejectsWritableConfigSelectors(t *testing.T) {
	for _, selector := range []string{"global", "system"} {
		t.Run(selector, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
				t.Fatal(err)
			}
			config := filepath.Join(root, selector+".gitconfig")
			if err := os.WriteFile(config, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			isolateGitConfig(t)
			if selector == "global" {
				t.Setenv("GIT_CONFIG_GLOBAL", config)
			} else {
				unsetTestEnv(t, "GIT_CONFIG_NOSYSTEM")
				t.Setenv("GIT_CONFIG_SYSTEM", config)
			}

			if _, err := gitGuardrailPaths(root); err == nil || !strings.Contains(err.Error(), selector+" Git config source") {
				t.Fatalf("gitGuardrailPaths() error = %v, want writable %s config rejection", err, selector)
			}
		})
	}
}

func TestGitGuardrailPathsAuditsSystemSelectorWhenNoSystemIsFalse(t *testing.T) {
	for _, value := range []string{"0", "false", "no", "off"} {
		t.Run(value, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
				t.Fatal(err)
			}
			isolateGitConfig(t)
			t.Setenv("GIT_CONFIG_NOSYSTEM", value)
			t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(root, "missing-system.gitconfig"))

			if _, err := gitGuardrailPaths(root); err == nil || !strings.Contains(err.Error(), "system Git config source") {
				t.Fatalf("gitGuardrailPaths() error = %v, want false NOSYSTEM selector rejection", err)
			}
		})
	}
}

func TestGitGuardrailPathsAuditsExplicitSystemSelectorWhenNoSystemIsTrue(t *testing.T) {
	for _, value := range []string{"1", "true", "yes", "on"} {
		t.Run(value, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
				t.Fatal(err)
			}
			isolateGitConfig(t)
			t.Setenv("GIT_CONFIG_NOSYSTEM", value)
			t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(root, "missing-system.gitconfig"))

			if _, err := gitGuardrailPaths(root); err == nil || !strings.Contains(err.Error(), "system Git config source") {
				t.Fatalf("gitGuardrailPaths() error = %v, want explicit system selector rejection", err)
			}
		})
	}
}

func TestGitGuardrailPathsRejectsMissingWorkspaceConfigInclude(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	setIsolatedGitConfigValue(t, "include.path", filepath.Join(root, "missing.gitconfig"))

	if _, err := gitGuardrailPaths(root); err == nil || !strings.Contains(err.Error(), "Git config include") {
		t.Fatalf("gitGuardrailPaths() error = %v, want missing writable include rejection", err)
	}
}

func TestGitGuardrailPathsResolvesIncludeRelativeToConfigOrigin(t *testing.T) {
	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	if err := os.Mkdir(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	global := filepath.Join(gitDir, "audit-global")
	if err := os.WriteFile(global, []byte("[include]\n\tpath = ../missing.gitconfig\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	isolateGitConfig(t)
	t.Setenv("GIT_CONFIG_GLOBAL", global)

	if _, err := gitGuardrailPaths(root); err == nil || !strings.Contains(err.Error(), "Git config include") {
		t.Fatalf("gitGuardrailPaths() error = %v, want origin-relative include rejection", err)
	}
}

func TestGitGuardrailPathsRejectsDanglingIncludeSymlinkIntoWorkspace(t *testing.T) {
	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	if err := os.Mkdir(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	global := filepath.Join(gitDir, "audit-global")
	if err := os.WriteFile(global, []byte("[include]\n\tpath = dangling-include\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "missing.gitconfig"), filepath.Join(gitDir, "dangling-include")); err != nil {
		t.Fatal(err)
	}
	isolateGitConfig(t)
	t.Setenv("GIT_CONFIG_GLOBAL", global)

	if _, err := gitGuardrailPaths(root); err == nil || !strings.Contains(err.Error(), "Git config include") {
		t.Fatalf("gitGuardrailPaths() error = %v, want dangling include route rejection", err)
	}
}

func TestGitGuardrailPathsRejectsHooksPathInNestedInactiveInclude(t *testing.T) {
	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	if err := os.Mkdir(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	global := filepath.Join(gitDir, "audit-global")
	inactive := filepath.Join(gitDir, "inactive")
	nested := filepath.Join(gitDir, "nested")
	if err := os.WriteFile(global, []byte("[includeIf \"onbranch:never-matches\"]\n\tpath = inactive\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inactive, []byte("[include]\n\tpath = nested\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nested, []byte("[core]\n\thooksPath = .githooks\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	isolateGitConfig(t)
	t.Setenv("GIT_CONFIG_GLOBAL", global)

	if _, err := gitGuardrailPaths(root); err == nil || !strings.Contains(err.Error(), "possible core.hooksPath") {
		t.Fatalf("gitGuardrailPaths() error = %v, want inactive nested hooks-path rejection", err)
	}
}

func TestGitGuardrailPathsRejectsOverriddenGlobalHooksPath(t *testing.T) {
	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	if err := os.MkdirAll(filepath.Join(gitDir, "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	global := filepath.Join(gitDir, "audit-global")
	if err := os.WriteFile(global, []byte("[core]\n\thooksPath = .githooks\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	isolateGitConfig(t)
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.hooksPath")
	t.Setenv("GIT_CONFIG_VALUE_0", ".git/hooks")

	if _, err := gitGuardrailPaths(root); err == nil || !strings.Contains(err.Error(), "possible core.hooksPath") {
		t.Fatalf("gitGuardrailPaths() error = %v, want overridden unsafe hooks-path rejection", err)
	}
}

func TestGitGuardrailPathsRejectsHardlinkedGlobalConfigSource(t *testing.T) {
	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	if err := os.Mkdir(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	global := filepath.Join(gitDir, "audit-global")
	if err := os.WriteFile(global, []byte("[user]\n\tname = test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(global, filepath.Join(root, "global-alias")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	isolateGitConfig(t)
	t.Setenv("GIT_CONFIG_GLOBAL", global)

	if _, err := gitGuardrailPaths(root); err == nil || !strings.Contains(err.Error(), "multiple hard links") {
		t.Fatalf("gitGuardrailPaths() error = %v, want hard-linked config-source rejection", err)
	}
}

func TestGitAuditEnvironmentDropsRoutingAndGitConfigOnlyOverride(t *testing.T) {
	t.Setenv("GIT_DIR", "/tmp/attacker-gitdir")
	t.Setenv("GIT_COMMON_DIR", "/tmp/attacker-common")
	t.Setenv("GIT_CONFIG", "/tmp/git-config-command-only")
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_COUNT", "0")
	t.Setenv("PATH", "/tmp/attacker-bin")

	got := make(map[string]string)
	for _, entry := range gitAuditEnvironment() {
		name, value, _ := strings.Cut(entry, "=")
		got[name] = value
	}
	for _, dropped := range []string{"GIT_DIR", "GIT_COMMON_DIR", "GIT_CONFIG", "PATH"} {
		if _, ok := got[dropped]; ok {
			t.Fatalf("gitAuditEnvironment() retained unsafe %s", dropped)
		}
	}
	for _, kept := range []string{"GIT_CONFIG_GLOBAL", "GIT_CONFIG_COUNT"} {
		if _, ok := got[kept]; !ok {
			t.Fatalf("gitAuditEnvironment() dropped effective config selector %s", kept)
		}
	}
}

func TestGitGuardrailPathsRejectsDarwinTempConfigSource(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin exposes host temp paths as writable; Linux provides a private tmpfs")
	}
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	global := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(global, []byte("[user]\n\tname = test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	isolateGitConfig(t)
	t.Setenv("GIT_CONFIG_GLOBAL", global)

	if _, err := gitGuardrailPaths(root); err == nil || !strings.Contains(err.Error(), "writable sandbox path") {
		t.Fatalf("gitGuardrailPaths() error = %v, want host-temp config rejection", err)
	}
}

func TestNewRevalidatesGitPolicyAfterWritablePathMerge(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(home) || filepath.Clean(home) == string(filepath.Separator) {
		t.Skipf("need a bounded absolute home directory, got %q", home)
	}

	for _, tc := range []struct {
		name string
		want string
		set  func(*testing.T, string, string)
	}{
		{
			name: "external global config from CLI write path",
			want: "global Git config source",
			set: func(t *testing.T, _, external string) {
				isolateGitConfig(t)
				t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(external, "global.gitconfig"))
			},
		},
		{
			name: "external include from tool overlay",
			want: "Git config include",
			set: func(t *testing.T, root, external string) {
				global := filepath.Join(root, ".git", "audit-global")
				contents := "[include]\n\tpath = " + filepath.Join(external, "included.gitconfig") + "\n"
				if err := os.WriteFile(global, []byte(contents), 0o600); err != nil {
					t.Fatal(err)
				}
				isolateGitConfig(t)
				t.Setenv("GIT_CONFIG_GLOBAL", global)
			},
		},
		{
			name: "external hooks path from tool overlay",
			want: "effective core.hooksPath",
			set: func(t *testing.T, _, external string) {
				setIsolatedGitConfigValue(t, "core.hooksPath", filepath.Join(external, "hooks"))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, ".git", "hooks"), 0o755); err != nil {
				t.Fatal(err)
			}
			external := filepath.Join(home, ".polly-sandbox-policy-"+filepath.Base(root))
			if _, err := os.Lstat(external); err == nil {
				t.Skipf("test target unexpectedly exists: %s", external)
			} else if !os.IsNotExist(err) {
				t.Fatal(err)
			}
			initialRoots, err := gitAuditWritableRoots(root)
			if err != nil {
				t.Fatal(err)
			}
			for _, initial := range initialRoots {
				if pathWithinPolicy(external, initial) {
					t.Skipf("home test target %q is already in implicit writable root %q", external, initial)
				}
			}

			tc.set(t, root, external)
			t.Chdir(root)
			cfg, err := ParsePreset("workspace")
			if err != nil {
				t.Fatalf("ParsePreset() rejected target before the later grant: %v", err)
			}
			// This is the same Merge used first for CLI --writepath and again for
			// a tool's own sandbox.writablePaths overlay.
			cfg = cfg.Merge(Config{WritablePaths: []string{home}})
			if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("New() error = %v, want final merged-policy %q rejection", err, tc.want)
			}
		})
	}
}

func TestGitHostWritablePathMatchesBackendTempSemantics(t *testing.T) {
	if runtime.GOOS == "linux" {
		for _, private := range []string{"/tmp", os.TempDir(), "/run"} {
			if gitHostWritablePath(private) {
				t.Fatalf("gitHostWritablePath(%q) = true, want private-root exclusion", private)
			}
			if !gitHostWritablePath(filepath.Join(private, "explicit-child")) {
				t.Fatalf("gitHostWritablePath(%q child) = false, want later host bind", private)
			}
		}
		if !gitHostWritablePath(string(filepath.Separator)) {
			t.Fatal("wider root bind must be audited because it re-exposes private descendants")
		}
		return
	}
	if !gitHostWritablePath(os.TempDir()) {
		t.Fatal("Darwin temp grants refer to the host filesystem and must be audited")
	}
}

func TestValidateConfiguredHooksPathAcceptsDevNull(t *testing.T) {
	if err := validateConfiguredHooksPath("/dev/null", "effective core.hooksPath", []string{t.TempDir()}, nil); err != nil {
		t.Fatalf("validateConfiguredHooksPath(/dev/null) error = %v", err)
	}
}

func TestGitGuardrailPathsAcceptsDevNullHooksPath(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	setIsolatedGitConfigValue(t, "core.hooksPath", "/dev/null")

	if _, err := gitGuardrailPaths(root); err != nil {
		t.Fatalf("gitGuardrailPaths() rejected hook-disabling /dev/null: %v", err)
	}
}

func setIsolatedGitConfigValue(t *testing.T, key, value string) {
	t.Helper()
	isolateGitConfig(t)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", key)
	t.Setenv("GIT_CONFIG_VALUE_0", value)
}

func isolateGitConfig(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_COUNT", "0")
	unsetTestEnv(t, "GIT_CONFIG_PARAMETERS")
}

func unsetTestEnv(t *testing.T, name string) {
	t.Helper()
	value, set := os.LookupEnv(name)
	if err := os.Unsetenv(name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if set {
			_ = os.Setenv(name, value)
		} else {
			_ = os.Unsetenv(name)
		}
	})
}

func caseVariedWorkspace(t *testing.T) (string, string) {
	t.Helper()
	parent := t.TempDir()
	root := filepath.Join(parent, "CaseWorkspace")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	variant := filepath.Join(parent, "caseworkspace")
	rootInfo, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	variantInfo, err := os.Stat(variant)
	if err != nil || !os.SameFile(rootInfo, variantInfo) {
		t.Skip("test volume is case-sensitive")
	}
	return root, variant
}

func TestGitGuardrailPathsRejectsHardlinkedProtectedFiles(t *testing.T) {
	t.Run("routing file", func(t *testing.T) {
		root := t.TempDir()
		worktree := filepath.Join(root, "worktree")
		gitDir := filepath.Join(root, "metadata")
		if err := os.MkdirAll(worktree, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(gitDir, 0o755); err != nil {
			t.Fatal(err)
		}
		routing := filepath.Join(worktree, ".git")
		if err := os.WriteFile(routing, []byte("gitdir: ../metadata\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(routing, filepath.Join(root, "routing-alias")); err != nil {
			t.Skipf("hard links unavailable: %v", err)
		}
		if _, err := gitGuardrailPaths(root); err == nil || !strings.Contains(err.Error(), "multiple hard links") {
			t.Fatalf("gitGuardrailPaths() error = %v, want hard-link routing failure", err)
		}
	})

	t.Run("commondir pointer", func(t *testing.T) {
		root := t.TempDir()
		worktree := filepath.Join(root, "worktree")
		gitDir := filepath.Join(root, "metadata", "worktrees", "wt")
		common := filepath.Join(root, "metadata")
		if err := os.MkdirAll(worktree, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(gitDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: ../metadata/worktrees/wt\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		pointer := filepath.Join(gitDir, "commondir")
		if err := os.WriteFile(pointer, []byte("../..\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(pointer, filepath.Join(common, "commondir-alias")); err != nil {
			t.Skipf("hard links unavailable: %v", err)
		}
		if _, err := gitGuardrailPaths(root); err == nil || !strings.Contains(err.Error(), "multiple hard links") {
			t.Fatalf("gitGuardrailPaths() error = %v, want hard-link commondir failure", err)
		}
	})

	for _, name := range []string{"config", "hook"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			gitDir := filepath.Join(root, ".git")
			hooks := filepath.Join(gitDir, "hooks")
			if err := os.MkdirAll(hooks, 0o755); err != nil {
				t.Fatal(err)
			}
			protected := filepath.Join(gitDir, "config")
			if name == "hook" {
				protected = filepath.Join(hooks, "pre-commit")
			}
			if err := os.WriteFile(protected, []byte("protected\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(protected, filepath.Join(root, name+"-alias")); err != nil {
				t.Skipf("hard links unavailable: %v", err)
			}
			if _, err := gitGuardrailPaths(root); err == nil || !strings.Contains(err.Error(), "multiple hard links") {
				t.Fatalf("gitGuardrailPaths() error = %v, want hard-link %s failure", err, name)
			}
		})
	}
}

func TestGitGuardrailPathsRejectsBareRepositoryRoot(t *testing.T) {
	for _, value := range []string{"true", "yes", "on", "1"} {
		t.Run(value, func(t *testing.T) {
			root := t.TempDir()
			makeBareGitRepository(t, root)
			if err := os.WriteFile(filepath.Join(root, "config"), []byte("[core]\n\tbare = "+value+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			if _, err := gitGuardrailPaths(root); err == nil || !strings.Contains(err.Error(), "bare Git repository") {
				t.Fatalf("gitGuardrailPaths() error = %v, want bare-workspace failure", err)
			}
		})
	}
}

func TestGitGuardrailPathsProtectsNestedBareRepository(t *testing.T) {
	root := t.TempDir()
	bare := filepath.Join(root, "mirrors", "project.git")
	makeBareGitRepository(t, bare)

	got, err := gitGuardrailPaths(root)
	if err != nil {
		t.Fatalf("gitGuardrailPaths() error = %v", err)
	}
	realBare := mustEvalSymlinks(t, bare)
	if !slices.Contains(got, realBare) {
		t.Fatalf("gitGuardrailPaths() = %v, want nested bare repository %q", got, realBare)
	}
}

func makeBareGitRepository(t *testing.T, dir string) {
	t.Helper()
	for _, child := range []string{"objects", "refs"} {
		if err := os.MkdirAll(filepath.Join(dir, child), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config"), []byte("[core]\n\tbare = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustEvalSymlinks(t *testing.T, path string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

func protectedByAny(paths []string, target string) bool {
	for _, path := range paths {
		rel, err := filepath.Rel(path, target)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
