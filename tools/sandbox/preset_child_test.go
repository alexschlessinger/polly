package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseChildPresetGitGuards(t *testing.T) {
	skipIfWindows(t)
	isolateGitConfig(t)
	for _, missing := range []string{"", "hooks", "config", "config.worktree"} {
		t.Run("missing="+missing, func(t *testing.T) {
			root := mustEvalSymlinks(t, t.TempDir())
			// Include nested repositories, not only the checkout's own .git.
			gitDir := makeLeafTestRepo(t, filepath.Join(root, "nested"))
			if missing == "config.worktree" {
				if err := os.WriteFile(filepath.Join(gitDir, "config"), []byte("[extensions]\nworktreeConfig = true\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else if missing != "" {
				if err := os.RemoveAll(filepath.Join(gitDir, missing)); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.Stat(gitDir)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := ParseChildPresetAtRoot("workspace+git", root)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err = PrepareConfig(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := WriteAllowed(cfg, filepath.Join(root, "file")); err != nil {
				t.Fatalf("lost workspace writes: %v", err)
			}
			if got := WriteAllowed(cfg, filepath.Join(gitDir, "index")) == nil; got != (missing == "") {
				t.Fatalf("Git metadata writable = %v, want %v", got, missing == "")
			}
			for _, leaf := range []string{"config", "hooks"} {
				if WriteAllowed(cfg, filepath.Join(gitDir, leaf)) == nil {
					t.Fatalf("unprotected Git guard %s", leaf)
				}
			}
			if missing != "" {
				if _, err := os.Lstat(filepath.Join(gitDir, missing)); !os.IsNotExist(err) {
					t.Fatalf("child created %s: %v", missing, err)
				}
			}
			after, err := os.Stat(gitDir)
			if err != nil || !before.ModTime().Equal(after.ModTime()) {
				t.Fatalf("child mutated Git directory: %v", err)
			}
			// Trusted parent parsing retains its existing materialization behavior.
			if missing != "" {
				if _, err := ParsePresetAtRoot("workspace+git", root); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Lstat(filepath.Join(gitDir, missing)); err != nil {
					t.Fatalf("parent did not materialize %s: %v", missing, err)
				}
			}
		})
	}
}

func TestParseChildPresetFailureDoesNotMaterializeGitGuards(t *testing.T) {
	skipIfWindows(t)
	isolateGitConfig(t)
	root := mustEvalSymlinks(t, t.TempDir())
	gitDir := makeLeafTestRepo(t, filepath.Join(root, "nested"))
	hooks := filepath.Join(gitDir, "hooks")
	if err := os.RemoveAll(hooks); err != nil {
		t.Fatal(err)
	}
	// This passes discovery but fails the final hook/config authority audit.
	if err := os.WriteFile(filepath.Join(gitDir, "config"), []byte("[core]\nhooksPath = ../unsafe-hooks\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseChildPresetAtRoot("workspace+git", root); err == nil {
		t.Fatal("accepted writable hook target")
	}
	if _, err := os.Lstat(hooks); !os.IsNotExist(err) {
		t.Fatalf("failed child policy materialized hooks: %v", err)
	}
	if _, err := ParseChildPresetAtRoot("workspace", "relative"); err == nil {
		t.Fatal("accepted relative root")
	}
}
