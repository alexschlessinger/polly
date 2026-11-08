package runtimemetrics

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestRuntimeStorageScaleMetricsDisabled(t *testing.T) {
	// AllocsPerRun requires a serial test.
	ctx := context.Background()
	r := New()
	disabled := WithRecorder(WithRecorder(ctx, r), nil)
	for _, ctx := range []context.Context{ctx, disabled} {
		if got := testing.AllocsPerRun(100, func() {
			Add(ctx, RowsRead, 1)
			Observe(ctx, ReadTransaction, time.Second)
			Start(ctx, ReadTransaction).Stop()
		}); got != 0 {
			t.Fatalf("disabled allocations = %v", got)
		}
		if timer := Start(ctx, ReadTransaction); !timer.start.IsZero() {
			t.Fatal("disabled timer read the clock")
		}
	}
	if r.Snapshot() != (Snapshot{}) {
		t.Fatal("disabled observations reached recorder")
	}
}

func TestRuntimeStorageScaleMetricsConcurrent(t *testing.T) {
	// The worker goroutines are concurrent; the allocation check must be serial.
	r := New()
	ctx := WithRecorder(context.Background(), r)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				Add(ctx, RowsRead, 3)
				Observe(ctx, ReadTransaction, 10*time.Nanosecond)
			}
		})
	}
	wg.Wait()
	s := r.Snapshot()
	h := s.Timing(ReadTransaction)
	if s.Count(RowsRead) != 2400 || h.Count != 800 || h.Nanoseconds != 8000 || h.Max != 10*time.Nanosecond || h.Buckets[4] != 800 {
		t.Fatalf("lost observations: rows=%d timing=%+v", s.Count(RowsRead), h)
	}
	Add(ctx, RowsRead, 1)
	if s.Count(RowsRead) != 2400 {
		t.Fatal("snapshot aliases recorder")
	}
	if got := testing.AllocsPerRun(100, func() {
		Add(ctx, RowsRead, 1)
		Observe(ctx, ReadTransaction, time.Second)
		Start(ctx, ReadTransaction).Stop()
	}); got != 0 {
		t.Fatalf("enabled observation allocations = %v", got)
	}
}

func TestRuntimeStorageScaleMetricsHistogram(t *testing.T) {
	t.Parallel()
	r := New()
	ctx := WithRecorder(context.Background(), r)
	for _, d := range []time.Duration{0, 1, 2, 3, 4, 7, 8, 15, 16, time.Duration(1<<63 - 1)} {
		Observe(ctx, Checkpoint, d)
	}
	Observe(ctx, Checkpoint, -1)
	Observe(ctx, Phase(255), time.Second)
	Add(ctx, Counter(255), 1)
	h := r.Snapshot().Timing(Checkpoint)
	if h.Count != 10 || h.Percentile(50) != 7 || h.Percentile(90) != 31 || h.Percentile(100) != h.Max || h.Percentile(101) != 0 {
		t.Fatalf("histogram = %+v; p50=%v p90=%v p100=%v", h, h.Percentile(50), h.Percentile(90), h.Percentile(100))
	}
	var empty *Recorder
	if empty.Snapshot() != (Snapshot{}) || (Snapshot{}).Count(Counter(255)) != 0 || (Snapshot{}).Timing(Phase(255)) != (Timing{}) {
		t.Fatal("invalid/disabled snapshot was not empty")
	}
}
