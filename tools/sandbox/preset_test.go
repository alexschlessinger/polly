package sandbox

import (
	"os"
	"path/filepath"
	"slices"
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
	for _, want := range []string{
		filepath.Join(cwd, ".git", "hooks"),
		filepath.Join(cwd, ".git", "config"),
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

	got := gitGuardrailPaths(wt)
	for _, want := range []string{
		filepath.Join(wtGitDir, "hooks"),
		filepath.Join(wtGitDir, "config"),
		filepath.Join(mainGit, "hooks"),
		filepath.Join(mainGit, "config"),
	} {
		if !slices.Contains(got, want) {
			t.Fatalf("gitGuardrailPaths() = %v, want to include %q", got, want)
		}
	}
}
