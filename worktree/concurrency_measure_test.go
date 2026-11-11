package worktree

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"
)

// runMeasuredWorktree runs n workers at once through run and logs where each
// spent its time: waiting on the manager, a source, administration or refs,
// and in Git.
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
