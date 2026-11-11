package worktree

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

// Git diff's --binary and --no-textconv do not isolate it from live attributes:
// -diff switches an otherwise textual patch to a binary patch.
func TestBuildApplyPlanAndParentApplyExclusion(t *testing.T) {
	m, root := fixture(t)
	checkSourceReservationsReleased(t, m)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	base := testSnapshot(t, m, root)
	c := testCheckout(t, m, base)
	writeTest(t, filepath.Join(c.Path, "a.txt"), "changed\n")
	candidate := testSnapshot(t, m, c.Path)
	attrsCheckout := testCheckout(t, m, base)
	writeTest(t, filepath.Join(attrsCheckout.Path, ".gitattributes"), "a.txt -diff\n")
	attrs := testSnapshot(t, m, attrsCheckout.Path)
	attrsPlan, err := m.BuildApplyPlan(ctx, "attribute-change", base, attrs, "tree")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.PreflightApply(ctx, attrsPlan); err != nil {
		t.Fatal(err)
	}
	plain, err := m.BuildApplyPlan(ctx, "content-change", base, candidate, "paths")
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	m.sandbox = commandBarrier(func(cmd *exec.Cmd) error {
		if slices.Contains(cmd.Args, "diff") && slices.Contains(cmd.Args, candidate.Commit) {
			close(entered)
			<-release
		}
		return nil
	})
	var got ApplyPlan
	planned := make(chan error, 1)
	go func() {
		var err error
		got, err = m.BuildApplyPlan(ctx, "content-change", base, candidate, "paths")
		planned <- err
	}()
	awaitWorktree(t, entered)
	applied := make(chan error, 1)
	go func() { applied <- m.WriteApply(ctx, attrsPlan) }()
	applyFinished := parentApplyFinishedOrQueued(t, m, applied)
	once.Do(func() { close(release) })
	if err := awaitWorktreeResult(t, planned); err != nil {
		t.Fatal(err)
	}
	if !applyFinished {
		if err := awaitWorktreeResult(t, applied); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("concurrent parent apply finished before diff=%t; before hash=%s, after hash=%s", applyFinished, plain.PatchHash, got.PatchHash)
	if got.PatchHash != plain.PatchHash {
		t.Fatal("concurrent WriteApply changed live parent attributes during patch generation")
	}
	// Changes after planning still fail closed at the existing identity check.
	// The paths drift mode permits an unrelated .gitattributes file, so it is
	// the patch check, not whole-tree drift detection, that must reject this.
	m.sandbox = commandBarrier(func(*exec.Cmd) error { return nil })
	if _, err := m.PreflightApply(ctx, got); err == nil || err.Error() != "integration patch identity changed" {
		t.Fatalf("accepted changed patch identity after planning: %v", err)
	}
}

func TestParentSourceReservationCancellationAndErrors(t *testing.T) {
	for _, operation := range []string{"Merge", "Preview", "BuildApplyPlan"} {
		t.Run(operation, func(t *testing.T) {
			outcomes := []string{"canceled", "git-error"}
			if operation == "Preview" {
				outcomes = append(outcomes, "capture-error")
			}
			for _, outcome := range outcomes {
				t.Run(outcome, func(t *testing.T) {
					m, root := fixture(t)
					checkSourceReservationsReleased(t, m)
					base := testSnapshot(t, m, root)
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					want := context.Canceled
					if outcome == "canceled" {
						unlock, err := m.reserveSource(ctx, root)
						if err != nil {
							t.Fatal(err)
						}
						defer unlock()
					} else {
						want = errors.New("injected Git error")
						command := "merge-tree"
						if operation == "BuildApplyPlan" {
							command = "diff"
						} else if outcome == "capture-error" {
							command = "rev-parse"
						}
						m.sandbox = commandBarrier(func(cmd *exec.Cmd) error {
							if slices.Contains(cmd.Args, command) {
								return want
							}
							return nil
						})
					}
					done := make(chan error, 1)
					go func() {
						var err error
						switch operation {
						case "Merge":
							_, err = m.Merge(ctx, base, base, base)
						case "Preview":
							_, err = m.Preview(ctx, base, base)
						case "BuildApplyPlan":
							_, err = m.BuildApplyPlan(ctx, "canceled", base, base, "tree")
						}
						done <- err
					}()
					if outcome == "canceled" {
						sourceWaiters(t, m, root, 2)
						cancel()
					}
					if err := awaitWorktreeResult(t, done); !errors.Is(err, want) {
						t.Fatalf("operation error: got %v, want %v", err, want)
					}
				})
			}
		})
	}
}

func TestPreviewReleasesParentBeforeAdministration(t *testing.T) {
	m, root := fixture(t)
	checkSourceReservationsReleased(t, m)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	base := testSnapshot(t, m, root)
	m.admin.Lock()
	locked := true
	defer func() {
		if locked {
			m.admin.Unlock()
		}
	}()
	done := make(chan error, 1)
	go func() { _, err := m.Preview(ctx, base, base); done <- err }()
	// Preview reaches slot creation only after the live-attribute command.
	sourceWaiters(t, m, filepath.Join(m.Slots[0], "tree"), 1)
	if _, err := m.Capture(ctx, root); err != nil {
		t.Fatal("preview held the parent while waiting for administration:", err)
	}
	m.admin.Unlock()
	locked = false
	if err := awaitWorktreeResult(t, done); err != nil {
		t.Fatal(err)
	}
}

// Wait for either the unsafe write to finish or its source reservation to queue
// behind the held Git command. This also reproduces the old bug without a sleep.
func parentApplyFinishedOrQueued(t *testing.T, m *Manager, applied <-chan error) bool {
	t.Helper()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case err := <-applied:
			if err != nil {
				t.Fatal(err)
			}
			return true
		case <-tick.C:
			m.sourcesMu.Lock()
			r := m.sources[m.Root]
			queued := r != nil && r.users >= 2
			m.sourcesMu.Unlock()
			if queued {
				return false
			}
		case <-timeout.C:
			t.Fatal("neither parent apply completion nor a source reservation")
		}
	}
}
