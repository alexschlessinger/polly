package worktree

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/tools/docker/protocol"
)

func tarOf(t *testing.T, entries ...tar.Header) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, header := range entries {
		content := header.PAXRecords["content"]
		header.PAXRecords = nil
		header.Size = int64(len(content))
		if header.Typeflag == 0 {
			header.Typeflag = tar.TypeReg
		}
		if err := tw.WriteHeader(&header); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

func file(name, content string, mode int64) tar.Header {
	return tar.Header{Name: name, Mode: mode, PAXRecords: map[string]string{"content": content}}
}

func TestBaseIsParentlessAndDeterministic(t *testing.T) {
	m, root := fixture(t)
	ctx := context.Background()
	writeTest(t, filepath.Join(root, "b.txt"), "second\n")
	gitTest(t, root, "add", "b.txt")
	gitTest(t, root, "commit", "-qm", "second")
	// The live tree's HEAD has history, so its base is a fresh parentless
	// commit of the same tree, the same commit every time.
	first, tree, err := m.Base(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	head := strings.TrimSpace(string(gitTest(t, root, "rev-parse", "HEAD")))
	headTree := strings.TrimSpace(string(gitTest(t, root, "rev-parse", "HEAD^{tree}")))
	if first == head || tree != headTree {
		t.Fatalf("base %s tree %s (HEAD %s tree %s)", first, tree, head, headTree)
	}
	if parents := strings.Fields(string(gitTest(t, root, "rev-list", "--parents", "-n1", first))); len(parents) != 1 {
		t.Fatalf("base has parents: %v", parents)
	}
	second, _, err := m.Base(ctx, root)
	if err != nil || second != first {
		t.Fatalf("base changed between calls: %s %s %v", first, second, err)
	}
	// A member checkout starts at a parentless snapshot commit, which is its
	// base as it stands.
	snapshot, err := m.Capture(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	checkout, err := m.Create(ctx, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	commit, tree, err := m.Base(ctx, checkout.Path)
	if err != nil || commit != snapshot.Commit || tree != snapshot.Tree {
		t.Fatalf("checkout base %s %s %v, want %s %s", commit, tree, err, snapshot.Commit, snapshot.Tree)
	}
}

func TestWriteBundleFetchesIntoAFreshRepository(t *testing.T) {
	m, root := fixture(t)
	ctx := context.Background()
	commit, _, err := m.Base(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	var bundle bytes.Buffer
	if err := m.WriteBundle(ctx, root, commit, &bundle); err != nil {
		t.Fatal(err)
	}
	copyDir := t.TempDir()
	bundlePath := filepath.Join(t.TempDir(), "base.bundle")
	if err := os.WriteFile(bundlePath, bundle.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, copyDir, "init", "-q")
	gitTest(t, copyDir, "fetch", "-q", bundlePath, protocol.BundleRef(commit))
	gitTest(t, copyDir, "checkout", "-q", "--detach", commit)
	if refs := gitTest(t, root, "for-each-ref", "refs/polly/copy-base"); len(strings.TrimSpace(string(refs))) != 0 {
		t.Fatalf("bundle reference retained: %s", refs)
	}
	if content, err := os.ReadFile(filepath.Join(copyDir, "a.txt")); err != nil || string(content) != "one\ntwo\nthree\n" {
		t.Fatalf("copy content %q %v", content, err)
	}
	if log := strings.TrimSpace(string(gitTest(t, copyDir, "rev-list", "--count", "HEAD"))); log != "1" {
		t.Fatalf("copy received history: %s commits", log)
	}
}

func TestDivergenceAndImportRoundTrip(t *testing.T) {
	m, root := fixture(t)
	ctx := context.Background()
	m.privatePaths = append(m.privatePaths, "private.txt")
	commit, tree, err := m.Base(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	writeTest(t, filepath.Join(root, "a.txt"), "changed\n")
	writeTest(t, filepath.Join(root, "new.txt"), "new\n")
	writeTest(t, filepath.Join(root, "private.txt"), "secret\n")
	if err := os.MkdirAll(filepath.Join(root, "nested", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTest(t, filepath.Join(root, "nested", "deep", "leaf.txt"), "leaf\n")
	changed, deleted, err := m.Divergence(ctx, root, tree)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(changed)
	if !slices.Equal(changed, []string{"a.txt", "nested/deep/leaf.txt", "new.txt"}) || len(deleted) != 0 {
		t.Fatalf("divergence changed %v deleted %v", changed, deleted)
	}
	if status := gitTest(t, root, "status", "--porcelain"); strings.Contains(string(status), "A ") {
		t.Fatalf("divergence touched the live index:\n%s", status)
	}
	if err := os.Remove(filepath.Join(root, "a.txt")); err != nil {
		t.Fatal(err)
	}
	if _, deleted, err = m.Divergence(ctx, root, tree); err != nil || !slices.Equal(deleted, []string{"a.txt"}) {
		t.Fatalf("deleted %v %v", deleted, err)
	}
	_ = commit

	// Import: files with modes, a symlink, then deletions that prune empty
	// directories.
	changes := tarOf(t,
		tar.Header{Typeflag: tar.TypeDir, Name: "made/", Mode: 0o755},
		file("made/script.sh", "#!/bin/sh\n", 0o755),
		file("new.txt", "from the copy\n", 0o644),
		tar.Header{Typeflag: tar.TypeSymlink, Name: "link", Linkname: "new.txt", Mode: 0o777},
	)
	if err := m.ImportChanges(ctx, root, changes, []string{"nested/deep/leaf.txt"}); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(root, "made", "script.sh")); err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("imported script %v %v", info, err)
	}
	if content, _ := os.ReadFile(filepath.Join(root, "new.txt")); string(content) != "from the copy\n" {
		t.Fatalf("imported file %q", content)
	}
	if target, err := os.Readlink(filepath.Join(root, "link")); err != nil || target != "new.txt" {
		t.Fatalf("imported symlink %q %v", target, err)
	}
	if _, err := os.Stat(filepath.Join(root, "nested")); !os.IsNotExist(err) {
		t.Fatal("deletion did not prune the emptied directory")
	}
	if err := m.ImportChanges(ctx, root, tarOf(t), []string{"private.txt"}); err == nil {
		t.Fatal("deletion of a private path was accepted")
	}
	if _, err := os.Stat(filepath.Join(root, "private.txt")); err != nil {
		t.Fatal("private file removed")
	}
}

func TestImportChangesRefusesEscapesAndUnownedRoots(t *testing.T) {
	m, root := fixture(t)
	ctx := context.Background()
	for name, changes := range map[string]*bytes.Buffer{
		"parent escape": tarOf(t, file("../escape.txt", "x", 0o644)),
		"absolute":      tarOf(t, file("/etc/passwd", "x", 0o644)),
		"git metadata":  tarOf(t, file(".git/hooks/pre-commit", "x", 0o755)),
		"device":        tarOf(t, tar.Header{Typeflag: tar.TypeChar, Name: "dev", Mode: 0o644}),
		"private":       tarOf(t, file("private.txt", "x", 0o644)),
	} {
		m.privatePaths = []string{"private.txt"}
		if err := m.ImportChanges(ctx, root, changes, nil); err == nil {
			t.Fatalf("%s was imported", name)
		}
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "outside")); err != nil {
		t.Fatal(err)
	}
	if err := m.ImportChanges(ctx, root, tarOf(t, file("outside/planted.txt", "x", 0o644)), nil); err == nil {
		t.Fatal("a write through a symlinked directory was accepted")
	}
	if err := m.ImportChanges(ctx, root, tarOf(t), []string{"../elsewhere"}); err == nil {
		t.Fatal("a deletion outside the worktree was accepted")
	}
	if err := m.ImportChanges(ctx, t.TempDir(), tarOf(t, file("x.txt", "x", 0o644)), nil); err == nil {
		t.Fatal("an unowned root accepted an import")
	}
	if len(m.privatePaths) == 0 {
		t.Fatal("fixture")
	}
}
