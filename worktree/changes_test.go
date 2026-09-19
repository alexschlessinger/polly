package worktree

import (
	"context"
	"crypto/sha256"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

// changeFixture is a committed repository plus a tracker over it with an
// unsandboxed registry; sandboxed coverage is in TestChangeTrackerSandboxed.
func changeFixture(t *testing.T, limits ChangeLimits, privatePaths ...string) (*ChangeTracker, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX Git fixture")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := t.TempDir()
	root := filepath.Join(dir, "source")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	gitTest(t, root, "init", "-q")
	gitTest(t, root, "config", "user.name", "test")
	gitTest(t, root, "config", "user.email", "test@example.invalid")
	writeTest(t, filepath.Join(root, "a.txt"), "one\ntwo\nthree\n")
	writeTest(t, filepath.Join(root, ".gitignore"), "ignored.txt\n")
	gitTest(t, root, "add", ".")
	gitTest(t, root, "commit", "-qm", "base")
	registry := tools.NewToolRegistry(nil, tools.WithUnsafeNoSandbox())
	t.Cleanup(func() { registry.Close() })
	tracker, err := NewChangeTracker(registry, filepath.Join(dir, "changes"), privatePaths, limits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tracker.Close() })
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return tracker, root
}

func snapshotTest(t *testing.T, tracker *ChangeTracker, dir string) string {
	t.Helper()
	token, ok, reason, err := tracker.Snapshot(context.Background(), dir)
	if err != nil || !ok {
		t.Fatalf("snapshot: ok=%v reason=%q err=%v", ok, reason, err)
	}
	return token
}

func changesTest(t *testing.T, tracker *ChangeTracker, dir, token string) tools.FileChanges {
	t.Helper()
	changes, err := tracker.Changes(context.Background(), dir, token)
	if err != nil {
		t.Fatal(err)
	}
	return changes
}

