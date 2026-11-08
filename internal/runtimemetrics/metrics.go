// Package runtimemetrics provides opt-in, bounded observations of runtime work.
// No recorder is installed by default. Instruments accept only fixed enums and
// numbers: they cannot retain payloads, record IDs, or host callbacks. A recorder
// belongs to an experiment/context, not to a process-wide metrics registry.
package runtimemetrics

import (
	"context"
	"math/bits"
	"sync/atomic"
	"time"
)

// Counter identifies a fixed unit of observed work. SQL counters must be wired
// at SQL boundaries; adapter result counts are not SQL row/statement counts.
type Counter uint8

const (
	ReadTransactions Counter = iota
	WriteTransactions
	SQLStatements
	RowsRead
	PayloadBytesRead
	RecordsDecoded
	RecordsEncoded
	CoordinationReads
	CoordinationUpdates
	CoordinationRecordsRead
	CoordinationBytesRead
	SummaryReads
	SummariesRead
	TranscriptReads
	TranscriptMessagesRead
	ExecutionsStarted
	ExecutionsCompleted
	ToolsStarted
	ToolsCompleted
	counterCount
)

// Phase identifies a fixed duration boundary, not a dynamic label. Durations
// describe inclusive wall time, not CPU time; nested phases can overlap.
type Phase uint8

const (
	ReadTransaction Phase = iota
	WriteTransaction
	CoordinationRead
	CoordinationUpdate
	StateDecode
	StateEncode
	Admission
	Execution
	Checkpoint
	ToolQueue
	ToolExecution
	ToolConstruction
	WorktreeLockWait
	WorktreeLockHold
	GitCommand
	phaseCount
)

type histogram struct {
	count, nanos, max atomic.Uint64
	buckets           [64]atomic.Uint64
}

// Recorder has a fixed memory footprint regardless of record or event count.
// Its zero value is ready to use. Do not copy a Recorder after first use.
type Recorder struct {
	counters [counterCount]atomic.Uint64
	phases   [phaseCount]histogram
}

func New() *Recorder { return &Recorder{} }

type contextKey struct{}

// WithRecorder enables observation for this context and its descendants. A nil
// recorder explicitly disables inherited observations. It changes no policies.
func WithRecorder(ctx context.Context, recorder *Recorder) context.Context {
	return context.WithValue(ctx, contextKey{}, recorder)
}

func FromContext(ctx context.Context) *Recorder {
	if ctx == nil {
		return nil
	}
	r, _ := ctx.Value(contextKey{}).(*Recorder)
	return r
}

func Add(ctx context.Context, counter Counter, n uint64) {
	if r := FromContext(ctx); r != nil && counter < counterCount {
		r.counters[counter].Add(n)
	}
}

func Observe(ctx context.Context, phase Phase, elapsed time.Duration) {
	observe(FromContext(ctx), phase, elapsed)
}

func observe(r *Recorder, phase Phase, elapsed time.Duration) {
	if r == nil || phase >= phaseCount || elapsed < 0 {
		return
	}
	h := &r.phases[phase]
	n := uint64(elapsed)
	h.buckets[bits.Len64(n)].Add(1)
	h.nanos.Add(n)
	for old := h.max.Load(); n > old; old = h.max.Load() {
		if h.max.CompareAndSwap(old, n) {
			break
		}
	}
	h.count.Add(1)
}

// Timer is an optional observation, not a callback or scheduling hook. Stop it
// exactly once. A disabled Start does not read the clock or allocate.
type Timer struct {
	recorder *Recorder
	phase    Phase
	start    time.Time
}

func Start(ctx context.Context, phase Phase) Timer {
	if r := FromContext(ctx); r != nil && phase < phaseCount {
		return Timer{recorder: r, phase: phase, start: time.Now()}
	}
	return Timer{}
}

func (t Timer) Stop() {
	if t.recorder != nil {
		observe(t.recorder, t.phase, time.Since(t.start))
	}
}

// Timing retains a logarithmic histogram, not individual samples. Bucket zero
// is zero duration; bucket i>0 includes [2^(i-1), 2^i-1] nanoseconds.
type Timing struct {
	Count, Nanoseconds uint64
	Max                time.Duration
	Buckets            [64]uint64
}

// Percentile returns an inclusive bucket upper bound, capped by the observed
// maximum. This is an approximation, not an exact sample quantile. p is 1..100;
// invalid percentiles and empty observations return zero.
func (t Timing) Percentile(p uint) time.Duration {
	if t.Count == 0 || p == 0 || p > 100 {
		return 0
	}
	// Avoid multiplying the full count (and overflowing) before dividing.
	target := t.Count/100*uint64(p) + (t.Count%100*uint64(p)+99)/100
	var seen uint64
	for i, count := range t.Buckets {
		seen += count
		if seen >= target {
			upper := time.Duration(uint64(1)<<i - 1)
			return min(upper, t.Max)
		}
	}
	return t.Max
}

// Snapshot is owned numeric data. Concurrent collection is race-free but not
// transactional across counters; quiesce an experiment before exact assertions.
type Snapshot struct {
	counters [counterCount]uint64
	phases   [phaseCount]Timing
}

func (r *Recorder) Snapshot() Snapshot {
	var s Snapshot
	if r == nil {
		return s
	}
	for i := range s.counters {
		s.counters[i] = r.counters[i].Load()
	}
	for i := range s.phases {
		h := &r.phases[i]
		t := &s.phases[i]
		t.Count, t.Nanoseconds, t.Max = h.count.Load(), h.nanos.Load(), time.Duration(h.max.Load())
		for j := range t.Buckets {
			t.Buckets[j] = h.buckets[j].Load()
		}
	}
	return s
}

func (s Snapshot) Count(counter Counter) uint64 {
	if counter >= counterCount {
		return 0
	}
	return s.counters[counter]
}

func (s Snapshot) Timing(phase Phase) Timing {
	if phase >= phaseCount {
		return Timing{}
	}
	return s.phases[phase]
}
