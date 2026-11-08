package worktree

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// A waiting operation must use the same canonical source it reserved (or
// reacquire/revalidate). A retarget must not redirect it into another owner.
func TestReviewRetargetedSourceReservation(t *testing.T) {
	for _, retain := range []bool{false, true} {
		t.Run(map[bool]string{false: "Capture", true: "RetainCommit"}[retain], func(t *testing.T) {
			for _, outcome := range []string{"retargeted", "stable", "removed", "replaced-source", "canceled", "already-canceled", "git-error"} {
				t.Run(outcome, func(t *testing.T) {
					m, root := fixture(t)
					checkSourceReservationsReleased(t, m)
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					base := testSnapshot(t, m, root)
					a, b := testCheckout(t, m, base), testCheckout(t, m, base)
					writeTest(t, filepath.Join(a.Path, "a.txt"), "source A\n")
					want := base
					if !retain {
						want = testSnapshot(t, m, a.Path)
					}
					alias := filepath.Join(filepath.Dir(a.Path), "alias")
					if err := os.Symlink(a.Path, alias); err != nil {
						t.Fatal(err)
					}
					unlockA, err := m.reserveSource(ctx, a.Path)
					if err != nil {
						t.Fatal(err)
					}
					defer func() {
						if unlockA != nil {
							unlockA()
						}
					}()
					unlockB, err := m.reserveSource(ctx, b.Path)
					if err != nil {
						t.Fatal(err)
					}
					defer unlockB()
					injected := errors.New("injected Git failure")
					if outcome == "git-error" {
						m.sandbox = commandBarrier(func(*exec.Cmd) error { return injected })
					}
					if outcome == "already-canceled" {
						cancel()
					}
					var got Snapshot
					done := make(chan error, 1)
					go func() {
						var err error
						if retain {
							got, err = m.RetainCommit(ctx, alias, base.Commit)
						} else {
							got, err = m.Capture(ctx, alias)
						}
						done <- err
					}()
					if outcome != "already-canceled" {
						sourceWaiters(t, m, a.Path, 2)
					}
					switch outcome {
					case "retargeted", "removed":
						if err := os.Remove(alias); err != nil {
							t.Fatal(err)
						}
						if outcome == "retargeted" {
							if err := os.Symlink(b.Path, alias); err != nil {
								t.Fatal(err)
							}
						}
					case "replaced-source":
						if err := os.Rename(a.Path, a.Path+"-saved"); err != nil {
							t.Fatal(err)
						}
						if err := os.Symlink(b.Path, a.Path); err != nil {
							t.Fatal(err)
						}
					case "canceled":
						cancel()
					}
					if outcome == "canceled" || outcome == "already-canceled" {
						if err := awaitWorktreeResult(t, done); !errors.Is(err, context.Canceled) {
							t.Fatalf("canceled reservation: %v", err)
						}
						return // cancellation must finish with A and B still held
					}
					unlockA()
					unlockA = nil
					err = awaitWorktreeResult(t, done)
					if outcome == "replaced-source" {
						if err == nil || !strings.Contains(err.Error(), "source changed after reservation") {
							t.Fatalf("accepted a replaced canonical source: %+v %v", got, err)
						}
						return
					}
					if outcome == "git-error" {
						if !errors.Is(err, injected) {
							t.Fatalf("lost Git error: %v", err)
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					if got.Source != a.Path || got.Tree != want.Tree || (retain && got.Commit != base.Commit) {
						t.Fatalf("operation left reserved source A while B was exclusively held: %+v, want tree %s from %s", got, want.Tree, a.Path)
					}
				})
			}
		})
	}
}

// A fixture may live below a symlink (for example /var on macOS), while
// reservations are keyed by canonical paths. Exercise both an existing source
// and a not-yet-created checkout so the barrier never depends on TMPDIR spelling.
func TestSourceWaitersCanonicalIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX symlink fixture")
	}
	for _, present := range []bool{true, false} {
		t.Run(map[bool]string{true: "present", false: "missing-tree"}[present], func(t *testing.T) {
			m := &Manager{}
			checkSourceReservationsReleased(t, m)
			dir := t.TempDir()
			real := filepath.Join(dir, "real")
			if err := os.Mkdir(real, 0700); err != nil {
				t.Fatal(err)
			}
			alias := filepath.Join(dir, "alias")
			if err := os.Symlink(real, alias); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(alias, "tree")
			if present {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			unlock, err := m.reserveSource(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() {
				unlock, err := m.reserveSource(ctx, filepath.Join(real, "tree"))
				if err == nil {
					unlock()
				}
				done <- err
			}()
			defer func() {
				cancel()
				if err := awaitWorktreeResult(t, done); !errors.Is(err, context.Canceled) {
					t.Errorf("waiter cancellation: got %v, want %v", err, context.Canceled)
				}
			}()
			sourceWaiters(t, m, path, 2)
			canonical, err := filepath.EvalSymlinks(real)
			if err != nil {
				t.Fatal(err)
			}
			m.sourcesMu.Lock()
			r := m.sources[filepath.Join(canonical, "tree")]
			blocked := r != nil && r.users == 2 && len(r.ready) == 0
			m.sourcesMu.Unlock()
			if !blocked {
				t.Fatal("barrier returned without a real waiter on the held canonical source")
			}
		})
	}
}

func checkSourceReservationsReleased(t *testing.T, m *Manager) {
	t.Helper()
	t.Cleanup(func() {
		m.sourcesMu.Lock()
		remaining := len(m.sources)
		m.sourcesMu.Unlock()
		if remaining != 0 {
			t.Errorf("retained %d idle source reservations", remaining)
		}
	})
}

// merge-tree reads live .gitattributes. Concurrent parent writes must not change
// a started merge's semantics, unless attribute resolution is made immutable.
func TestReviewMergeAndParentApplyExclusion(t *testing.T) {
	for _, preview := range []bool{false, true} {
		t.Run(map[bool]string{false: "Merge", true: "Preview"}[preview], func(t *testing.T) {
			m, root := fixture(t)
			checkSourceReservationsReleased(t, m)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			base := testSnapshot(t, m, root)
			b := testCheckout(t, m, base)
			writeTest(t, filepath.Join(root, "a.txt"), "ours\n")
			writeTest(t, filepath.Join(b.Path, "a.txt"), "theirs\n")
			ours, theirs := testSnapshot(t, m, root), testSnapshot(t, m, b.Path)
			attrsCheckout := testCheckout(t, m, ours)
			writeTest(t, filepath.Join(attrsCheckout.Path, ".gitattributes"), "a.txt merge=union\n")
			attrs := testSnapshot(t, m, attrsCheckout.Path)
			plan, err := m.BuildApplyPlan(ctx, "attribute-change", ours, attrs, "tree")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := m.PreflightApply(ctx, plan); err != nil {
				t.Fatal(err)
			}
			plain, err := m.Merge(ctx, base, ours, theirs)
			if err != nil || len(plain.Conflicts) == 0 {
				t.Fatalf("expected conflicting inputs: %+v %v", plain, err)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(release) })
			m.sandbox = commandBarrier(func(cmd *exec.Cmd) error {
				if slices.Contains(cmd.Args, "merge-tree") {
					close(entered)
					<-release
				}
				return nil
			})
			type mergeResult struct {
				tree       string
				conflicted bool
				err        error
			}
			merged := make(chan mergeResult, 1)
			go func() {
				if preview {
					got, err := m.Preview(ctx, base, theirs)
					merged <- mergeResult{got.Merged.Tree, got.Conflicts != "", err}
				} else {
					got, err := m.Merge(ctx, base, ours, theirs)
					merged <- mergeResult{got.Snapshot.Tree, len(got.Conflicts) != 0, err}
				}
			}()
			awaitWorktree(t, entered)
			// Parent-dependent Git work must not fence independent captures or
			// worktree administration, even while the merge itself is held.
			if _, err := m.Capture(ctx, b.Path); err != nil {
				t.Fatal(err)
			}
			if _, err := m.Create(ctx, theirs); err != nil {
				t.Fatal(err)
			}
			applied := make(chan error, 1)
			go func() { applied <- m.WriteApply(ctx, plan) }()
			applyFinished := parentApplyFinishedOrQueued(t, m, applied)
			once.Do(func() { close(release) })
			got := <-merged
			if !applyFinished {
				if err := awaitWorktreeResult(t, applied); err != nil {
					t.Fatal(err)
				}
			}
			if got.err != nil {
				t.Fatal(got.err)
			}
			t.Logf("concurrent parent apply finished before merge=%t; before tree=%s, after tree=%s, conflicted=%t", applyFinished, plain.Snapshot.Tree, got.tree, got.conflicted)
			if !got.conflicted {
				t.Fatal("concurrent WriteApply changed live parent .gitattributes and silently resolved an otherwise conflicting merge")
			}
		})
	}
}
