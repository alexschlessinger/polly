package swarm

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/sessions"
)

// This host exposes only callback capabilities. Embedding the unused methods
// makes any attempted full-state read, preparation, lease, repair, or payload
// query fail rather than silently succeeding through a concrete SQLite path.
type notificationTestHost struct {
	sessions.CoordinationSession
	mu       sync.Mutex
	root     string
	durable  bool
	tokens   map[sessions.RevisionScope]string
	err      error
	reads    int
	requests [][]sessions.RevisionScope
}

func newNotificationTestHost() *notificationTestHost {
	return &notificationTestHost{root: "root", durable: true, tokens: make(map[sessions.RevisionScope]string)}
}
func (h *notificationTestHost) ViewID() string { return h.root }
func (h *notificationTestHost) CoordinationCapabilities() sessions.CoordinationCapabilities {
	// Durable revision callbacks need not advertise indexed payload access.
	return sessions.CoordinationCapabilities{DurableRevisions: h.durable}
}
func (h *notificationTestHost) ReadRecords(ctx context.Context, fn func(sessions.CoordinationReader) error) error {
	h.mu.Lock()
	h.reads++
	err := h.err
	raw := &notificationTestReader{identity: sessions.CoordinationIdentity{ActorID: h.root, ParentID: h.root}}
	for scope, token := range h.tokens {
		raw.revisions = append(raw.revisions, sessions.Revision{Scope: scope, Token: token})
	}
	h.mu.Unlock()
	if err != nil {
		return err
	}
	raw.request = func(scopes []sessions.RevisionScope) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.requests = append(h.requests, append([]sessions.RevisionScope(nil), scopes...))
	}
	return fn(raw)
}
func (h *notificationTestHost) UpdateRecords(context.Context, func(sessions.CoordinationTx) error) (sessions.CoordinationCommit, error) {
	panic("notification source attempted a write")
}
func (h *notificationTestHost) ReadRecordsView(ctx context.Context, root string, fn func(sessions.CoordinationReader) error) error {
	if root != h.root {
		return sessions.ErrSessionNotFound
	}
	return h.ReadRecords(ctx, fn)
}
func (h *notificationTestHost) source() *notificationSource {
	return &notificationSource{root: h.root, durable: h.durable, read: h.ReadRecords}
}
func (h *notificationTestHost) set(scope sessions.RevisionScope, token string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tokens[scope] = token
}
func (h *notificationTestHost) fail(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.err = err
}

type notificationTestReader struct {
	sessions.CoordinationReader
	identity  sessions.CoordinationIdentity
	revisions []sessions.Revision
	request   func([]sessions.RevisionScope)
}

func (r *notificationTestReader) Identity() (sessions.CoordinationIdentity, error) {
	return r.identity, nil
}
func (r *notificationTestReader) Revisions(scopes []sessions.RevisionScope) ([]sessions.Revision, error) {
	if r.request != nil {
		r.request(scopes)
	}
	out := make([]sessions.Revision, len(scopes))
	for i, scope := range scopes {
		out[i].Scope = scope
		for _, revision := range r.revisions {
			if revision.Scope == scope {
				out[i].Token = revision.Token
			}
		}
	}
	return out, nil
}

func notificationTestSubscribe(t *testing.T, n *Notifications, scopes ...sessions.RevisionScope) *ChangeSubscription {
	t.Helper()
	sub, err := n.Subscribe(t.Context(), scopes)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sub.Close)
	return sub
}

// Register before starting a fake-clock poller, avoiding scheduler-dependent
// initial batches while keeping the production hub/source/poller stack intact.
func notificationTestProvider(t *testing.T, source *notificationSource) (*Notifications, *changeTestClock, func()) {
	t.Helper()
	clock := newChangeTestClock()
	n := &Notifications{hub: newChangeHub(), source: source}
	start := func() {
		config := defaultNotificationPolling()
		config.clock = clock
		var err error
		n.poller, err = newRevisionPoller(t.Context(), n.hub, source, config)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(n.Close)
	}
	return n, clock, start
}

