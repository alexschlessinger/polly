package swarm

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/sessions"
)

// This is a functional consistent-view revision/readiness source, not a
// success-only stub. Every source call captures a view, announces a barrier and
// waits for its release. set commits atomically without any local notification.
type changeTestSource struct {
	mu     sync.Mutex
	state  map[sessions.RevisionScope]changeTestState
	calls  chan *changeTestCall
	active atomic.Int32
}

type changeTestState struct {
	token string
	ready bool
}

type changeTestCall struct {
	scopes    []sessions.RevisionScope
	revisions []sessions.Revision
	err       error
	release   chan struct{}
}

func newChangeTestSource() *changeTestSource {
	return &changeTestSource{state: make(map[sessions.RevisionScope]changeTestState), calls: make(chan *changeTestCall, 1)}
}

func (s *changeTestSource) set(scope sessions.RevisionScope, token string, ready bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state[scope] = changeTestState{token, ready}
}

func (s *changeTestSource) read(scope sessions.RevisionScope) (bool, sessions.Revision) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.state[scope]
	return state.ready, sessions.Revision{Scope: scope, Token: state.token}
}

func (s *changeTestSource) Revisions(ctx context.Context, scopes []sessions.RevisionScope) ([]sessions.Revision, error) {
	s.active.Add(1)
	defer s.active.Add(-1)
	call := &changeTestCall{scopes: append([]sessions.RevisionScope(nil), scopes...), release: make(chan struct{})}
	s.mu.Lock()
	for _, scope := range scopes {
		call.revisions = append(call.revisions, sessions.Revision{Scope: scope, Token: s.state[scope].token})
	}
	s.mu.Unlock()
	select {
	case s.calls <- call:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case <-call.release:
		return call.revisions, call.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type changeTestClock struct {
	timers chan *changeTestTimer
	active atomic.Int32
}

type changeTestTimer struct {
	clock   *changeTestClock
	delay   time.Duration
	ch      chan time.Time
	stopped chan struct{}
	once    sync.Once
}

func newChangeTestClock() *changeTestClock {
	return &changeTestClock{timers: make(chan *changeTestTimer, 16)}
}

func (c *changeTestClock) NewTimer(delay time.Duration) revisionPollTimer {
	timer := &changeTestTimer{clock: c, delay: delay, ch: make(chan time.Time, 1), stopped: make(chan struct{})}
	c.active.Add(1)
	c.timers <- timer
	return timer
}

func (t *changeTestTimer) C() <-chan time.Time { return t.ch }
func (t *changeTestTimer) Stop() {
	t.once.Do(func() { t.clock.active.Add(-1); close(t.stopped) })
}
func (t *changeTestTimer) fire() { t.ch <- time.Time{} }

func changeTestPoller(t *testing.T, h *changeHub, source revisionSource, clock *changeTestClock, batch int) *revisionPoller {
	t.Helper()
	p, err := newRevisionPoller(context.Background(), h, source, revisionPollerConfig{
		Interval: time.Second, MaxBackoff: 8 * time.Second, BatchSize: batch, clock: clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func changeTestFinishRound(t *testing.T, source *changeTestSource, clock *changeTestClock, batches int) *changeTestTimer {
	t.Helper()
	for range batches {
		call := changeTestReceive(t, source.calls)
		close(call.release)
	}
	return changeTestReceive(t, clock.timers)
}

func TestRuntimeStorageScaleRevisionPollerRemoteAndLostNotify(t *testing.T) {
	t.Parallel()
	h, source, clock := newChangeHub(), newChangeTestSource(), newChangeTestClock()
	a, b := changeTestScope("remote-recipient"), changeTestScope("unrelated-recipient")
	sa := h.Subscribe(context.Background(), []sessions.RevisionScope{a})
	sb := h.Subscribe(context.Background(), []sessions.RevisionScope{b})
	if ready, _ := source.read(a); ready {
		t.Fatal("fixture starts ready")
	}
	// Commit after readiness read, before the first poll, with no Publish.
	source.set(a, "opaque-first", true)
	changeTestPoller(t, h, source, clock, 0)
	timer := changeTestFinishRound(t, source, clock, 1)
	changeTestReceive(t, sa.Changes())
	changeTestReceive(t, sb.Changes()) // initial observations are conservative hints
	if ready, revision := source.read(a); !ready || revision.Token != "opaque-first" {
		t.Fatal("first poll hid the commit-before-notify crash")
	}
	// No source change: polling success itself must not become a fresh hint.
	timer.fire()
	timer = changeTestFinishRound(t, source, clock, 1)
	changeTestQuiet(t, sa)
	changeTestQuiet(t, sb)

	// Opaque tokens need equality only, not numeric ordering. A slow reader
	// coalesces multiple changes but always rereads the latest durable state.
	for _, token := range []string{"z", "a", ""} {
		source.set(a, token, true)
		timer.fire()
		timer = changeTestFinishRound(t, source, clock, 1)
		changeTestQuiet(t, sb)
		if len(sa.changes) != 1 {
			t.Fatalf("slow subscriber retained %d hints", len(sa.changes))
		}
	}
	changeTestReceive(t, sa.Changes())
	if _, revision := source.read(a); revision.Token != "" {
		t.Fatalf("hint consumption concealed latest revision: %+v", revision)
	}
}

func TestRuntimeStorageScaleRevisionPollerErrorsBackoffAndValidation(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"error", "missing", "duplicate", "unexpected"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			h, source, clock := newChangeHub(), newChangeTestSource(), newChangeTestClock()
			a, b := changeTestScope("a"), changeTestScope("b")
			s := h.Subscribe(context.Background(), []sessions.RevisionScope{a, b})
			changeTestPoller(t, h, source, clock, 0)
			timer := changeTestFinishRound(t, source, clock, 1)
			changeTestReceive(t, s.Changes())
			source.set(a, "committed-with-error", true)
			for _, delay := range []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second} {
				timer.fire()
				call := changeTestReceive(t, source.calls)
				switch mode {
				case "error":
					call.err = errors.New("transient source failure")
				case "missing":
					call.revisions = call.revisions[:1]
				case "duplicate":
					call.revisions[1] = call.revisions[0]
				case "unexpected":
					call.revisions[0].Scope = changeTestScope("unrequested")
				}
				close(call.release)
				timer = changeTestReceive(t, clock.timers)
				if timer.delay != delay {
					t.Fatalf("backoff=%s, want %s", timer.delay, delay)
				}
				changeTestQuiet(t, s)
				h.mu.Lock()
				token := h.scopes[a].token
				h.mu.Unlock()
				if token != "" {
					t.Fatal("failed/invalid read advanced a durable baseline")
				}
			}
			timer.fire()
			timer = changeTestFinishRound(t, source, clock, 1)
			if timer.delay != time.Second {
				t.Fatalf("success did not reset backoff: %s", timer.delay)
			}
			changeTestReceive(t, s.Changes())
			timer.fire()
			changeTestFinishRound(t, source, clock, 1)
			changeTestQuiet(t, s) // success isn't delivery or a recurring event
		})
	}
}

func TestRuntimeStorageScaleRevisionPollerBoundedActiveBatches(t *testing.T) {
	t.Parallel()
	h, source, clock := newChangeHub(), newChangeTestSource(), newChangeTestClock()
	const count = 2*sessions.MaxCoordinationRevisionScopes + 3
	subs := make(map[sessions.RevisionScope]*changeSubscription, count)
	for i := range count {
		scope := changeTestScope(fmt.Sprint(i))
		subs[scope] = h.Subscribe(context.Background(), []sessions.RevisionScope{scope, scope})
	}
	changeTestPoller(t, h, source, clock, 0)
	seen := make(map[sessions.RevisionScope]bool)
	for i := range 3 {
		call := changeTestReceive(t, source.calls)
		want := sessions.MaxCoordinationRevisionScopes
		if i == 2 {
			want = 3
		}
		if len(call.scopes) != want {
			t.Fatalf("batch size=%d, want %d", len(call.scopes), want)
		}
		for _, scope := range call.scopes {
			if seen[scope] {
				t.Fatal("active scope polled more than once in a round")
			}
			seen[scope] = true
		}
		// First partition unavailable: later partitions still make progress.
		if i == 0 {
			call.err = errors.New("partition unavailable")
		}
		close(call.release)
	}
	timer := changeTestReceive(t, clock.timers)
	if len(seen) != count || timer.delay != 2*time.Second {
		t.Fatalf("incomplete/backoff round: %d scopes, %s delay", len(seen), timer.delay)
	}
	if clock.active.Load() != 1 || source.active.Load() != 0 {
		t.Fatalf("many subscriptions require one timer, no parked source call: %d/%d", clock.active.Load(), source.active.Load())
	}
	keep := changeTestScope("0")
	for scope, sub := range subs {
		if scope != keep {
			sub.Close()
		}
	}
	changeTestCounts(t, h, 1, 1)
	timer.fire()
	call := changeTestReceive(t, source.calls)
	if len(call.scopes) != 1 || call.scopes[0] != keep {
		t.Fatalf("historical registrations survived: %+v", call.scopes)
	}
	close(call.release)
	changeTestReceive(t, clock.timers)
}

func TestRuntimeStorageScaleRevisionPollerRegistrationDuringRead(t *testing.T) {
	t.Parallel()
	h, source, clock := newChangeHub(), newChangeTestSource(), newChangeTestClock()
	a := changeTestScope("recreated")
	old := h.Subscribe(context.Background(), []sessions.RevisionScope{a})
	changeTestPoller(t, h, source, clock, 0)
	call := changeTestReceive(t, source.calls) // old registration's snapshot
	old.Close()
	replacement := h.Subscribe(context.Background(), []sessions.RevisionScope{a})
	source.set(a, "after-recreation", true)
	close(call.release)
	timer := changeTestReceive(t, clock.timers)
	changeTestQuiet(t, replacement)
	h.mu.Lock()
	known := h.scopes[a].known
	h.mu.Unlock()
	if known {
		t.Fatal("stale in-flight read initialized a new registration")
	}
	timer.fire()
	changeTestFinishRound(t, source, clock, 1)
	changeTestReceive(t, replacement.Changes())
	if ready, revision := source.read(a); !ready || revision.Token != "after-recreation" {
		t.Fatal("replacement missed remote commit")
	}
}

func TestRuntimeStorageScaleRevisionPollerIdleReactivationAndSmallBatches(t *testing.T) {
	t.Parallel()
	h, source, clock := newChangeHub(), newChangeTestSource(), newChangeTestClock()
	changeTestPoller(t, h, source, clock, 2)
	a, b, c := changeTestScope("a"), changeTestScope("b"), changeTestScope("c")
	s := h.Subscribe(context.Background(), []sessions.RevisionScope{a, b, c})
	for _, size := range []int{2, 1} {
		call := changeTestReceive(t, source.calls)
		if len(call.scopes) != size {
			t.Fatalf("configured source bound: batch=%d, want %d", len(call.scopes), size)
		}
		close(call.release)
	}
	timer := changeTestReceive(t, clock.timers)
	changeTestReceive(t, s.Changes())
	s.Close()
	changeTestReceive(t, timer.stopped)
	changeTestCounts(t, h, 0, 0)
	if clock.active.Load() != 0 {
		t.Fatal("idle poller retained timer")
	}

	// Adding a scope must restart an idle poller without advancing the clock.
	s = h.Subscribe(context.Background(), []sessions.RevisionScope{a})
	if ready, _ := source.read(a); ready {
		t.Fatal("fixture starts ready")
	}
	call := changeTestReceive(t, source.calls)
	if len(call.scopes) != 1 || call.scopes[0] != a {
		t.Fatalf("reactivation polled historical scopes: %+v", call.scopes)
	}
	// The source captured an older view; the initial hint still forces a new
	// readiness read and the following poll detects the new opaque token.
	source.set(a, "remote-during-poll", true)
	close(call.release)
	timer = changeTestReceive(t, clock.timers)
	changeTestReceive(t, s.Changes())
	if ready, _ := source.read(a); !ready {
		t.Fatal("in-flight poll concealed the remote commit")
	}
	timer.fire()
	changeTestFinishRound(t, source, clock, 1)
	changeTestReceive(t, s.Changes())
}

func TestRuntimeStorageScaleRevisionPollerShutdownAndIdle(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"idle", "source-read", "timer", "parent-cancel", "hub-close"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			h, source, clock := newChangeHub(), newChangeTestSource(), newChangeTestClock()
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			s := h.Subscribe(context.Background(), []sessions.RevisionScope{changeTestScope("live")})
			p, err := newRevisionPoller(ctx, h, source, revisionPollerConfig{
				Interval: time.Second, MaxBackoff: time.Second, clock: clock,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(p.Close)
			call := changeTestReceive(t, source.calls)
			if phase == "idle" || phase == "timer" {
				close(call.release)
				timer := changeTestReceive(t, clock.timers)
				if phase == "idle" {
					s.Close()
					changeTestReceive(t, timer.stopped)
					if clock.active.Load() != 0 {
						t.Fatal("idle poller retained a timer")
					}
					// Empty subscriptions also belong to hub lifecycle cleanup.
					s = h.Subscribe(context.Background(), nil)
				}
			}
			if phase == "parent-cancel" {
				cancel()
			} else if phase == "hub-close" {
				h.Close()
			} else {
				p.Close()
			}
			changeTestReceive(t, p.done)
			p.Close()
			changeTestReceive(t, s.Done())
			if source.active.Load() != 0 || clock.active.Load() != 0 {
				t.Fatalf("shutdown leaked source calls/timers: %d/%d", source.active.Load(), clock.active.Load())
			}
			changeTestCounts(t, h, 0, 0)
		})
	}
}

