package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExposeCheckoutGitLinkedWorktree(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(dir, "home")
	repo := filepath.Join(home, "repo")
	common := filepath.Join(repo, ".git")
	gitDir := filepath.Join(common, "worktrees", "linked")
	linked := filepath.Join(dir, "linked")
	mustMkdirAll(t, gitDir, filepath.Join(common, "objects"), linked)
	for path, content := range map[string]string{
		filepath.Join(linked, ".git"):       "gitdir: " + gitDir + "\n",
		filepath.Join(gitDir, "commondir"):  "../..\n",
		filepath.Join(common, "config"):     "[core]\n",
		filepath.Join(repo, "README"):       "main checkout\n",
		filepath.Join(dir, "plain", "keep"): "",
	} {
		mustMkdirAll(t, filepath.Dir(path))
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	modelPrivateRoots(t, home)

	cfg, err := ExposeCheckoutGit(Config{}, linked)
	if err != nil {
		t.Fatalf("ExposeCheckoutGit = %v", err)
	}
	for _, path := range []string{gitDir, filepath.Join(common, "config"), filepath.Join(common, "objects")} {
		if err := ReadAllowed(cfg, path); err != nil {
			t.Errorf("ReadAllowed(%s) = %v, want the worktree's Git metadata readable", path, err)
		}
	}
	if err := ReadAllowed(cfg, filepath.Join(repo, "README")); err == nil {
		t.Error("ReadAllowed(main checkout) = nil, want only its .git exposed")
	}
	if err := WriteAllowed(cfg, filepath.Join(common, "config")); err == nil {
		t.Error("WriteAllowed(common config) = nil, want the exposure read-only")
	}

	// A denied path wins over the exposure.
	if _, err := ExposeCheckoutGit(Config{DenyPaths: []string{repo}}, linked); err == nil {
		t.Error("ExposeCheckoutGit under a denied repository = nil error, want the mask named")
	}

	// A checkout whose metadata tools already read is left alone.
	plain := filepath.Join(dir, "plain")
	mustMkdirAll(t, filepath.Join(plain, ".git"))
	cfg, err = ExposeCheckoutGit(Config{}, plain)
	if err != nil || len(cfg.visiblePaths) != 0 {
		t.Errorf("ExposeCheckoutGit(readable checkout) = %v, %v; want nothing exposed", cfg.visiblePaths, err)
	}
}