func TestNotificationsScopesReplaceAndValidation(t *testing.T) {
	t.Parallel()
	host := newNotificationTestHost()
	n, clock, start := notificationTestProvider(t, host.source())
	prefix := sessions.RevisionScope{Index: sessions.CoordinationIndexMemberNamePrefix, Key: "/root/a/"}
	s := notificationTestSubscribe(t, n, prefix, prefix, sessions.RevisionScope{}, ActivityScope())
	changeTestCounts(t, n.hub, 1, 3)
	if _, err := n.Subscribe(t.Context(), []sessions.RevisionScope{{Index: "unknown"}}); !errors.Is(err, sessions.ErrCoordinationRequest) {
		t.Fatalf("invalid selector: %v", err)
	}
	for _, invalid := range []sessions.RevisionScope{
		{Index: "unknown"}, {Index: activityIndex, Key: "bad"},
		{Index: sessions.CoordinationIndexRecord, Key: "not-a-tuple"},
		{Index: sessions.CoordinationIndexRecords, Key: "bad"},
	} {
		if added, err := s.Replace([]sessions.RevisionScope{invalid}); added || !errors.Is(err, sessions.ErrCoordinationRequest) {
			t.Fatalf("invalid replacement: added=%v err=%v", added, err)
		}
	}
	changeTestCounts(t, n.hub, 1, 3)
	prefix.Key = "/root/a"
	if added, err := s.Replace([]sessions.RevisionScope{prefix}); err != nil || added {
		t.Fatalf("normalization changed identity: %v, %v", added, err)
	}
	changeTestCounts(t, n.hub, 1, 1)
	if added, err := s.Replace([]sessions.RevisionScope{prefix, changeTestScope("new")}); err != nil || !added {
		t.Fatalf("new dependency did not require reread: %v, %v", added, err)
	}
	start()
	changeTestReceive(t, clock.timers)
	changeTestReceive(t, s.Changes())
	s.Close()
	changeTestReceive(t, s.Done())
	changeTestCounts(t, n.hub, 0, 0)
	if added, err := s.Replace([]sessions.RevisionScope{prefix}); err != nil || added {
		t.Fatalf("closed replacement: %v, %v", added, err)
	}
	changeTestQuiet(t, s.sub)
}