func TestRuntimeStorageScaleRevisionPollerConfiguration(t *testing.T) {
	t.Parallel()
	h, source := newChangeHub(), newChangeTestSource()
	defer h.Close()
	for _, config := range []revisionPollerConfig{
		{}, {Interval: time.Second}, {Interval: -1, MaxBackoff: time.Second},
		{Interval: time.Second, MaxBackoff: time.Second, BatchSize: -1},
		{Interval: time.Second, MaxBackoff: time.Second, BatchSize: sessions.MaxCoordinationRevisionScopes + 1},
	} {
		if _, err := newRevisionPoller(context.Background(), h, source, config); err == nil {
			t.Fatalf("accepted invalid configuration: %+v", config)
		}
	}
	clock := newChangeTestClock()
	p := changeTestPoller(t, h, source, clock, 1)
	config := revisionPollerConfig{Interval: time.Second, MaxBackoff: time.Second}
	if _, err := newRevisionPoller(context.Background(), h, source, config); err == nil {
		t.Fatal("accepted two pollers on one hub")
	}
	if _, err := newRevisionPoller(context.Background(), nil, source, config); err == nil {
		t.Fatal("accepted nil hub")
	}
	if _, err := newRevisionPoller(context.Background(), h, nil, config); err == nil {
		t.Fatal("accepted nil source")
	}
	p.Close()
	if _, err := newRevisionPoller(context.Background(), h, source, config); err == nil {
		t.Fatal("accepted a poller on a closed hub")
	}
}
