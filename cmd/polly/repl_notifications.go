package main

import (
	"context"
	"fmt"
	"reflect"

	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/swarm"
)

// A tab shares one notification source across its overview, selected inspector,
// and context popover. Runtime sources are borrowed; only saved-view managers
// are owned here. Names and replaceable conversationState wrappers are not IDs.
// All binding/watch bookkeeping belongs to the event loop, never read workers.
type replNotificationSource struct {
	root    string
	store   sessions.SessionStore
	runtime *swarm.Runtime
	saved   *swarm.Notifications
	watches map[*replChangeWatch]struct{}
	closed  bool
}

func (s *replNotificationSource) matches(tab *replTab) bool {
	if s == nil || s.closed || tab.state == nil {
		return false
	}
	store := tab.state.sessionStore
	// Native stores and the usual host wrappers are comparable pointers.
	sameStore := s.store == nil && store == nil || s.store != nil && store != nil && reflect.TypeOf(store).Comparable() && s.store == store
	return s.root == tab.viewID() && s.runtime == tab.state.swarm && sameStore
}

func (tab *replTab) notificationSource(ctx context.Context) (*replNotificationSource, error) {
	if tab.notifications.matches(tab) {
		return tab.notifications, nil
	}
	tab.notifications.close()
	tab.notifications = nil
	if tab.state == nil || tab.viewID() == "" {
		return nil, fmt.Errorf("swarm notification identity unavailable")
	}
	if store := tab.state.sessionStore; store != nil && !reflect.TypeOf(store).Comparable() {
		// A value backend containing maps/functions has no comparable identity.
		// Keep the old throttled reads rather than alias a replacement backend,
		// discard every result, or create and tear down managers every frame.
		return nil, nil
	}
	s := &replNotificationSource{root: tab.viewID(), store: tab.state.sessionStore, runtime: tab.state.swarm, watches: make(map[*replChangeWatch]struct{})}
	if s.runtime == nil {
		var err error
		s.saved, err = swarm.NewViewNotifications(ctx, s.store, s.root)
		if err != nil {
			return nil, err
		}
	}
	tab.notifications = s
	return s, nil
}

func (s *replNotificationSource) close() {
	if s == nil || s.closed {
		return
	}
	s.closed = true
	for watch := range s.watches {
		watch.close()
	}
	if s.saved != nil {
		s.saved.Close()
	}
}

// The interface also permits deterministic tests without timers or a polling
// backend. Production subscriptions coalesce into one hint and never close
// Changes; Done, rather than a readable closed Changes channel, ends a watch.
type replChanges interface {
	Changes() <-chan struct{}
	Done() <-chan struct{}
	Close()
}

type replChangeWatch struct {
	source  *replNotificationSource
	sub     replChanges
	initial bool
}

func (w *replChangeWatch) bind(ctx context.Context, source *replNotificationSource, scopes []sessions.RevisionScope) error {
	if w.source == source && w.sub != nil && !w.stopped() {
		return nil
	}
	w.close()
	if source == nil {
		return nil // No stable source identity: retain conservative reads.
	}
	if source.closed {
		return fmt.Errorf("swarm notifications unavailable")
	}
	var sub *swarm.ChangeSubscription
	var err error
	if source.runtime != nil {
		sub, err = source.runtime.Subscribe(ctx, scopes)
	} else {
		sub, err = source.saved.Subscribe(ctx, scopes)
	}
	if err != nil {
		return err
	}
	w.source, w.sub, w.initial = source, sub, true
	source.watches[w] = struct{}{}
	return nil
}

func (w *replChangeWatch) stopped() bool {
	if w.sub != nil {
		select {
		case <-w.sub.Done():
			return true
		default:
		}
	}
	return false
}

func (w *replChangeWatch) pending() bool {
	return w.sub == nil || w.initial || w.stopped() || len(w.sub.Changes()) > 0
}

// Drain BEFORE dispatching the read, never when its result lands. A commit
// while I/O is in flight must remain dirty for the next read. A failed or absent
// subscription falls back to the existing throttled reads, never stale data.
func (w *replChangeWatch) take(force bool) bool {
	dirty := force || w.initial || w.sub == nil || w.stopped()
	w.initial = false
	if w.sub != nil {
		// One receive clears the coalesced old hint. Do not chase producers
		// indefinitely on the event loop when commits arrive continuously.
		select {
		case <-w.sub.Changes():
			dirty = true
		default:
		}
	}
	return dirty
}

func (w *replChangeWatch) close() {
	if w.sub != nil {
		w.sub.Close()
	}
	if w.source != nil {
		delete(w.source.watches, w)
	}
	*w = replChangeWatch{}
}

func (tab *replTab) closeNotifications() {
	tab.notifications.close()
	tab.notifications = nil
	if tab.workspace != nil && tab.workspace.inspector.current != nil {
		tab.workspace.inspector.current.changes.close()
	}
	if tab.model != nil && tab.model.modal != nil && tab.model.modal.context != nil {
		tab.model.modal.context.close()
	}
}

func swarmOverviewScopes() []sessions.RevisionScope {
	return []sessions.RevisionScope{{Index: sessions.CoordinationIndexDisplay}, swarm.ActivityScope()}
}

func swarmInspectorScopes() []sessions.RevisionScope {
	// Inspector pages currently use the full records revision, even when the
	// selected page is bounded. Do not pretend it has narrower dependencies.
	return []sessions.RevisionScope{{Index: sessions.CoordinationIndexRecords}, swarm.ActivityScope()}
}

func swarmUsageScopes() []sessions.RevisionScope {
	return []sessions.RevisionScope{
		{Index: sessions.CoordinationIndexKind, Key: "execution"},
		{Index: sessions.CoordinationIndexKind, Key: "member"},
		{Index: sessions.CoordinationIndexKind, Key: "task"},
	}
}