func TestNotificationsRemoteLocalNoopAndActivityIsolation(t *testing.T) {
	t.Parallel()
	host := newNotificationTestHost()
	a, b := changeTestScope("a"), changeTestScope("b")
	host.set(a, "before")
	n, clock, start := notificationTestProvider(t, host.source())
	sa, sb := notificationTestSubscribe(t, n, a), notificationTestSubscribe(t, n, b)
	activity := notificationTestSubscribe(t, n, ActivityScope())
	r := &Runtime{notifications: n}
	start()
	timer := changeTestReceive(t, clock.timers)
	for _, sub := range []*ChangeSubscription{sa, sb, activity} {
		changeTestReceive(t, sub.Changes()) // first observation closes read/subscribe races
	}
	host.set(a, "remote-without-notify")
	timer.fire()
	timer = changeTestReceive(t, clock.timers)
	changeTestReceive(t, sa.Changes())
	changeTestQuiet(t, sb.sub)
	changeTestQuiet(t, activity.sub)
	// A successful same-token observation and an empty commit are not hints.
	r.publishChanges(nil)
	timer.fire()
	timer = changeTestReceive(t, clock.timers)
	changeTestQuiet(t, sa.sub)
	changeTestQuiet(t, sb.sub)
	changeTestQuiet(t, activity.sub)
	// Local commits signal only their exact registered scopes without polling.
	r.publishChanges([]sessions.RevisionScope{b})
	changeTestReceive(t, sb.Changes())
	changeTestQuiet(t, sa.sub)
	r.publishActivity()
	changeTestReceive(t, activity.Changes())
	changeTestQuiet(t, sa.sub)
	changeTestQuiet(t, sb.sub)
	// Explicit conservative invalidation does not pretend Records is a wildcard.
	r.invalidateChanges()
	for _, sub := range []*ChangeSubscription{sa, sb, activity} {
		changeTestReceive(t, sub.Changes())
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	for _, scopes := range host.requests {
		for _, scope := range scopes {
			if scope == ActivityScope() {
				t.Fatal("runtime activity escaped into durable revision callback")
			}
		}
	}
}

func TestNotificationsErrorsMissingAndBaseline(t *testing.T) {
	t.Parallel()
	host := newNotificationTestHost()
	scope := changeTestScope("watched")
	host.set(scope, "missing") // arbitrary host tokens cannot collide with deletion
	n, clock, start := notificationTestProvider(t, host.source())
	sub := notificationTestSubscribe(t, n, scope)
	start()
	timer := changeTestReceive(t, clock.timers)
	changeTestReceive(t, sub.Changes())
	host.fail(errors.New("source unavailable"))
	for _, want := range []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second} {
		timer.fire()
		timer = changeTestReceive(t, clock.timers)
		if timer.delay != want {
			t.Fatalf("error backoff=%v want %v", timer.delay, want)
		}
		changeTestQuiet(t, sub.sub)
	}
	host.fail(nil)
	timer.fire()
	timer = changeTestReceive(t, clock.timers)
	changeTestQuiet(t, sub.sub) // errors did not discard the known baseline
	if timer.delay != 250*time.Millisecond {
		t.Fatalf("success did not reset backoff: %v", timer.delay)
	}
	host.fail(fmt.Errorf("deleted root: %w", sessions.ErrSessionNotFound))
	timer.fire()
	timer = changeTestReceive(t, clock.timers)
	changeTestReceive(t, sub.Changes())
	timer.fire()
	timer = changeTestReceive(t, clock.timers)
	changeTestQuiet(t, sub.sub) // stable missing root, not repeated invalidation
	host.fail(nil)
	timer.fire()
	changeTestReceive(t, clock.timers)
	changeTestReceive(t, sub.Changes()) // restored root differs from missing token
}

func TestNotificationsLegacyHintsWithoutReads(t *testing.T) {
	t.Parallel()
	host := newNotificationTestHost()
	host.durable = false
	n, clock, start := notificationTestProvider(t, host.source())
	s := notificationTestSubscribe(t, n, changeTestScope("legacy"))
	activity := notificationTestSubscribe(t, n, ActivityScope())
	start()
	timer := changeTestReceive(t, clock.timers)
	changeTestReceive(t, s.Changes())
	changeTestReceive(t, activity.Changes())
	for range 3 {
		timer.fire()
		timer = changeTestReceive(t, clock.timers)
		changeTestReceive(t, s.Changes())
		changeTestQuiet(t, activity.sub)
	}
	r := &Runtime{notifications: n}
	r.publishChanges(nil)
	changeTestQuiet(t, s.sub)
	changeTestQuiet(t, activity.sub)
	// Legacy commit scopes may omit derived partitions. A nonempty receipt
	// therefore hints even an unrelated exact dependency immediately.
	r.publishChanges([]sessions.RevisionScope{changeTestScope("unreported-derived-scope")})
	changeTestReceive(t, s.Changes())
	changeTestReceive(t, activity.Changes())
	host.mu.Lock()
	defer host.mu.Unlock()
	if host.reads != 0 {
		t.Fatalf("legacy revision polling performed %d full-state reads", host.reads)
	}
}

