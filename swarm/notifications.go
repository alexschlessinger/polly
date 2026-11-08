package swarm

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/alexschlessinger/pollytool/internal/coordindex"
	"github.com/alexschlessinger/pollytool/sessions"
)

const activityIndex sessions.CoordinationIndex = "runtime.activity"

// ActivityScope observes in-memory runtime readiness and display activity. It
// is local to this notification provider, not a durable storage revision.
func ActivityScope() sessions.RevisionScope {
	return sessions.RevisionScope{Index: activityIndex}
}

// ChangeSubscription is a coalesced hint stream with one logical reader. A hint
// carries no payload, delivery acknowledgement, or mutation authority. Changes
// is never closed; Done signals cancellation, Close, or provider shutdown.
//
// Subscribe before reading readiness. Drain an old hint BEFORE each consistent
// read, then Replace the dependencies discovered by that read. If Replace
// reports added scopes, read again before waiting: their registration may have
// raced a commit. Always select on Done and the operation context as well as
// Changes. Neither a hint nor an unchanged token establishes readiness.
// All methods are safe for concurrent use.
type ChangeSubscription struct {
	sub *changeSubscription
}

func (s *ChangeSubscription) Changes() <-chan struct{} { return s.sub.Changes() }
func (s *ChangeSubscription) Done() <-chan struct{}    { return s.sub.Done() }
func (s *ChangeSubscription) Close()                   { s.sub.Close() }

// Replace atomically changes dependencies. Invalid scopes leave the existing
// subscription untouched. True requires another read before waiting. A closed
// subscription returns false; callers must still inspect Done.
func (s *ChangeSubscription) Replace(scopes []sessions.RevisionScope) (bool, error) {
	normalized, err := notificationScopes(scopes)
	if err != nil {
		return false, err
	}
	return s.sub.Replace(normalized), nil
}

// Notifications owns one hub and revision poller for one root and one supplied
// store capability. It retains no readers, payloads, leases, or revision history
// for inactive scopes. Close must be called when the provider is no longer used;
// cancelling its owner context also stops it. The zero value is not usable.
type Notifications struct {
	hub    *changeHub
	poller *revisionPoller
	source *notificationSource
}

// NewViewNotifications creates a trusted-host, root-only, lease-free observer.
// It uses only coordination view capabilities on store: no session acquisition,
// store discovery, preparation, repair, or payload reads occur here. A store
// without durable revisions conservatively hints on each active poll instead
// of materializing whole-family state merely to check whether it changed. If a
// durable host rejects a revision selector as unsupported, only that requested
// batch falls back to conservative hints; other batches retain exact revisions.
func NewViewNotifications(ctx context.Context, store any, root string) (*Notifications, error) {
	if root == "" {
		return nil, fmt.Errorf("%w: notification root required", sessions.ErrCoordinationRequest)
	}
	access, err := sessions.OpenCoordinationViewAccess(store)
	if err != nil {
		return nil, err
	}
	source := &notificationSource{
		root: root, durable: access.Capabilities().DurableRevisions,
		read: func(ctx context.Context, fn func(sessions.CoordinationReader) error) error {
			return access.ReadRecordsView(ctx, root, fn)
		},
	}
	return newNotifications(ctx, source, defaultNotificationPolling())
}

func defaultNotificationPolling() revisionPollerConfig {
	return revisionPollerConfig{Interval: 250 * time.Millisecond, MaxBackoff: 4 * time.Second}
}

