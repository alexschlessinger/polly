package worktree

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

func TestRetainCommitPreservesIdentityHistoryAndParent(t *testing.T) {
	m, root := fixture(t)
	ctx := context.Background()
	first := strings.TrimSpace(string(gitTest(t, root, "rev-parse", "HEAD")))
	writeTest(t, filepath.Join(root, "a.txt"), "committed change\n")
	gitTest(t, root, "add", ".")
	gitTest(t, root, "commit", "-qm", "second")
	commit := strings.TrimSpace(string(gitTest(t, root, "rev-parse", "HEAD")))
	writeTest(t, filepath.Join(root, "a.txt"), "dirty parent\n")
	writeTest(t, filepath.Join(root, "untracked.txt"), "untracked\n")
	index, err := os.ReadFile(filepath.Join(root, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := m.RetainCommit(ctx, root, strings.ToUpper(commit))
	if err != nil {
		t.Fatal(err)
	}
	if s.Commit != commit || s.Source != m.Root || s.Tree != strings.TrimSpace(string(gitTest(t, root, "rev-parse", commit+"^{tree}"))) {
		t.Fatalf("changed identity: %+v", s)
	}
	c, err := m.Create(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(gitTest(t, c.Path, "rev-parse", "HEAD^"))); got != first {
		t.Fatalf("lost ancestry: %s", got)
	}
	if data, _ := os.ReadFile(filepath.Join(c.Path, "a.txt")); string(data) != "committed change\n" {
		t.Fatalf("wrong selected content: %q", data)
	}
	if _, err := os.Stat(filepath.Join(c.Path, "untracked.txt")); !os.IsNotExist(err) {
		t.Fatal("explicit commit included untracked parent files")
	}
	after, _ := os.ReadFile(filepath.Join(root, ".git", "index"))
	if !bytes.Equal(index, after) || strings.TrimSpace(string(gitTest(t, root, "rev-parse", "HEAD"))) != commit {
		t.Fatal("modified parent HEAD/index")
	}
	if data, _ := os.ReadFile(filepath.Join(root, "a.txt")); string(data) != "dirty parent\n" {
		t.Fatal("modified dirty parent")
	}
	// Automatic capture still includes dirty/untracked files.
	auto, err := m.Capture(ctx, root)
	if err != nil || auto.Tree == s.Tree || string(gitTest(t, root, "show", auto.Commit+":untracked.txt")) != "untracked\n" {
		t.Fatalf("automatic capture changed: %+v %v", auto, err)
	}
	if err := m.CleanupSnapshotRefs(ctx); err != nil {
		t.Fatal(err)
	}
	if refs := gitTest(t, root, "for-each-ref", "refs/polly/snapshots"); len(refs) != 0 {
		t.Fatalf("leaked refs: %s", refs)
	}
	gitTest(t, root, "cat-file", "-e", commit)
}

func TestRetainCommitRejectsInvalidObjectsAndSources(t *testing.T) {
	m, root := fixture(t)
	ctx := context.Background()
	commit := strings.TrimSpace(string(gitTest(t, root, "rev-parse", "HEAD")))
	tree := strings.TrimSpace(string(gitTest(t, root, "rev-parse", "HEAD^{tree}")))
	gitTest(t, root, "tag", "-am", "annotated", "annotated")
	tag := strings.TrimSpace(string(gitTest(t, root, "rev-parse", "annotated")))
	for _, invalid := range []string{"", "HEAD", "HEAD^{commit}", commit[:12], tree, tag, strings.Repeat("f", 40)} {
		if _, err := m.RetainCommit(ctx, root, invalid); err == nil {
			t.Errorf("accepted %q", invalid)
		}
	}
	foreign := t.TempDir()
	gitTest(t, foreign, "init", "-q")
	if _, err := m.RetainCommit(ctx, foreign, commit); err == nil {
		t.Fatal("accepted foreign repository source")
	}
	linked := filepath.Join(t.TempDir(), "linked")
	gitTest(t, root, "worktree", "add", "--detach", linked, commit)
	if _, err := m.RetainCommit(ctx, linked, commit); err != nil {
		t.Fatal("refused authorized linked checkout:", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := m.RetainCommit(canceled, root, commit); err == nil {
		t.Fatal("accepted canceled retention")
	}
}

func TestRetainCommitChecksHistoricalContentPolicy(t *testing.T) {
	for _, kind := range []string{"private", "denied", "symlink", "filter", "submodule"} {
		t.Run(kind, func(t *testing.T) {
			m, root := fixture(t)
			ctx := context.Background()
			writeTest(t, filepath.Join(root, "secret.txt"), "secret\n")
			if kind == "filter" {
				writeTest(t, filepath.Join(root, ".gitattributes"), "secret.txt filter=example\n")
			}
			if kind == "symlink" {
				if err := os.Symlink("secret.txt", filepath.Join(root, "link")); err != nil {
					t.Fatal(err)
				}
				gitTest(t, root, "add", "link")
			} else {
				gitTest(t, root, "add", ".")
			}
			if kind == "submodule" {
				base := strings.TrimSpace(string(gitTest(t, root, "rev-parse", "HEAD")))
				gitTest(t, root, "update-index", "--add", "--cacheinfo", "160000,"+base+",module")
			}
			gitTest(t, root, "commit", "-qm", "historical contents")
			commit := strings.TrimSpace(string(gitTest(t, root, "rev-parse", "HEAD")))
			// Policy checks must use the selected tree even after HEAD and
			// its working files no longer contain these paths/attributes.
			gitTest(t, root, "reset", "--hard", "HEAD^")
			if kind == "private" {
				m.privatePaths = []string{"secret.txt"}
			}
			if kind == "denied" || kind == "symlink" {
				registry := tools.NewToolRegistry(nil, tools.WithSandboxFactory(sandbox.New, sandbox.Config{DenyPaths: []string{filepath.Join(root, "secret.txt")}}))
				defer registry.Close()
				m.Registry = registry
			}
			if _, err := m.RetainCommit(ctx, root, commit); err == nil {
				t.Fatal("admitted prohibited historical contents")
			}
			if refs := gitTest(t, root, "for-each-ref", "refs/polly/snapshots"); len(refs) != 0 {
				t.Fatalf("pinned rejected commit: %s", refs)
			}
		})
	}
}

func TestRetainCommitIgnoresReplacementObjects(t *testing.T) {
	m, root := fixture(t)
	original := strings.TrimSpace(string(gitTest(t, root, "rev-parse", "HEAD")))
	writeTest(t, filepath.Join(root, "a.txt"), "replacement\n")
	gitTest(t, root, "add", ".")
	gitTest(t, root, "commit", "-qm", "replacement")
	replacement := strings.TrimSpace(string(gitTest(t, root, "rev-parse", "HEAD")))
	gitTest(t, root, "replace", original, replacement)
	s, err := m.RetainCommit(context.Background(), root, original)
	if err != nil {
		t.Fatal(err)
	}
	c, err := m.Create(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(c.Path, "a.txt")); string(data) != "one\ntwo\nthree\n" {
		t.Fatalf("replacement changed selected contents: %q", data)
	}
}

func TestRetainCommitSandboxedWorktree(t *testing.T) {
	if os.Getenv("POLLYTOOL_REQUIRE_SANDBOX_TESTS") != "1" {
		t.Skip("opt-in process sandbox")
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("sandbox platform")
	}
	for _, preset := range []string{"workspace", "workspace+git"} {
		t.Run(preset, func(t *testing.T) {
			original, root := fixture(t)
			t.Chdir(root)
			cfg, err := sandbox.ParsePreset(preset)
			if err != nil {
				t.Fatal(err)
			}
			registry := tools.NewToolRegistry(nil, tools.WithSandboxFactory(sandbox.New, cfg))
			defer registry.Close()
			ctx := context.Background()
			m, err := New(ctx, Config{Root: root, Directory: original.Directory, Registry: registry, MaxWorktrees: 2})
			if err != nil {
				t.Fatal(err)
			}
			commit := strings.TrimSpace(string(gitTest(t, root, "rev-parse", "HEAD")))
			s, err := m.RetainCommit(ctx, root, commit)
			if err != nil {
				t.Fatal(err)
			}
			c, err := m.Create(ctx, s)
			if err != nil {
				t.Fatal(err)
			}
			if data, _ := os.ReadFile(filepath.Join(c.Path, "a.txt")); string(data) != "one\ntwo\nthree\n" {
				t.Fatalf("sandboxed contents = %q", data)
			}
			if err := m.ValidateSnapshot(ctx, s); err != nil {
				t.Fatal(err)
			}
			// A member checkout lives outside the parent's writable roots; its
			// commits are still retainable under the parent policy.
			memberCommit := strings.TrimSpace(string(gitTest(t, c.Path, "rev-parse", "HEAD")))
			if _, err := m.RetainCommit(ctx, c.Path, memberCommit); err != nil {
				t.Fatalf("retain from a member checkout: %v", err)
			}
		})
	}
}
