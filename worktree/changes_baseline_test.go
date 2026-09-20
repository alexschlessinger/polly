package worktree

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/tools"
)

func TestChangeBaselineNetAndUntracked(t *testing.T) {
	tracker, root := changeFixture(t, ChangeLimits{})
	writeTest(t, filepath.Join(root, "a.txt"), "dirty before session\n")
	writeTest(t, filepath.Join(root, "preexisting.txt"), "untracked before session\n")
	writeTest(t, filepath.Join(root, "ignored.txt"), "ignored\n")
	original := repoState(t, root)
	base, reason, err := tracker.CaptureBaseline(context.Background(), root)
	if err != nil || reason != "" {
		t.Fatalf("baseline: %s %v", reason, err)
	}
	if base.Root != root || len(base.Pack) == 0 {
		t.Fatalf("baseline: %+v", base)
	}
	changes := changesTest(t, tracker, root, base.Tree)
	if len(changes.Changes) != 1 || changes.Changes[0].Path != "preexisting.txt" || changes.Changes[0].Kind != tools.ChangeCreated {
		t.Fatalf("initial untracked: %+v", changes)
	}
	writeTest(t, filepath.Join(root, "a.txt"), "intermediate\n")
	changesTest(t, tracker, root, base.Tree)
	writeTest(t, filepath.Join(root, "a.txt"), "final\n")
	changes = changesTest(t, tracker, root, base.Tree)
	if len(changes.Changes) != 2 || !strings.Contains(changes.Changes[0].Diff, "-dirty before session\n+final\n") || strings.Contains(changes.Changes[0].Diff, "intermediate") {
		t.Fatalf("net: %+v", changes)
	}
	writeTest(t, filepath.Join(root, "a.txt"), "dirty before session\n")
	if err = os.Remove(filepath.Join(root, "preexisting.txt")); err != nil {
		t.Fatal(err)
	}
	changes = changesTest(t, tracker, root, base.Tree)
	if len(changes.Changes) != 0 {
		t.Fatalf("reverted: %+v", changes)
	}
	if repoState(t, root) != original {
		t.Fatal("baseline changed real index, refs or objects")
	}
}

func TestChangeBaselineSurvivesCacheRemoval(t *testing.T) {
	tracker, root := changeFixture(t, ChangeLimits{})
	base, reason, err := tracker.CaptureBaseline(context.Background(), root)
	if err != nil || reason != "" {
		t.Fatalf("baseline: %s %v", reason, err)
	}
	writeTest(t, filepath.Join(root, "a.txt"), "after\n")
	// Rewriting and collecting the repository cannot remove an exported tree's
	// borrowed objects. Import into a completely fresh runtime cache.
	gitTest(t, root, "checkout", "--orphan", "replacement")
	gitTest(t, root, "add", ".")
	gitTest(t, root, "commit", "-qm", "replacement")
	gitTest(t, root, "update-ref", "-d", "refs/heads/main")
	gitTest(t, root, "update-ref", "-d", "refs/heads/master")
	gitTest(t, root, "reflog", "expire", "--expire=now", "--all")
	gitTest(t, root, "gc", "--prune=now")
	// The exported pack is independently readable even after the old cache goes.
	if err = os.RemoveAll(tracker.Directory()); err != nil {
		t.Fatal(err)
	}
	restored, err := NewChangeTracker(tracker.registry, tracker.Directory(), nil, ChangeLimits{})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if err = restored.RestoreBaseline(context.Background(), base); err != nil {
		t.Fatal(err)
	}
	changes := changesTest(t, restored, root, base.Tree)
	if len(changes.Changes) != 1 || !strings.Contains(changes.Changes[0].Diff, "-one\n") {
		t.Fatalf("restored net: %+v", changes)
	}
}

func TestChangeTrackerOtherObserverCannotHideEdit(t *testing.T) {
	tracker, root := changeFixture(t, ChangeLimits{})
	before := snapshotTest(t, tracker, root)
	other, err := NewChangeTracker(tracker.registry, tracker.Directory(), nil, ChangeLimits{})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	writeTest(t, filepath.Join(root, "a.txt"), "changed\n")
	snapshotTest(t, other, root)
	changes := changesTest(t, tracker, root, before)
	if len(changes.Changes) != 1 {
		t.Fatalf("edit hidden by other observer: %+v", changes)
	}
}

func TestChangeTrackerLargeCountsAreExplicitlyUnknown(t *testing.T) {
	tracker, root := changeFixture(t, ChangeLimits{})
	old := strings.Repeat("x\n", tools.ChangeMaxFileBytes/2+1)
	writeTest(t, filepath.Join(root, "a.txt"), old)
	before := snapshotTest(t, tracker, root)
	writeTest(t, filepath.Join(root, "a.txt"), "y\n"+old[2:])
	changes := changesTest(t, tracker, root, before)
	if len(changes.Changes) != 1 || !changes.Changes[0].CountsUnknown || !changes.Changes[0].Truncated {
		t.Fatalf("unknown counts: %+v", changes)
	}
}

func TestChangeTrackerPruneProtectsActiveObservers(t *testing.T) {
	tracker, root := changeFixture(t, ChangeLimits{})
	before := snapshotTest(t, tracker, root)
	entries, err := os.ReadDir(tracker.Directory())
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * changeObjectsRetention)
	for _, entry := range entries {
		if entry.IsDir() {
			if err = os.Chtimes(filepath.Join(tracker.Directory(), entry.Name()), old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	other, err := NewChangeTracker(tracker.registry, tracker.Directory(), nil, ChangeLimits{})
	if err != nil {
		t.Fatal(err)
	}
	otherBefore := snapshotTest(t, other, root)
	writeTest(t, filepath.Join(root, "a.txt"), "after\n")
	if got := changesTest(t, tracker, root, before); len(got.Changes) != 1 {
		t.Fatalf("active cache pruned: %+v", got)
	}
	if err = tracker.Close(); err != nil {
		t.Fatal(err)
	}
	if got := changesTest(t, other, root, otherBefore); len(got.Changes) != 1 {
		t.Fatalf("other observer's store removed: %+v", got)
	}
	if err = other.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err = os.ReadDir(other.Directory())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			t.Fatalf("unowned snapshot cache retained: %s", entry.Name())
		}
	}
}

func TestWorkspaceChangesUntrackedAndIgnoreTransitions(t *testing.T) {
	tracker, root := changeFixture(t, ChangeLimits{})
	writeTest(t, filepath.Join(root, "new.txt"), "new\n")
	base, reason, err := tracker.CaptureBaseline(context.Background(), root)
	if err != nil || reason != "" {
		t.Fatalf("baseline: %s %v", reason, err)
	}
	gitTest(t, root, "rm", "--cached", "a.txt")
	report, err := tracker.WorkspaceChanges(context.Background(), root, base.Tree)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Changes) != 2 || report.Changes[0].Kind != tools.ChangeCreated || report.Changes[0].Path != "a.txt" {
		t.Fatalf("newly untracked: %+v", report)
	}
	writeTest(t, filepath.Join(root, ".gitignore"), "ignored.txt\nnew.txt\n")
	report, err = tracker.WorkspaceChanges(context.Background(), root, base.Tree)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range report.Changes {
		if c.Path == "new.txt" {
			t.Fatal("shadow index kept a now-ignored untracked file")
		}
	}
}
