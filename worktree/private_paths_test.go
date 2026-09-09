package worktree

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCaptureExcludesPrivateFilesAndDirectoriesBeforeStaging(t *testing.T) {
	m, root := fixture(t)
	ctx := context.Background()
	privateDir := filepath.Join(root, "private[1]")
	if err := os.Mkdir(privateDir, 0700); err != nil {
		t.Fatal(err)
	}
	writeTest(t, filepath.Join(root, "tracked.db"), "already in history\n")
	writeTest(t, filepath.Join(privateDir, "tracked"), "old directory content\n")
	gitTest(t, root, "add", ".")
	gitTest(t, root, "commit", "-qm", "tracked private fixture")
	index, err := os.ReadFile(filepath.Join(root, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	head := gitTest(t, root, "rev-parse", "HEAD")
	config := m.Config
	config.PrivatePaths = []string{filepath.Join(root, "tracked.db"), filepath.Join(root, "untracked.db"), privateDir, filepath.Join(root, "future.db")}
	m, err = New(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	private := map[string]string{
		"tracked.db":         "private tracked change\n",
		"untracked.db":       "private untracked content\n",
		"private[1]/tracked": "private directory tracked change\n",
		"private[1]/new":     "private directory untracked content\n",
		"future.db":          "private path created after policy construction\n",
	}
	for name, value := range private {
		writeTest(t, filepath.Join(root, name), value)
	}
	// A literal exclusion must not treat brackets as a glob.
	writeTest(t, filepath.Join(root, "private1"), "public file\n")
	snapshot, err := m.Capture(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	listing := string(gitTest(t, root, "ls-tree", "-r", "--name-only", snapshot.Tree))
	if listing != "a.txt\nprivate1\n" {
		t.Fatalf("snapshot paths: %q", listing)
	}
	for name, value := range private {
		hash, err := m.git(ctx, root, nil, []byte(value), "hash-object", "--stdin")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := m.git(ctx, root, nil, nil, "cat-file", "-e", strings.TrimSpace(string(hash))); err == nil {
			t.Errorf("capture wrote private blob for %s", name)
		}
	}
	after, err := os.ReadFile(filepath.Join(root, ".git", "index"))
	if err != nil || !bytes.Equal(index, after) || !bytes.Equal(head, gitTest(t, root, "rev-parse", "HEAD")) {
		t.Fatal("private exclusions changed parent index or HEAD", err)
	}
	child, err := m.Create(ctx, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	writeTest(t, filepath.Join(child.Path, "tracked.db"), "private content in member copy\n")
	again, err := m.Capture(ctx, child.Path)
	if err != nil {
		t.Fatal(err)
	}
	if again.Tree != snapshot.Tree {
		t.Fatal("private exclusion did not follow member checkout")
	}
}
