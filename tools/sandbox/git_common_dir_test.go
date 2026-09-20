package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGitCommonDir(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mkdir := func(parts ...string) string {
		t.Helper()
		dir := filepath.Join(append([]string{root}, parts...)...)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	write := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// An ordinary checkout, asked from its root and from a subdirectory.
	repo := mkdir("repo")
	common := mkdir("repo", ".git")
	sub := mkdir("repo", "src", "pkg")
	// A linked worktree routes through its own Git directory, whose
	// commondir pointer names the main repository's.
	linked := mkdir("linked")
	worktreeGitDir := mkdir("repo", ".git", "worktrees", "linked")
	write(filepath.Join(linked, ".git"), "gitdir: "+worktreeGitDir+"\n")
	write(filepath.Join(worktreeGitDir, "commondir"), "../..\n")
	// A submodule's .git file routes to a Git directory with no commondir.
	module := mkdir("repo", "vendor", "module")
	moduleGitDir := mkdir("repo", ".git", "modules", "module")
	write(filepath.Join(module, ".git"), "gitdir: ../../.git/modules/module\n")
	outside := mkdir("outside")

	for _, tc := range []struct {
		name, dir, want string
	}{
		{"root", repo, common},
		{"subdirectory", sub, common},
		{"linked worktree", linked, common},
		{"submodule", module, moduleGitDir},
	} {
		got, err := GitCommonDir(tc.dir)
		if err != nil || got != tc.want {
			t.Errorf("%s: GitCommonDir = %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
	// Outside every repository there is no common directory. The walk goes
	// up to the filesystem root, so this holds only when no ancestor of the
	// temp directory is a checkout.
	if got, err := GitCommonDir(outside); err != nil {
		t.Errorf("outside: GitCommonDir error %v", err)
	} else if got != "" && !PathWithin(root, filepath.Dir(got)) {
		t.Errorf("outside: GitCommonDir = %q, want none", got)
	}

	// A .git file that routes nowhere is an error, not a repository.
	broken := mkdir("broken")
	write(filepath.Join(broken, ".git"), "")
	if got, err := GitCommonDir(broken); err == nil {
		t.Errorf("empty .git file: GitCommonDir = %q, want an error", got)
	}
}
