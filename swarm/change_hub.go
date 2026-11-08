package swarm

import (
	"context"
	"sync"

	"github.com/alexschlessinger/pollytool/sessions"
)

// changeHub routes hints, not payloads or delivery acknowledgements. Scopes are
// exact: the caller, not the hub, decides selector and audience semantics. Its
// memory is proportional to live subscriptions and their current dependencies.
// The zero value is not usable; construct it with newChangeHub.
type changeHub struct {
	mu            sync.Mutex
	closed        bool
	updated       chan struct{}
	subscriptions map[*changeSubscription]struct{}
	scopes        map[sessions.RevisionScope]*changeScope
	poller        *revisionPoller
}

// A scope entry is also its registration identity: an in-flight poll must not
// install an old baseline into a removed and subsequently recreated scope.
type changeScope struct {
	subscribers map[*changeSubscription]struct{}
	known       bool
	token       string
}

// changeSubscription has one coalesced dirty bit. Changes is never closed;
// producers and unsubscribe therefore cannot race a send with a channel close.
// Done is the independent lifetime signal. No goroutine waits per subscription.
//
// The wait protocol is:
//   - Subscribe before reading readiness and revisions in one consistent view.
//   - Drain an old Changes hint BEFORE each read, never after it.
//   - Replace discovered dependencies. If Replace returns true, repeat the read:
//     a newly discovered scope may have changed before it was registered.
//   - If not ready, select on Changes, Done and the operation context; reread on
//     Changes. Neither a hint nor a revision token establishes delivery/authority.
//
// Buffered hints cover local commits between subscribe/read/wait. An event
// before subscribe is found by the read, not retained history. One hub poller
// covers remote commits, uncertain commits and commit-before-notify crashes.
// Callers must inspect Done even when Replace returns false: closed is not ready.
// A subscription has one logical reader, though all methods are concurrency-safe.
type changeSubscription struct {
	hub     *changeHub
	changes chan struct{}
	done    chan struct{}
	// The fields below are protected by hub.mu.
	closed bool
	scopes map[sessions.RevisionScope]struct{}
	stop   func() bool
}

func newChangeHub() *changeHub {
	return &changeHub{
		updated:       make(chan struct{}, 1),
		subscriptions: make(map[*changeSubscription]struct{}),
		scopes:        make(map[sessions.RevisionScope]*changeScope),
	}
}

func (h *changeHub) Subscribe(ctx context.Context, scopes []sessions.RevisionScope) *changeSubscription {
	s := &changeSubscription{hub: h, changes: make(chan struct{}, 1), done: make(chan struct{})}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || ctx.Err() != nil {
		s.closed = true
		close(s.done)
		return s
	}
	h.subscriptions[s] = struct{}{}
	s.replaceLocked(scopes)
	// AfterFunc has no parked goroutine for ordinary cancellable contexts. Stop
	// detaches the callback when a caller closes before context cancellation.
	s.stop = context.AfterFunc(ctx, s.Close)
	return s
}

func (s *changeSubscription) Changes() <-chan struct{} { return s.changes }
func (s *changeSubscription) Done() <-chan struct{}    { return s.done }

// Replace installs new dependencies before dropping old ones, under the same
// lock as Publish. True means a registration race needs a fresh consistent read.
func (s *changeSubscription) Replace(scopes []sessions.RevisionScope) bool {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	if s.closed {
		return false
	}
	return s.replaceLocked(scopes)
}

func (s *changeSubscription) replaceLocked(scopes []sessions.RevisionScope) bool {
	next := make(map[sessions.RevisionScope]struct{}, len(scopes))
	added, changed := false, false
	for _, scope := range scopes {
		if _, duplicate := next[scope]; duplicate {
			continue
		}
		next[scope] = struct{}{}
		if _, present := s.scopes[scope]; present {
			continue
		}
		entry := s.hub.scopes[scope]
		if entry == nil {
			entry = &changeScope{subscribers: make(map[*changeSubscription]struct{})}
			s.hub.scopes[scope] = entry
			changed = true
		}
		entry.subscribers[s] = struct{}{}
		added = true
	}
	for scope := range s.scopes {
		if _, retain := next[scope]; !retain {
			changed = s.removeLocked(scope) || changed
		}
	}
	s.scopes = next
	if changed {
		s.hub.updatedLocked()
	}
	return added
}

// removeLocked reports whether the scope ceased to be active.
func (s *changeSubscription) removeLocked(scope sessions.RevisionScope) bool {
	entry := s.hub.scopes[scope]
	delete(entry.subscribers, s)
	if len(entry.subscribers) == 0 {
		delete(s.hub.scopes, scope)
		return true
	}
	return false
}

func (h *changeHub) updatedLocked() {
	select {
	case h.updated <- struct{}{}:
	default:
	}
}

// Publish accepts only the changed scopes of a known successful commit. It is
// nonblocking with respect to consumers, including slow or abandoned readers.
func (h *changeHub) Publish(scopes []sessions.RevisionScope) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, scope := range scopes {
		if entry := h.scopes[scope]; entry != nil {
			entry.signalLocked()
		}
	}
}

// Invalidate conservatively hints every active dependency without changing any
// durable baseline. Unlike Publish, it asserts no successful scoped commit.
func (h *changeHub) Invalidate() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, entry := range h.scopes {
		entry.signalLocked()
	}
}

func (entry *changeScope) signalLocked() {
	for s := range entry.subscribers {
		select {
		case s.changes <- struct{}{}:
		default:
		}
	}
}

func (s *changeSubscription) Close() {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	s.closeLocked()
}

func (s *changeSubscription) closeLocked() {
	if s.closed {
		return
	}
	s.closed = true
	if s.stop != nil {
		s.stop()
		s.stop = nil
	}
	changed := false
	for scope := range s.scopes {
		changed = s.removeLocked(scope) || changed
	}
	s.scopes = nil
	delete(s.hub.subscriptions, s)
	close(s.done)
	if changed {
		s.hub.updatedLocked()
	}
}

// Close synchronously releases subscriptions and cancels the optional poller.
// revisionPoller.Close additionally joins its worker (the source must honor ctx).
func (h *changeHub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for s := range h.subscriptions {
		s.closeLocked()
	}
	if h.poller != nil {
		h.poller.cancel()
	}
}
