package swarm

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/alexschlessinger/pollytool/sessions"
)

type notificationOwnedReader struct {
	sessions.CoordinationReader
	values []sessions.Revision
	err    error
}

func (*notificationOwnedReader) Identity() (sessions.CoordinationIdentity, error) {
	return sessions.CoordinationIdentity{ActorID: "root", ParentID: "root"}, nil
}
func (r *notificationOwnedReader) Revisions([]sessions.RevisionScope) ([]sessions.Revision, error) {
	return r.values, r.err
}

func TestNotificationsSourceCopiesAndPropagatesCallbackErrors(t *testing.T) {
	t.Parallel()
	scope := changeTestScope("source")
	raw := &notificationOwnedReader{values: []sessions.Revision{{Scope: scope, Token: "owned"}}}
	source := &notificationSource{root: "root", durable: true, read: func(ctx context.Context, fn func(sessions.CoordinationReader) error) error {
		err := fn(raw)
		raw.values[0].Token = "callback-storage-reused"
		return err
	}}
	got, err := source.Revisions(t.Context(), []sessions.RevisionScope{scope})
	if err != nil || len(got) != 1 || got[0].Token != "present:owned" {
		t.Fatalf("reader-owned revisions escaped callback: %+v %v", got, err)
	}
	failure := errors.New("revision callback failed")
	raw.err = failure
	// Even a host that incorrectly swallows a callback failure cannot install a
	// successful empty vector or overwrite the caller's known baseline.
	source.read = func(ctx context.Context, fn func(sessions.CoordinationReader) error) error {
		_ = fn(raw)
		return nil
	}
	if got, err := source.Revisions(t.Context(), []sessions.RevisionScope{scope}); !errors.Is(err, failure) || got != nil {
		t.Fatalf("swallowed reader failure succeeded: %+v %v", got, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	raw.err = nil
	source.read = func(ctx context.Context, fn func(sessions.CoordinationReader) error) error {
		if err := fn(raw); err != nil {
			return err
		}
		cancel()
		return nil
	}
	if got, err := source.Revisions(ctx, []sessions.RevisionScope{scope}); !errors.Is(err, context.Canceled) || got != nil {
		t.Fatalf("post-callback cancellation succeeded: %+v %v", got, err)
	}
}

func TestNotificationsSourceRejectsMalformedRevisionVectors(t *testing.T) {
	t.Parallel()
	a, b := changeTestScope("a"), changeTestScope("b")
	for name, vector := range map[string][]sessions.Revision{
		"missing":   {{Scope: a}},
		"duplicate": {{Scope: a}, {Scope: a}},
		"foreign":   {{Scope: a}, {Scope: changeTestScope("other")}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			raw := &notificationOwnedReader{values: vector}
			source := &notificationSource{root: "root", durable: true, read: func(ctx context.Context, fn func(sessions.CoordinationReader) error) error { return fn(raw) }}
			n, clock, start := notificationTestProvider(t, source)
			s := notificationTestSubscribe(t, n, a, b)
			start()
			timer := changeTestReceive(t, clock.timers)
			if timer.delay != 2*defaultNotificationPolling().Interval {
				t.Fatal("malformed revision vector did not back off")
			}
			changeTestQuiet(t, s.sub)
			n.hub.mu.Lock()
			defer n.hub.mu.Unlock()
			for _, entry := range n.hub.scopes {
				if entry.known {
					t.Fatal("malformed vector installed a baseline")
				}
			}
		})
	}
}

// An older arbitrary durable host can support existing scopes while rejecting
// a new revision-only scope. The source must not turn that optional capability
// mismatch into a permanently sleeping subscriber or a whole-state read.
func TestNotificationsUnsupportedSelectorFallsBackPerBatch(t *testing.T) {
	t.Parallel()
	oldScope := changeTestScope("supported")
	newScope := sessions.RevisionScope{Index: "semantic.news"}
	for _, unsupported := range []error{sessions.ErrCoordinationSelector, sessions.ErrCoordinationUnsupported} {
		t.Run(unsupported.Error(), func(t *testing.T) {
			t.Parallel()
			raw := &notificationOldHostReader{unsupported: unsupported, newScope: newScope}
			source := &notificationSource{root: "root", durable: true, read: func(ctx context.Context, fn func(sessions.CoordinationReader) error) error { return fn(raw) }}
			before, err := source.Revisions(t.Context(), []sessions.RevisionScope{oldScope})
			if err != nil {
				t.Fatal(err)
			}
			first, err := source.Revisions(t.Context(), []sessions.RevisionScope{newScope, ActivityScope()})
			if err != nil || len(first) != 2 {
				t.Fatalf("unsupported batch: %+v %v", first, err)
			}
			second, err := source.Revisions(t.Context(), []sessions.RevisionScope{newScope, ActivityScope()})
			if err != nil || len(second) != 2 || first[1].Token == second[1].Token || first[0] != second[0] {
				t.Fatalf("fallback failed to hint durable scope only: first=%+v second=%+v err=%v", first, second, err)
			}
			after, err := source.Revisions(t.Context(), []sessions.RevisionScope{oldScope})
			if err != nil || len(after) != 1 || before[0] != after[0] {
				t.Fatalf("unsupported batch invalidated supported batch: before=%+v after=%+v err=%v", before, after, err)
			}
			// Unsupported scopes cannot bypass exactly-once validation.
			source.read = func(ctx context.Context, fn func(sessions.CoordinationReader) error) error {
				_ = fn(raw)
				_ = fn(raw)
				return nil
			}
			if got, err := source.Revisions(t.Context(), []sessions.RevisionScope{newScope}); !errors.Is(err, sessions.ErrCoordinationRequest) || got != nil {
				t.Fatalf("unsupported selector hid callback replay: %+v %v", got, err)
			}
		})
	}
}

type notificationOldHostReader struct {
	notificationOwnedReader
	unsupported error
	newScope    sessions.RevisionScope
}

func (r *notificationOldHostReader) Revisions(scopes []sessions.RevisionScope) ([]sessions.Revision, error) {
	var out []sessions.Revision
	for _, scope := range scopes {
		if scope == r.newScope {
			return nil, r.unsupported
		}
		out = append(out, sessions.Revision{Scope: scope, Token: "stable"})
	}
	return out, nil
}

func TestNotificationsRuntimeConcurrentSubscribersShareOneWorker(t *testing.T) {
	t.Parallel()
	host := newNotificationTestHost()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r := &Runtime{parent: host, ctx: ctx}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var subscriptions []*ChangeSubscription
	for range 64 {
		wg.Go(func() {
			sub, err := r.Subscribe(t.Context(), nil)
			if err != nil {
				t.Errorf("subscribe: %v", err)
				return
			}
			mu.Lock()
			subscriptions = append(subscriptions, sub)
			mu.Unlock()
		})
	}
	wg.Wait()
	if len(subscriptions) != 64 {
		t.Fatal("lost concurrent subscribers")
	}
	n := r.notifications
	t.Cleanup(n.Close)
	changeTestCounts(t, n.hub, 64, 0)
	cancel()
	n.Close()
	for _, sub := range subscriptions {
		if sub.sub.hub != n.hub {
			t.Fatal("subscriber attached to another hub")
		}
		changeTestReceive(t, sub.Done())
	}
	changeTestCounts(t, n.hub, 0, 0)
}