func newNotifications(ctx context.Context, source *notificationSource, config revisionPollerConfig) (*Notifications, error) {
	if ctx == nil || source == nil || source.root == "" {
		return nil, fmt.Errorf("%w: notification context, source and root required", sessions.ErrCoordinationRequest)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	hub := newChangeHub()
	poller, err := newRevisionPoller(ctx, hub, source, config)
	if err != nil {
		hub.Close()
		return nil, err
	}
	return &Notifications{hub: hub, poller: poller, source: source}, nil
}

// Subscribe registers exact normalized scopes. An empty set is idle and creates
// no polling timer. Invalid selectors create no subscription. A closed provider
// returns a subscription whose Done is already closed.
func (n *Notifications) Subscribe(ctx context.Context, scopes []sessions.RevisionScope) (*ChangeSubscription, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: subscription context required", sessions.ErrCoordinationRequest)
	}
	normalized, err := notificationScopes(scopes)
	if err != nil {
		return nil, err
	}
	return &ChangeSubscription{sub: n.hub.Subscribe(ctx, normalized)}, nil
}

// Close cancels in-flight reads, closes every subscription, and joins the sole
// poller worker. Repeated and concurrent calls are safe. Supplied read callbacks
// must honor their context; shutdown never abandons an unjoined source worker.
func (n *Notifications) Close() { n.poller.Close() }

// Subscribe lazily shares one notification provider for this runtime. Runtime
// ownership, rather than the first subscriber's context, controls its lifetime.
func (r *Runtime) Subscribe(ctx context.Context, scopes []sessions.RevisionScope) (*ChangeSubscription, error) {
	// Reject invalid input before allocating an owned worker.
	if ctx == nil {
		return nil, fmt.Errorf("%w: subscription context required", sessions.ErrCoordinationRequest)
	}
	if _, err := notificationScopes(scopes); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.notifications == nil {
		access, err := sessions.OpenCoordinationAccess(r.parent)
		if err != nil {
			return nil, err
		}
		root := r.ID
		if root == "" {
			root = r.parent.ViewID()
		}
		owner := r.ctx
		if owner == nil {
			owner = context.Background()
		}
		source := &notificationSource{root: root, durable: access.Capabilities().DurableRevisions, read: access.ReadRecords}
		n, err := newNotifications(owner, source, defaultNotificationPolling())
		if err != nil {
			return nil, err
		}
		r.notifications = n
	}
	return r.notifications.Subscribe(ctx, scopes)
}

// publishChanges must receive only the changed scopes of a successful commit.
// Writes with no observers never allocate a provider, and an empty receipt is
// not a hint. Publishers must not hold r.mu.
func (r *Runtime) publishChanges(scopes []sessions.RevisionScope) {
	r.mu.Lock()
	n := r.notifications
	r.mu.Unlock()
	if n != nil && len(scopes) != 0 {
		if n.source.durable {
			n.hub.Publish(scopes)
		} else {
			// Legacy receipts may omit derived scopes that their backend does
			// not maintain. They cannot establish exact unchanged detection.
			n.hub.Invalidate()
		}
	}
}

// publishActivity is separate from committed storage changes: a readiness or UI
// transition must not wake unrelated durable-scope subscribers.
func (r *Runtime) publishActivity() {
	r.mu.Lock()
	n := r.notifications
	r.mu.Unlock()
	if n != nil {
		n.source.activity.Add(1)
		n.hub.Publish([]sessions.RevisionScope{ActivityScope()})
	}
}

// invalidateChanges is a conservative hint for legacy/uncertain writes, never
// evidence of a successful commit. It does not create a provider or a wildcard
// scope, and does not advance the poller's durable baseline.
func (r *Runtime) invalidateChanges() {
	r.mu.Lock()
	n := r.notifications
	r.mu.Unlock()
	if n != nil {
		n.hub.Invalidate()
	}
}

func notificationScopes(scopes []sessions.RevisionScope) ([]sessions.RevisionScope, error) {
	normalized := make([]sessions.RevisionScope, 0, len(scopes))
	seen := make(map[sessions.RevisionScope]struct{}, len(scopes))
	for _, scope := range scopes {
		if scope.Index == activityIndex {
			if scope.Key != "" {
				return nil, fmt.Errorf("%w: activity takes no key", sessions.ErrCoordinationRequest)
			}
		} else {
			index, key, err := coordindex.Normalize(coordindex.Index(scope.Index), scope.Key)
			if err != nil {
				return nil, fmt.Errorf("%w: %w", sessions.ErrCoordinationRequest, err)
			}
			scope = sessions.RevisionScope{Index: sessions.CoordinationIndex(index), Key: key}
		}
		if _, exists := seen[scope]; !exists {
			seen[scope] = struct{}{}
			normalized = append(normalized, scope)
		}
	}
	return normalized, nil
}

