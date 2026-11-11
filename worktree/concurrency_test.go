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
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

// commandBarrier observes the existing managed-execution seam; it never replaces
// Git results. The fixture explicitly acknowledges unsafe execution; native
// sandbox behavior is exercised separately by the package's sandbox tests.
type commandBarrier func(*exec.Cmd) error

func (f commandBarrier) Wrap(cmd *exec.Cmd) error { return f(cmd) }

func awaitWorktree(t *testing.T, ready <-chan struct{}) {
	t.Helper()
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("worktree barrier timed out")
	}
}
func awaitWorktreeResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("worktree operation timed out")
		return nil
	}
}

// holdGit parks the first Git command that matches until release is called;
// entered is closed once it is parked. Later matches run straight through.
func holdGit(m *Manager, match func(*exec.Cmd) bool) (entered <-chan struct{}, release func()) {
	parked, resume := make(chan struct{}), make(chan struct{})
	var held atomic.Bool
	m.sandbox = commandBarrier(func(cmd *exec.Cmd) error {
		if match(cmd) && held.CompareAndSwap(false, true) {
			close(parked)
			<-resume
		}
		return nil
	})
	return parked, sync.OnceFunc(func() { close(resume) })
}

// sourceUsers counts the operations holding or awaiting path's reservation.
// The fixture's input spelling can contain symlinked ancestors, but the
// reservation map uses canonical identities, resolved as the operation did.
func sourceUsers(t *testing.T, m *Manager, path string) int {
	t.Helper()
	key, err := sourceKey(path)
	if err != nil {
		t.Fatal(err)
	}
	m.sourcesMu.Lock()
	defer m.sourcesMu.Unlock()
	if r := m.sources[key]; r != nil {
		return r.users
	}
	return 0
}
func sourceWaiters(t *testing.T, m *Manager, path string, n int) {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for sourceUsers(t, m, path) < n {
		select {
		case <-deadline.C:
			t.Fatalf("source reservation for %q did not reach %d users", path, n)
		case <-tick.C:
		}
	}
}
func testCheckout(t *testing.T, m *Manager, base Snapshot) Checkout {
	t.Helper()
	c, err := m.Create(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func testSnapshot(t *testing.T, m *Manager, source string) Snapshot {
	t.Helper()
	s, err := m.Capture(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestIndependentCapturesOverlapAndSameSourceCancels(t *testing.T) {
	m, root := fixture(t)
	checkSourceReservationsReleased(t, m)
	ctx := context.Background()
	base := testSnapshot(t, m, root)
	a, b := testCheckout(t, m, base), testCheckout(t, m, base)
	writeTest(t, filepath.Join(a.Path, "a.txt"), "first\n")
	writeTest(t, filepath.Join(b.Path, "a.txt"), "second\n")
	entered, release := holdGit(m, func(cmd *exec.Cmd) bool { return cmd.Dir == a.Path })
	defer release()
	first := make(chan error, 1)
	go func() { _, err := m.Capture(ctx, a.Path); first <- err }()
	awaitWorktree(t, entered)
	// A full second capture, including its real object/ref writes, must finish
	// while the first capture is held before its first Git read.
	second := make(chan error, 1)
	go func() {
		s, err := m.Capture(ctx, b.Path)
		if err == nil && s.Tree == base.Tree {
			err = errors.New("lost changed tree")
		}
		second <- err
	}()
	if err := awaitWorktreeResult(t, second); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(filepath.Dir(a.Path), "alias")
	if err := os.Symlink(a.Path, alias); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	defer cancel()
	same := make(chan error, 1)
	go func() { _, err := m.Capture(canceled, alias); same <- err }()
	sourceWaiters(t, m, a.Path, 2)
	cancel()
	if err := awaitWorktreeResult(t, same); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled reservation: %v", err)
	}
	release()
	if err := awaitWorktreeResult(t, first); err != nil {
		t.Fatal(err)
	}
}

func TestSourceReservationSerializesParentApplyAndCheckoutCleanup(t *testing.T) {
	m, root := fixture(t)
	ctx := context.Background()
	base := testSnapshot(t, m, root)
	c := testCheckout(t, m, base)
	plan, err := m.BuildApplyPlan(ctx, "same-source", base, base, "tree")
	if err != nil {
		t.Fatal(err)
	}
	// All parent observers and writers use the same reservation, while an
	// independent checkout can still complete useful Git work.
	unlock, err := m.reserveSource(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if unlock != nil {
			unlock()
		}
	}()
	results := make(chan error, 3)
	go func() { _, err := m.PreflightApply(ctx, plan); results <- err }()
	go func() { results <- m.WriteApply(ctx, plan) }()
	go func() { _, err := m.ReconcileApply(ctx, plan); results <- err }()
	sourceWaiters(t, m, root, 4)
	if _, err := m.Capture(ctx, c.Path); err != nil {
		t.Fatal(err)
	}
	unlock()
	unlock = nil
	for range 3 {
		if err := awaitWorktreeResult(t, results); err != nil {
			t.Fatal(err)
		}
	}
	unlock, err = m.reserveSource(ctx, c.Path)
	if err != nil {
		t.Fatal(err)
	}
	go func() { results <- m.Cleanup(ctx, c, "") }()
	sourceWaiters(t, m, c.Path, 2)
	if _, err := os.Stat(c.Path); err != nil {
		t.Fatal("cleanup removed a reserved checkout", err)
	}
	unlock()
	unlock = nil
	if err := awaitWorktreeResult(t, results); err != nil {
		t.Fatal(err)
	}
}

func TestAdministrationWaitRechecksCleanupTreeAndOwnership(t *testing.T) {
	for _, changeOwner := range []bool{false, true} {
		t.Run(map[bool]string{false: "tree", true: "owner"}[changeOwner], func(t *testing.T) {
			m, root := fixture(t)
			ctx := context.Background()
			base := testSnapshot(t, m, root)
			c, other := testCheckout(t, m, base), testCheckout(t, m, base)
			m.admin.Lock()
			unlockAdmin := sync.OnceFunc(m.admin.Unlock)
			defer unlockAdmin()
			cleanup := make(chan error, 1)
			go func() { cleanup <- m.Cleanup(ctx, c, "") }()
			sourceWaiters(t, m, c.Path, 1)
			created := make(chan error, 1)
			go func() { _, err := m.Create(ctx, base); created <- err }()
			sourceWaiters(t, m, filepath.Join(m.Slots[2], "tree"), 1)
			// Captures do not queue behind unrelated shared worktree administration.
			if _, err := m.Capture(ctx, other.Path); err != nil {
				t.Fatal(err)
			}
			if changeOwner {
				writeTest(t, filepath.Join(filepath.Dir(c.Path), "owner"), "new-owner")
			} else {
				writeTest(t, filepath.Join(c.Path, "late.txt"), "retain me\n")
			}
			unlockAdmin()
			if err := awaitWorktreeResult(t, cleanup); err == nil {
				t.Fatal("cleanup trusted stale eligibility")
			}
			if err := awaitWorktreeResult(t, created); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(c.Path); err != nil {
				t.Fatal("cleanup removed changed checkout", err)
			}
		})
	}
}

func TestCleanupRechecksOwnerAfterContentProof(t *testing.T) {
	m, root := fixture(t)
	ctx := context.Background()
	base := testSnapshot(t, m, root)
	c := testCheckout(t, m, base)
	m.sandbox = commandBarrier(func(cmd *exec.Cmd) error {
		if cmd.Dir == c.Path && slices.Contains(cmd.Args, "status") {
			return os.WriteFile(filepath.Join(filepath.Dir(c.Path), "owner"), []byte("external-owner"), 0600)
		}
		return nil
	})
	if err := m.Cleanup(ctx, c, ""); err == nil {
		t.Fatal("cleanup removed another owner's checkout")
	}
	if _, err := os.Stat(c.Path); err != nil {
		t.Fatal(err)
	}
}

func TestWorktreeAdministrationSerializesRealCreates(t *testing.T) {
	m, root := fixture(t)
	ctx := context.Background()
	base := testSnapshot(t, m, root)
	entered, release := holdGit(m, func(cmd *exec.Cmd) bool {
		return slices.Contains(cmd.Args, "worktree") && slices.Contains(cmd.Args, "add")
	})
	defer release()
	results := make(chan error, 2)
	go func() { _, err := m.Create(ctx, base); results <- err }()
	awaitWorktree(t, entered)
	if m.admin.TryLock() {
		m.admin.Unlock()
		t.Fatal("worktree add lacks shared administration fence")
	}
	go func() { _, err := m.Create(ctx, base); results <- err }()
	sourceWaiters(t, m, filepath.Join(m.Slots[1], "tree"), 1)
	// Merges reserve the parent's live attributes, not checkout/admin work.
	if _, err := m.Merge(ctx, base, base, base); err != nil {
		t.Fatal(err)
	}
	release()
	for range 2 {
		if err := awaitWorktreeResult(t, results); err != nil {
			t.Fatal(err)
		}
	}
}

func TestConcurrentSandboxedCapturesAndMerges(t *testing.T) {
	if os.Getenv("POLLYTOOL_REQUIRE_SANDBOX_TESTS") != "1" {
		t.Skip("opt-in process sandbox")
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("sandbox platform")
	}
	old, root := fixture(t)
	registry := tools.NewToolRegistry(nil, tools.WithSandboxFactory(sandbox.New, sandbox.DefaultConfig()))
	defer registry.Close()
	m, err := New(context.Background(), Config{Root: root, Directory: old.Directory, Registry: registry})
	if err != nil {
		t.Fatal(err)
	}
	base := testSnapshot(t, m, root)
	copies := make([]Checkout, 4)
	for i := range copies {
		copies[i] = testCheckout(t, m, base)
		writeTest(t, filepath.Join(copies[i].Path, "a.txt"), "sandboxed edit\n")
	}
	runMeasuredWorktree(t, len(copies), "native sandbox capture/merge/cleanup", func(ctx context.Context, i int) error {
		s, err := m.Capture(ctx, copies[i].Path)
		if err != nil {
			return err
		}
		merged, err := m.Merge(ctx, base, base, s)
		if err != nil {
			return err
		}
		if len(merged.Conflicts) != 0 || merged.Snapshot.Tree != s.Tree {
			return errors.New("concurrent merge lost captured edit")
		}
		if err := m.ValidateSnapshot(ctx, merged.Snapshot); err != nil {
			return err
		}
		return m.Cleanup(ctx, copies[i], s.Tree)
	})
}

func TestSlotHintProbesAndRestartOwnership(t *testing.T) {
	m, root := fixture(t)
	ctx := context.Background()
	m.Slots = SlotPaths(m.Directory, 4)
	m.MaxWorktrees = 4
	base := testSnapshot(t, m, root)
	if err := os.MkdirAll(m.Slots[0], 0700); err != nil {
		t.Fatal(err)
	}
	writeTest(t, filepath.Join(m.Slots[0], "owner"), "external-owner")
	metrics := new(worktreeTimings)
	measured := context.WithValue(ctx, worktreeTimingsKey{}, metrics)
	copies := make([]Checkout, 3)
	for i := range copies {
		var err error
		copies[i], err = m.Create(measured, base)
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := metrics.slotProbes.Load(); got != 4 {
		t.Fatalf("claims probed %d slots, want 4", got)
	}
	if err := m.Cleanup(ctx, copies[1], ""); err != nil {
		t.Fatal(err)
	}
	copy, err := m.Create(measured, base)
	if err != nil {
		t.Fatal(err)
	}
	if copy.Path != copies[1].Path || copy.ID == copies[1].ID {
		t.Fatal("released slot was not freshly claimed")
	}
	if got := metrics.slotProbes.Load(); got != 5 {
		t.Fatalf("reuse probed %d total slots, want 5", got)
	}
	writeTest(t, filepath.Join(copy.ScratchDir(), "keep"), "new owner's scratch")
	if err := m.FinishCleanup(ctx, copies[1], ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(copy.ScratchDir(), "keep")); err != nil {
		t.Fatal("old release touched new scratch", err)
	}
	restarted, err := New(ctx, m.Config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Create(ctx, base); err == nil || !strings.Contains(err.Error(), "capacity") {
		t.Fatalf("restart stole live slot: %v", err)
	}
	if err := os.Remove(filepath.Join(m.Slots[0], "owner")); err != nil {
		t.Fatal(err)
	}
	next, err := restarted.Create(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if next.Path != filepath.Join(m.Slots[0], "tree") {
		t.Fatal("restart failed to discover externally freed slot")
	}
}

func TestSnapshotRefPinningUsesSharedFence(t *testing.T) {
	m, root := fixture(t)
	m.sandbox = commandBarrier(func(cmd *exec.Cmd) error {
		if slices.Contains(cmd.Args, "update-ref") && m.refsMu.TryLock() {
			m.refsMu.Unlock()
			return errors.New("snapshot ref update lacks shared storage fence")
		}
		return nil
	})
	testSnapshot(t, m, root)
}

func TestSnapshotRefCleanupWaitsForCapture(t *testing.T) {
	m, root := fixture(t)
	ctx := context.Background()
	entered, release := holdGit(m, func(cmd *exec.Cmd) bool { return slices.Contains(cmd.Args, "commit-tree") })
	defer release()
	capture := make(chan error, 1)
	go func() { _, err := m.Capture(ctx, root); capture <- err }()
	awaitWorktree(t, entered)
	cleanup := make(chan error, 1)
	go func() { cleanup <- m.CleanupSnapshotRefs(ctx) }()
	// A pending writer fences new readers; this checks actual lock state rather
	// than declaring success after an arbitrary sleep.
	deadline := time.Now().Add(10 * time.Second)
	for m.mu.TryRLock() {
		m.mu.RUnlock()
		if time.Now().After(deadline) {
			t.Fatal("ref cleanup did not reach its exclusive fence")
		}
		time.Sleep(time.Millisecond)
	}
	release()
	if err := awaitWorktreeResult(t, capture); err != nil {
		t.Fatal(err)
	}
	if err := awaitWorktreeResult(t, cleanup); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(m.Directory, "snapshot-*.json"))
	if err != nil || len(files) != 0 {
		t.Fatalf("unfinished snapshot cleanup: %v %v", files, err)
	}
	if n := snapshotRefCount(t, root); n != 0 {
		t.Fatalf("%d unretired refs", n)
	}
}