// repoState fingerprints the repository's own metadata that a tracker must
// never touch: index bytes and mtime, refs, and loose object directories.
func repoState(t *testing.T, root string) string {
	t.Helper()
	index, err := os.ReadFile(filepath.Join(root, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(root, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	refs := gitTest(t, root, "for-each-ref")
	var objects []string
	filepath.WalkDir(filepath.Join(root, ".git", "objects"), func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			objects = append(objects, path)
		}
		return nil
	})
	sum := sha256.Sum256(index)
	return string(sum[:]) + info.ModTime().String() + string(refs) + strings.Join(objects, "\n")
}

func TestChangeTrackerReportsModifiedCreatedAndDeleted(t *testing.T) {
	tracker, root := changeFixture(t, ChangeLimits{})
	before := repoState(t, root)
	token := snapshotTest(t, tracker, root)
	writeTest(t, filepath.Join(root, "a.txt"), "one\n2\nthree\n")
	writeTest(t, filepath.Join(root, "new.txt"), "fresh\nfile\n")
	writeTest(t, filepath.Join(root, "ignored.txt"), "never\n")
	if err := os.Mkdir(filepath.Join(root, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	writeTest(t, filepath.Join(root, "sub", "deep.txt"), "x\n")
	changes := changesTest(t, tracker, root, token)
	if !changes.Tracked || changes.Root != root || len(changes.Changes) != 3 {
		t.Fatalf("changes: %+v", changes)
	}
	a, n, d := changes.Changes[0], changes.Changes[1], changes.Changes[2]
	if a.Path != "a.txt" || a.Kind != tools.ChangeModified || a.Additions != 1 || a.Deletions != 1 || !strings.Contains(a.Diff, "--- a/a.txt\n+++ b/a.txt\n") || !strings.Contains(a.Diff, "-two\n+2\n") {
		t.Fatalf("modified: %+v", a)
	}
	if n.Path != "new.txt" || n.Kind != tools.ChangeCreated || n.Additions != 2 || !strings.HasPrefix(n.Diff, "--- /dev/null\n") {
		t.Fatalf("created: %+v", n)
	}
	if d.Path != "sub/deep.txt" || d.Kind != tools.ChangeCreated {
		t.Fatalf("nested created: %+v", d)
	}
	token = snapshotTest(t, tracker, root)
	if err := os.Remove(filepath.Join(root, "a.txt")); err != nil {
		t.Fatal(err)
	}
	changes = changesTest(t, tracker, root, token)
	if len(changes.Changes) != 1 || changes.Changes[0].Kind != tools.ChangeDeleted || changes.Changes[0].Deletions != 3 || !strings.Contains(changes.Changes[0].Diff, "+++ /dev/null\n") {
		t.Fatalf("deleted: %+v", changes)
	}
	if repoState(t, root) != before {
		t.Fatal("tracker touched the repository's index, refs or objects")
	}
	if out := gitTest(t, root, "status", "--porcelain"); !strings.Contains(string(out), " D a.txt") || !strings.Contains(string(out), "?? new.txt") {
		t.Fatalf("status changed: %s", out)
	}
	entries, _ := os.ReadDir(tracker.Directory())
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "index-") {
			t.Fatal("temporary index left behind")
		}
	}
}

func TestChangeTrackerNoChangesAndNestedDirectory(t *testing.T) {
	tracker, root := changeFixture(t, ChangeLimits{})
	if err := os.Mkdir(filepath.Join(root, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "sub")
	token := snapshotTest(t, tracker, sub)
	changes := changesTest(t, tracker, sub, token)
	if !changes.Tracked || changes.Changes == nil || len(changes.Changes) != 0 || changes.Root != root {
		t.Fatalf("no changes: %+v", changes)
	}
	writeTest(t, filepath.Join(sub, "x.txt"), "x\n")
	changes = changesTest(t, tracker, sub, token)
	if len(changes.Changes) != 1 || changes.Changes[0].Path != "sub/x.txt" {
		t.Fatalf("nested path: %+v", changes)
	}
	// Both spellings share one repository state.
	if len(tracker.repos) != 1 || tracker.repos[root] == nil {
		t.Fatalf("repository cache: %d entries", len(tracker.repos))
	}
}

func TestChangeTrackerOutsideRepository(t *testing.T) {
	tracker, _ := changeFixture(t, ChangeLimits{})
	plain := t.TempDir()
	for range 2 {
		_, ok, reason, err := tracker.Snapshot(context.Background(), plain)
		if err != nil || ok || reason != "not a git repository" {
			t.Fatalf("outside: ok=%v reason=%q err=%v", ok, reason, err)
		}
	}
	changes, err := tracker.Changes(context.Background(), plain, "token")
	if err != nil || changes.Tracked || changes.Reason != "not a git repository" {
		t.Fatalf("changes outside: %+v %v", changes, err)
	}
	if _, ok, reason, _ := tracker.Snapshot(context.Background(), filepath.Join(plain, "missing")); ok || reason == "" {
		t.Fatal("missing directory tracked")
	}
}

func TestChangeTrackerBinaryLargeAndFileCap(t *testing.T) {
	tracker, root := changeFixture(t, ChangeLimits{})
	token := snapshotTest(t, tracker, root)
	writeTest(t, filepath.Join(root, "blob.bin"), "a\x00b")
	large := make([]byte, tools.ChangeMaxFileBytes+1)
	if err := os.WriteFile(filepath.Join(root, "large.txt"), large, 0600); err != nil {
		t.Fatal(err)
	}
	changes := changesTest(t, tracker, root, token)
	if len(changes.Changes) != 2 {
		t.Fatalf("changes: %+v", changes)
	}
	if b := changes.Changes[0]; b.Path != "blob.bin" || !b.Binary || b.Diff != "" {
		t.Fatalf("binary: %+v", b)
	}
	if l := changes.Changes[1]; l.Path != "large.txt" || !l.Truncated || l.Diff != "" {
		t.Fatalf("large: %+v", l)
	}
	token = snapshotTest(t, tracker, root)
	for i := 0; i < tools.ChangeMaxFiles+5; i++ {
		writeTest(t, filepath.Join(root, "f"+strings.Repeat("0", 3-len(string(rune('0'+i%10))))+string(rune('a'+i%26))+strings.Repeat("x", i/26)+".txt"), "n\n")
	}
	changes = changesTest(t, tracker, root, token)
	if !changes.Truncated || changes.Omitted != 5 || len(changes.Changes) != tools.ChangeMaxFiles {
		t.Fatalf("file cap: truncated=%v omitted=%d n=%d", changes.Truncated, changes.Omitted, len(changes.Changes))
	}
	for i := 1; i < len(changes.Changes); i++ {
		if changes.Changes[i-1].Path >= changes.Changes[i].Path {
			t.Fatal("changes not sorted")
		}
	}
}

func TestChangeTrackerUntrackedSizeGateAndPrivatePaths(t *testing.T) {
	tracker, root := changeFixture(t, ChangeLimits{MaxUntrackedFileBytes: 1024}, "polly.db")
	writeTest(t, filepath.Join(root, "polly.db"), "private state\n")
	token := snapshotTest(t, tracker, root)
	writeTest(t, filepath.Join(root, "polly.db"), "changed private state\n")
	writeTest(t, filepath.Join(root, "b.txt"), "b\n")
	changes := changesTest(t, tracker, root, token)
	if len(changes.Changes) != 1 || changes.Changes[0].Path != "b.txt" {
		t.Fatalf("private path reported: %+v", changes)
	}
	writeTest(t, filepath.Join(root, "big.txt"), strings.Repeat("x", 2048))
	if _, ok, reason, err := tracker.Snapshot(context.Background(), root); err != nil || ok || reason != "untracked files too large to snapshot" {
		t.Fatalf("size gate: ok=%v reason=%q err=%v", ok, reason, err)
	}
	// A tracked file of the same size is fine.
	os.Remove(filepath.Join(root, "big.txt"))
	writeTest(t, filepath.Join(root, "a.txt"), strings.Repeat("y", 2048))
	snapshotTest(t, tracker, root)
}

func TestChangeTrackerTimeoutDisablesRepository(t *testing.T) {
	tracker, root := changeFixture(t, ChangeLimits{SnapshotTimeout: time.Nanosecond})
	for range 2 {
		if _, ok, _, err := tracker.Snapshot(context.Background(), root); ok || err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("timeout: ok=%v err=%v", ok, err)
		}
	}
	_, ok, reason, err := tracker.Snapshot(context.Background(), root)
	if err != nil || ok || reason != "disabled after repeated snapshot timeouts" {
		t.Fatalf("after timeouts: ok=%v reason=%q err=%v", ok, reason, err)
	}
}

func TestChangeTrackerLinkedWorktree(t *testing.T) {
	tracker, root := changeFixture(t, ChangeLimits{})
	linked := filepath.Join(filepath.Dir(root), "linked")
	gitTest(t, root, "worktree", "add", "-q", linked)
	linked, err := filepath.EvalSymlinks(linked)
	if err != nil {
		t.Fatal(err)
	}
	before := repoState(t, root)
	token := snapshotTest(t, tracker, linked)
	writeTest(t, filepath.Join(linked, "a.txt"), "linked\n")
	changes := changesTest(t, tracker, linked, token)
	if changes.Root != linked || len(changes.Changes) != 1 || changes.Changes[0].Deletions != 3 {
		t.Fatalf("linked worktree: %+v", changes)
	}
	if repoState(t, root) != before {
		t.Fatal("linked worktree snapshot touched the main repository")
	}
}

func TestChangeTrackerConcurrentSnapshots(t *testing.T) {
	tracker, root := changeFixture(t, ChangeLimits{})
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token, ok, reason, err := tracker.Snapshot(context.Background(), root)
			if err != nil || !ok {
				errs <- err
				return
			}
			writeTest(t, filepath.Join(root, "c"+string(rune('a'+i))+".txt"), "c\n")
			if _, err := tracker.Changes(context.Background(), root, token); err != nil {
				errs <- err
			}
			_ = reason
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(tracker.Directory())
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "index-") {
			t.Fatal("temporary index left behind")
		}
	}
}