// notificationSource reads only revision tokens from the supplied capability.
// The callback and reader never escape an operation. Prefixing stored tokens
// keeps a missing root distinct even from an arbitrary host's opaque tokens.
type notificationSource struct {
	root     string
	durable  bool
	read     func(context.Context, func(sessions.CoordinationReader) error) error
	activity atomic.Uint64
	fallback atomic.Uint64
}

func (s *notificationSource) Revisions(ctx context.Context, scopes []sessions.RevisionScope) ([]sessions.Revision, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(scopes) > sessions.MaxCoordinationRevisionScopes {
		return nil, fmt.Errorf("%w: too many notification scopes", sessions.ErrCoordinationRequest)
	}
	var durable []sessions.RevisionScope
	var out []sessions.Revision
	for _, scope := range scopes {
		if scope == ActivityScope() {
			out = append(out, sessions.Revision{Scope: scope, Token: "activity:" + strconv.FormatUint(s.activity.Load(), 10)})
		} else {
			durable = append(durable, scope)
		}
	}
	if len(durable) == 0 {
		return out, nil
	}
	if !s.durable {
		return s.conservative(out, durable), nil
	}
	var revisions []sessions.Revision
	var callbackErr error
	called, unsupported := false, false
	err := s.read(ctx, func(raw sessions.CoordinationReader) error {
		if called {
			callbackErr = fmt.Errorf("%w: backend retried notification callback", sessions.ErrCoordinationRequest)
			return callbackErr
		}
		called = true
		callbackErr = func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if raw == nil {
				return fmt.Errorf("%w: backend supplied nil notification reader", sessions.ErrCoordinationRequest)
			}
			identity, err := raw.Identity()
			if err != nil {
				return err
			}
			if identity.ActorID != s.root || identity.ParentID != s.root {
				return fmt.Errorf("%w: notification reader is not root %q", sessions.ErrCoordinationRequest, s.root)
			}
			revisions, err = raw.Revisions(durable)
			if err != nil {
				unsupported = errors.Is(err, sessions.ErrCoordinationSelector) || errors.Is(err, sessions.ErrCoordinationUnsupported)
				return err
			}
			revisions = append([]sessions.Revision(nil), revisions...)
			return ctx.Err()
		}()
		return callbackErr
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if callbackErr != nil {
		err = callbackErr
	}
	if unsupported && (errors.Is(err, sessions.ErrCoordinationSelector) || errors.Is(err, sessions.ErrCoordinationUnsupported)) {
		// Older durable hosts may not implement a newly added revision-only
		// selector. Hint this batch without payload reads, but do not weaken
		// identity/callback validation or treat transient failures as success.
		return s.conservative(out, durable), nil
	}
	if errors.Is(err, sessions.ErrSessionNotFound) {
		for _, scope := range durable {
			out = append(out, sessions.Revision{Scope: scope, Token: "missing"})
		}
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	if !called {
		return nil, fmt.Errorf("%w: backend omitted notification callback", sessions.ErrCoordinationRequest)
	}
	for _, revision := range revisions {
		revision.Token = "present:" + revision.Token
		out = append(out, revision)
	}
	return out, nil
}

func (s *notificationSource) conservative(out []sessions.Revision, scopes []sessions.RevisionScope) []sessions.Revision {
	token := "fallback:" + strconv.FormatUint(s.fallback.Add(1), 10)
	for _, scope := range scopes {
		out = append(out, sessions.Revision{Scope: scope, Token: token})
	}
	return out
}
