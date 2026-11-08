package worktree

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

// This opt-in component fixture uses real Git and the ordinary manager paths.
// It is not an end-to-end coordination or sandbox throughput benchmark.
func TestRuntimeStorageScaleWorktreeContention(t *testing.T) {
	if os.Getenv("POLLYTOOL_WORKTREE_MEASURE") == "" {
		t.Skip("set POLLYTOOL_WORKTREE_MEASURE=1 for controlled manager curves")
	}
	for _, n := range []int{1, 4, 8} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			m, root := fixture(t)
			m.Slots = SlotPaths(m.Directory, 2*n)
			base, err := m.Capture(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			copies := make([]Checkout, n)
			for i := range copies {
				copies[i], err = m.Create(context.Background(), base)
				if err != nil {
					t.Fatal(err)
				}
				writeTest(t, filepath.Join(copies[i].Path, "a.txt"), fmt.Sprintf("worker %d\n", i))
			}
			runMeasuredWorktree(t, n, "capture/create/cleanup", func(ctx context.Context, i int) error {
				snap, err := m.Capture(ctx, copies[i].Path)
				if err != nil {
					return err
				}
				copy, err := m.Create(ctx, snap)
				if err != nil {
					return err
				}
				if err = m.Cleanup(ctx, copy, ""); err != nil {
					return err
				}
				return m.Cleanup(ctx, copies[i], snap.Tree)
			})
		})
	}
}

// Parent merges deliberately share a source reservation for live attributes,
// but release it before ref pinning. Measure that residual independently from
// checkout allocation/cleanup; this is not an end-to-end throughput claim.
func TestRuntimeStorageScaleParentMerges(t *testing.T) {
	if os.Getenv("POLLYTOOL_WORKTREE_MEASURE") == "" {
		t.Skip("set POLLYTOOL_WORKTREE_MEASURE=1 for controlled manager curves")
	}
	for _, n := range []int{1, 4, 8} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			m, root := fixture(t)
			base := testSnapshot(t, m, root)
			c := testCheckout(t, m, base)
			writeTest(t, filepath.Join(c.Path, "a.txt"), "candidate\n")
			candidate := testSnapshot(t, m, c.Path)
			runMeasuredWorktree(t, n, "parent merge", func(ctx context.Context, _ int) error {
				got, err := m.Merge(ctx, base, base, candidate)
				if err == nil && (len(got.Conflicts) != 0 || got.Snapshot.Tree != candidate.Tree) {
					return fmt.Errorf("merge changed candidate tree: %+v", got)
				}
				return err
			})
		})
	}
}

func runMeasuredWorktree(t *testing.T, n int, phase string, run func(context.Context, int) error) {
	t.Helper()
	metrics := make([]worktreeTimings, n)
	elapsed := make([]time.Duration, n)
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ctx := context.WithValue(context.Background(), worktreeTimingsKey{}, &metrics[i])
			begin := time.Now()
			errs[i] = run(ctx, i)
			elapsed[i] = time.Since(begin)
		}()
	}
	begin := time.Now()
	close(start)
	wg.Wait()
	wall := time.Since(begin)
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	manager, source, admin, refs, git := make([]time.Duration, n), make([]time.Duration, n), make([]time.Duration, n), make([]time.Duration, n), make([]time.Duration, n)
	var probes, calls int64
	for i := range n {
		manager[i] = time.Duration(metrics[i].managerWait.Load())
		source[i] = time.Duration(metrics[i].sourceWait.Load())
		admin[i] = time.Duration(metrics[i].adminWait.Load())
		refs[i] = time.Duration(metrics[i].refWait.Load())
		git[i] = time.Duration(metrics[i].gitTime.Load())
		probes += metrics[i].slotProbes.Load()
		calls += metrics[i].gitCalls.Load()
	}
	summary := func(values []time.Duration) string {
		slices.Sort(values)
		return fmt.Sprintf("p50=%s p95=%s max=%s", values[(len(values)-1)/2], values[(95*len(values)+99)/100-1], values[len(values)-1])
	}
	t.Logf("%s n=%d wall=%s latency{%s} manager-wait{%s} source-wait{%s} admin-wait{%s} ref-wait{%s} git{%s} git-calls=%d slot-probes=%d", phase, n, wall, summary(elapsed), summary(manager), summary(source), summary(admin), summary(refs), summary(git), calls, probes)
	t.Log("Retained boundaries: same-source observations/writes (including live parent attributes for merge/diff), worktree administration including final cleanup proof, ref pinning and exclusive snapshot-ref cleanup; Git time includes process launch/output drain; summed worker Git time can exceed wall time.")
}