func TestChangeTrackerRequiresSandboxPosture(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX Git fixture")
	}
	registry := tools.NewToolRegistry(nil)
	defer registry.Close()
	if _, err := NewChangeTracker(registry, t.TempDir(), nil, ChangeLimits{}); err == nil {
		t.Fatal("tracker constructed without sandbox or acknowledgement")
	}
}

func TestChangeTrackerSandboxed(t *testing.T) {
	if os.Getenv("POLLYTOOL_REQUIRE_SANDBOX_TESTS") != "1" {
		t.Skip("opt-in process sandbox")
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("sandbox platform")
	}
	_, root := changeFixture(t, ChangeLimits{})
	registry := tools.NewToolRegistry(nil, tools.WithSandboxFactory(sandbox.New, sandbox.DefaultConfig()))
	defer registry.Close()
	tracker, err := NewChangeTracker(registry, filepath.Join(filepath.Dir(root), "sandboxed-changes"), nil, ChangeLimits{})
	if err != nil {
		t.Fatal(err)
	}
	before := repoState(t, root)
	token := snapshotTest(t, tracker, root)
	writeTest(t, filepath.Join(root, "a.txt"), "one\n2\nthree\n")
	changes := changesTest(t, tracker, root, token)
	if len(changes.Changes) != 1 || changes.Changes[0].Additions != 1 {
		t.Fatalf("sandboxed changes: %+v", changes)
	}
	if repoState(t, root) != before {
		t.Fatal("sandboxed tracker touched the repository")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatal(err)
	}
}

func TestChangeTrackerObserversUseIndependentIndexes(t *testing.T) {
	tracker, root := changeFixture(t, ChangeLimits{})
	first := snapshotTest(t, tracker, root)
	if again := snapshotTest(t, tracker, root); again != first {
		t.Fatalf("unchanged tree got a new id: %s vs %s", first, again)
	}
	writeTest(t, filepath.Join(root, "a.txt"), "changed\n")
	edited := snapshotTest(t, tracker, root)
	if edited == first {
		t.Fatal("edit not observed")
	}
	// A new tracker over the same directory has its own index and
	// still sees what changes from here on, including a file restored to
	// its committed content.
	registry := tools.NewToolRegistry(nil, tools.WithUnsafeNoSandbox())
	defer registry.Close()
	reopened, err := NewChangeTracker(registry, tracker.Directory(), nil, ChangeLimits{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	token := snapshotTest(t, reopened, root)
	if token != edited {
		t.Fatalf("reopened tracker disagrees about the tree: %s vs %s", token, edited)
	}
	writeTest(t, filepath.Join(root, "a.txt"), "one\ntwo\nthree\n")
	changes := changesTest(t, reopened, root, token)
	if len(changes.Changes) != 1 || changes.Changes[0].Deletions != 1 || changes.Changes[0].Additions != 3 {
		t.Fatalf("restored file: %+v", changes)
	}
	matches, _ := filepath.Glob(filepath.Join(tracker.Directory(), "*", "index-*"))
	if len(matches) != 2 {
		t.Fatalf("expected an index per observer, found %v", matches)
	}
}