func TestNotificationsBatchingAndIdleCleanup(t *testing.T) {
	t.Parallel()
	host := newNotificationTestHost()
	n, clock, start := notificationTestProvider(t, host.source())
	var scopes []sessions.RevisionScope
	for i := range 2*sessions.MaxCoordinationRevisionScopes + 3 {
		scopes = append(scopes, changeTestScope(fmt.Sprint(i)))
	}
	sub := notificationTestSubscribe(t, n, scopes...)
	start()
	timer := changeTestReceive(t, clock.timers)
	changeTestReceive(t, sub.Changes())
	host.mu.Lock()
	if host.reads != 3 {
		t.Fatalf("read %d callbacks, want three bounded batches", host.reads)
	}
	seen := make(map[sessions.RevisionScope]bool)
	for _, request := range host.requests {
		if len(request) > sessions.MaxCoordinationRevisionScopes {
			t.Fatalf("oversized revision batch: %d", len(request))
		}
		for _, scope := range request {
			seen[scope] = true
		}
	}
	host.mu.Unlock()
	if len(seen) != len(scopes) {
		t.Fatalf("requested %d scopes, want %d", len(seen), len(scopes))
	}
	if added, err := sub.Replace(nil); err != nil || added {
		t.Fatalf("empty replace: %v %v", added, err)
	}
	changeTestReceive(t, timer.stopped)
	changeTestCounts(t, n.hub, 1, 0)
	if clock.active.Load() != 0 {
		t.Fatal("idle subscription retained a timer")
	}
	sub.Close()
	changeTestCounts(t, n.hub, 0, 0)
	n.Close()
	select {
	case <-clock.timers:
		t.Fatal("empty scope set started another timer")
	default:
	}
}

func TestNotificationsCallbackBoundary(t *testing.T) {
	t.Parallel()
	scope := changeTestScope("actor")
	reader := &notificationTestReader{identity: sessions.CoordinationIdentity{ActorID: "root", ParentID: "root"}}
	for name, read := range map[string]func(context.Context, func(sessions.CoordinationReader) error) error{
		"omitted": func(context.Context, func(sessions.CoordinationReader) error) error { return nil },
		"retry ignored": func(ctx context.Context, fn func(sessions.CoordinationReader) error) error {
			_ = fn(reader)
			_ = fn(reader)
			return nil
		},
		"foreign root ignored": func(ctx context.Context, fn func(sessions.CoordinationReader) error) error {
			_ = fn(&notificationTestReader{identity: sessions.CoordinationIdentity{ActorID: "other", ParentID: "other"}})
			return nil
		},
		"child": func(ctx context.Context, fn func(sessions.CoordinationReader) error) error {
			return fn(&notificationTestReader{identity: sessions.CoordinationIdentity{ActorID: "child", ParentID: "root"}})
		},
		"nil reader": func(ctx context.Context, fn func(sessions.CoordinationReader) error) error { return fn(nil) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			source := &notificationSource{root: "root", durable: true, read: read}
			if got, err := source.Revisions(t.Context(), []sessions.RevisionScope{scope}); !errors.Is(err, sessions.ErrCoordinationRequest) || got != nil {
				t.Fatalf("invalid callback succeeded: %+v %v", got, err)
			}
		})
	}
}

func TestNotificationsCancellationJoinsSource(t *testing.T) {
	t.Parallel()
	for _, shutdown := range []string{"close", "owner-cancel"} {
		t.Run(shutdown, func(t *testing.T) {
			t.Parallel()
			owner, cancel := context.WithCancel(t.Context())
			defer cancel()
			entered, exited := make(chan struct{}), make(chan struct{})
			var active atomic.Int32
			source := &notificationSource{root: "root", durable: true, read: func(ctx context.Context, fn func(sessions.CoordinationReader) error) error {
				active.Add(1)
				defer active.Add(-1)
				defer close(exited)
				close(entered)
				<-ctx.Done()
				return ctx.Err()
			}}
			n, err := newNotifications(owner, source, defaultNotificationPolling())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(n.Close)
			s := notificationTestSubscribe(t, n, changeTestScope("a"))
			changeTestReceive(t, entered)
			if shutdown == "owner-cancel" {
				cancel()
				changeTestReceive(t, n.poller.done)
			} else {
				n.Close()
			}
			changeTestReceive(t, exited)
			changeTestReceive(t, s.Done())
			if active.Load() != 0 {
				t.Fatal("shutdown abandoned in-flight source work")
			}
			changeTestCounts(t, n.hub, 0, 0)
			n.Close()
			closed := notificationTestSubscribe(t, n, changeTestScope("b"))
			changeTestReceive(t, closed.Done())
			changeTestCounts(t, n.hub, 0, 0)
		})
	}
}

