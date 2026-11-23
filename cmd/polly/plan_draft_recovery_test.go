package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/sessions"
)

func assertDraftFile(t *testing.T, path, want string) {
	t.Helper()
	if got, err := os.ReadFile(path); err != nil || string(got) != want {
		t.Fatalf("%s: got %q, %v; want %q", path, got, err, want)
	}
}

func interruptedDraft(t *testing.T, path string) (root *os.Root, name, stage, pending string) {
	t.Helper()
	root, name, err := openPlanDraftDir(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	stage, pending = stagedPlanDraft(name), pendingDraftOriginal(name)
	if err := root.WriteFile(stage, []byte("unpublished proposal"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := moveDraftEntry(root, name, pending); err != nil {
		t.Fatal(err)
	}
	return root, name, stage, pending
}

func TestPlanDraftRestoresPendingOriginalAfterInterruption(t *testing.T) {
	path := testDraft(t)
	const original = "the last published draft"
	rev, err := writePlanDraft(path, sessions.PlanRevision(""), original)
	if err != nil {
		t.Fatal(err)
	}
	// Model a process dying after displacement but before installation; an
	// older retained version must not be mistaken for the pending original.
	root, _, stage, pending := interruptedDraft(t, path)
	info, err := root.Lstat(pending)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canonical draft should be missing at this boundary: %v", err)
	}
	if text, recoveredRev, err := readPlanDraft(path); err != nil || text != original || recoveredRev != rev {
		t.Fatalf("recover: %q, %s, %v", text, recoveredRev, err)
	}
	if restored, err := os.Lstat(path); err != nil || !os.SameFile(info, restored) {
		t.Fatalf("restoration didn't move the original inode back: %v", err)
	}
	stagePath := filepath.Join(root.Name(), stage)
	assertDraftFile(t, stagePath, "unpublished proposal")
	if _, err := writePlanDraft(path, rev, "another proposal"); err == nil || !strings.Contains(err.Error(), strconv.Quote(stagePath)) {
		t.Fatalf("abandoned stage should require explicit cleanup: %v", err)
	}
	if err := os.Rename(stagePath, filepath.Join(t.TempDir(), "staged.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := writePlanDraft(path, rev, "after recovery"); err != nil {
		t.Fatal(err)
	}
}

func TestPlanDraftRecoveryDoesNotOverwriteConcurrentSave(t *testing.T) {
	path := testDraft(t)
	root, name, stage, pending := interruptedDraft(t, path)
	const saved = "editor wrote a new canonical file"
	if err := os.WriteFile(path, []byte(saved), 0600); err != nil {
		t.Fatal(err)
	}
	pendingPath := filepath.Join(root.Name(), pending)
	if err := installDraftReplacement(root, name, stage, sessions.PlanRevision("")); err == nil || !strings.Contains(err.Error(), strconv.Quote(pendingPath)) {
		t.Fatalf("installation must reject the save and name the displaced draft: %v", err)
	}
	if _, _, err := readPlanDraft(path); err == nil || !strings.Contains(err.Error(), strconv.Quote(pendingPath)) {
		t.Fatalf("interruption with both versions needs reconciliation: %v", err)
	}
	assertDraftFile(t, path, saved)
	assertDraftFile(t, pendingPath, "")
	assertDraftFile(t, filepath.Join(root.Name(), stage), "unpublished proposal")
}

func TestPlanDraftRollsBackWhenDisplacedVersionChanged(t *testing.T) {
	path := testDraft(t)
	const saved = "editor saved right before the old name was claimed"
	if err := os.WriteFile(path, []byte(saved), 0600); err != nil {
		t.Fatal(err)
	}
	root, name, stage, pending := interruptedDraft(t, path)
	if err := installDraftReplacement(root, name, stage, sessions.PlanRevision("")); err == nil || !strings.Contains(err.Error(), "changed during update") {
		t.Fatalf("stale revision should conflict: %v", err)
	}
	assertDraftFile(t, path, saved)
	if _, err := root.Lstat(pending); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rollback should restore the original without a second link: %v", err)
	}
}

func TestPlanDraftRetainsLateEditorWrites(t *testing.T) {
	path := testDraft(t)
	root, name, err := openPlanDraftDir(path)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	// Root opens with FILE_SHARE_DELETE on Windows, modeling an editor which
	// permits renames while it holds the original file open.
	editor, err := root.OpenFile(name, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer editor.Close()
	rev, err := writePlanDraft(path, sessions.PlanRevision(""), "first revision")
	if err != nil {
		t.Fatal(err)
	}
	backups, err := draftBackups(root, name)
	if err != nil || len(backups) != 1 {
		t.Fatalf("retained originals: %v, %v", backups, err)
	}
	backup := filepath.Join(root.Name(), backups[0].name)
	// The old handle can write not just after the final compare, but even
	// after an additional revision has already been published.
	rev, err = writePlanDraft(path, rev, "second revision")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := editor.WriteString("editor's late changes"); err != nil {
		t.Fatal(err)
	}
	assertDraftFile(t, backup, "editor's late changes")
	assertDraftFile(t, path, "second revision")
	if _, _, err := readPlanDraft(path); err == nil || !strings.Contains(err.Error(), strconv.Quote(backup)) || !strings.Contains(err.Error(), "retained original changed") {
		t.Fatalf("read should identify the late save for reconciliation: %v", err)
	}
	if _, err := writePlanDraft(path, rev, "unmerged revision"); err == nil || !strings.Contains(err.Error(), strconv.Quote(backup)) {
		t.Fatalf("write should refuse to advance past the late save: %v", err)
	}
	assertDraftFile(t, backup, "editor's late changes")
	assertDraftFile(t, path, "second revision")
}

func TestPlanDraftRecoversLegacyPrevious(t *testing.T) {
	for _, ambiguous := range []bool{false, true} {
		t.Run(fmt.Sprintf("ambiguous=%t", ambiguous), func(t *testing.T) {
			path := testDraft(t)
			const original = "legacy editor changes"
			if err := os.WriteFile(path, []byte(original), 0600); err != nil {
				t.Fatal(err)
			}
			old := path + ".previous-" + strings.Repeat("a", 32)
			if err := os.Rename(path, old); err != nil {
				t.Fatal(err)
			}
			if !ambiguous {
				if text, rev, err := readPlanDraft(path); err != nil || text != original || rev != sessions.PlanRevision(original) {
					t.Fatalf("legacy recovery: %q, %s, %v", text, rev, err)
				}
				assertDraftFile(t, path, original)
				return
			}
			other := path + ".previous-" + strings.Repeat("b", 32)
			if err := os.WriteFile(other, []byte("another candidate"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := readPlanDraft(path); err == nil || !strings.Contains(err.Error(), strconv.Quote(old)) || !strings.Contains(err.Error(), strconv.Quote(other)) {
				t.Fatalf("ambiguous history should name both candidates: %v", err)
			}
			if _, err := writePlanDraft(path, missingPlanDraftRevision, "must not mask recovery"); err == nil {
				t.Fatal("created over ambiguous history")
			}
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("chose a version without confirmation: %v", err)
			}
			assertDraftFile(t, old, original)
			assertDraftFile(t, other, "another candidate")
		})
	}
}

func TestPlanDraftRejectsLinkedRecoveryEntry(t *testing.T) {
	for _, link := range []struct {
		name   string
		create func(string, string) error
	}{{"symlink", os.Symlink}, {"hardlink", os.Link}} {
		t.Run(link.name, func(t *testing.T) {
			path := testDraft(t)
			other := filepath.Join(t.TempDir(), "not-a-draft.txt")
			if err := os.WriteFile(other, []byte("unrelated file"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			old := path + ".previous-" + strings.Repeat("a", 32)
			if err := link.create(other, old); err != nil {
				t.Skip(err)
			}
			if _, _, err := readPlanDraft(path); err == nil || !strings.Contains(err.Error(), strconv.Quote(old)) {
				t.Fatalf("must refuse linked backup without losing its location: %v", err)
			}
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("restored a linked entry: %v", err)
			}
			if _, err := os.Lstat(old); err != nil {
				t.Fatalf("recovery entry was modified: %v", err)
			}
			assertDraftFile(t, other, "unrelated file")
		})
	}
}

func TestPlanDraftBackupLimitRequiresExplicitRetirement(t *testing.T) {
	path := testDraft(t)
	rev, current := sessions.PlanRevision(""), ""
	for i := 1; i <= maxPlanDraftBackups; i++ {
		current = fmt.Sprintf("revision %d", i)
		var err error
		if rev, err = writePlanDraft(path, rev, current); err != nil {
			t.Fatal(err)
		}
	}
	if unchanged, err := writePlanDraft(path, rev, current); err != nil || unchanged != rev {
		t.Fatalf("unchanged revision must not spend a backup slot: %s, %v", unchanged, err)
	}
	if _, err := writePlanDraft(path, rev, "extra revision"); err == nil || !strings.Contains(err.Error(), "backup limit") {
		t.Fatalf("expected explicit limit: %v", err)
	}
	assertDraftFile(t, path, current)
	if info, err := os.Stat(path); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0600) {
		t.Fatalf("published draft does not have private permissions: %v, %v", info, err)
	}
	root, name, err := openPlanDraftDir(path)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	backups, err := draftBackups(root, name)
	if err != nil || len(backups) != maxPlanDraftBackups {
		t.Fatalf("backup count: %d, %v", len(backups), err)
	}
	if err := os.Rename(filepath.Join(root.Name(), backups[0].name), filepath.Join(t.TempDir(), "reviewed.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := writePlanDraft(path, rev, "after explicit retirement"); err != nil {
		t.Fatal(err)
	}
}