func TestNotificationsRuntimeSharedOwnershipAndNoObserverWrites(t *testing.T) {
	t.Parallel()
	host := newNotificationTestHost()
	r := &Runtime{parent: host} // ctx and ID omitted by manually built runtimes
	r.publishChanges([]sessions.RevisionScope{changeTestScope("a")})
	r.publishActivity()
	r.invalidateChanges()
	if r.notifications != nil {
		t.Fatal("write allocated a notification provider without observers")
	}
	if _, err := r.Subscribe(t.Context(), []sessions.RevisionScope{{Index: "bad"}}); err == nil || r.notifications != nil {
		t.Fatal("invalid selector allocated an owned worker")
	}
	firstCtx, cancel := context.WithCancel(t.Context())
	first, err := r.Subscribe(firstCtx, nil)
	if err != nil {
		t.Fatal(err)
	}
	n := r.notifications
	t.Cleanup(n.Close)
	second, err := r.Subscribe(t.Context(), nil)
	if err != nil || r.notifications != n {
		t.Fatalf("runtime did not share provider: %v", err)
	}
	cancel()
	changeTestReceive(t, first.Done())
	select {
	case <-second.Done():
		t.Fatal("first subscriber owned the shared provider")
	default:
	}
	changeTestCounts(t, n.hub, 1, 0)
	n.Close()
	changeTestReceive(t, second.Done())
	host.mu.Lock()
	defer host.mu.Unlock()
	if host.reads != 0 || n.source.root != host.root {
		t.Fatalf("idle provider read state or lost supplied root: reads=%d root=%q", host.reads, n.source.root)
	}
}

func TestNotificationsViewUsesOnlySuppliedCapability(t *testing.T) {
	t.Parallel()
	host := newNotificationTestHost()
	n, err := NewViewNotifications(t.Context(), host, host.root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Close)
	// Calling the source directly avoids timing assumptions and exercises the
	// exact view callback selected by the exported constructor.
	scopes := []sessions.RevisionScope{changeTestScope("host"), ActivityScope()}
	got, err := n.source.Revisions(t.Context(), scopes)
	if err != nil || len(got) != len(scopes) {
		t.Fatalf("host callback revisions: %v %v", got, err)
	}
	first, err := n.source.Revisions(t.Context(), scopes)
	if err != nil || !reflect.DeepEqual(first, got) {
		t.Fatalf("unchanged arbitrary host revisions: %v %v", first, err)
	}
	for _, root := range []string{"", "another-root"} {
		other, err := NewViewNotifications(t.Context(), host, root)
		if root == "" {
			if !errors.Is(err, sessions.ErrCoordinationRequest) {
				t.Fatalf("empty root: %v", err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		other.Close()
	}
	if _, err := NewViewNotifications(t.Context(), struct{}{}, host.root); !errors.Is(err, sessions.ErrCoordinationUnsupported) {
		t.Fatalf("unsupported host: %v", err)
	}
	var nilHost *notificationTestHost
	if _, err := NewViewNotifications(t.Context(), nilHost, host.root); !errors.Is(err, sessions.ErrCoordinationUnsupported) {
		t.Fatalf("typed nil host: %v", err)
	}
}
