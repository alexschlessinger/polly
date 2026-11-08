package swarm

import (
	"context"
	"fmt"
	"time"

	"github.com/alexschlessinger/pollytool/sessions"
)

// revisionSource reads one consistent revision vector per call. It must return
// exactly one revision for each requested scope (order is immaterial), and honor
// cancellation. Tokens are opaque equality hints, never commit/lease authority.
// A source without durable revisions must conservatively invalidate, not claim
// durable unchanged detection. No reader or store callback is retained here.
type revisionSource interface {
	Revisions(context.Context, []sessions.RevisionScope) ([]sessions.Revision, error)
}

// Timing is explicitly supplied by the owner; this component chooses no runtime
// polling policy. BatchSize zero uses the contract bound. A smaller positive
// size supports a source with a lower parameter budget.
type revisionPollerConfig struct {
	Interval, MaxBackoff time.Duration
	BatchSize            int
	clock                revisionPollClock
}

type revisionPollClock interface {
	NewTimer(time.Duration) revisionPollTimer
}

type revisionPollTimer interface {
	C() <-chan time.Time
	Stop()
}

type realRevisionPollClock struct{}
type realRevisionPollTimer struct{ *time.Timer }

func (realRevisionPollClock) NewTimer(d time.Duration) revisionPollTimer {
	return realRevisionPollTimer{time.NewTimer(d)}
}
func (t realRevisionPollTimer) C() <-chan time.Time { return t.Timer.C }
func (t realRevisionPollTimer) Stop()               { t.Timer.Stop() }

// revisionPoller owns exactly one worker and at most one timer for the hub.
// There are no per-waiter tickers, durable payloads or historical revision maps.
// First observation also hints: a commit may have happened after a subscriber's
// read but before the first poll. Merely establishing that baseline cannot hide
// the commit. Errors never advance a baseline or acknowledge delivery.
type revisionPoller struct {
	hub    *changeHub
	source revisionSource
	config revisionPollerConfig
	cancel context.CancelFunc
	done   chan struct{}
}

func newRevisionPoller(ctx context.Context, hub *changeHub, source revisionSource, config revisionPollerConfig) (*revisionPoller, error) {
	if hub == nil || source == nil {
		return nil, fmt.Errorf("revision poller requires a hub and source")
	}
	if config.Interval <= 0 || config.MaxBackoff < config.Interval {
		return nil, fmt.Errorf("revision poller requires a positive interval and max backoff >= interval")
	}
	if config.BatchSize == 0 {
		config.BatchSize = sessions.MaxCoordinationRevisionScopes
	}
	if config.BatchSize < 1 || config.BatchSize > sessions.MaxCoordinationRevisionScopes {
		return nil, fmt.Errorf("revision poll batch size must be between 1 and %d", sessions.MaxCoordinationRevisionScopes)
	}
	if config.clock == nil {
		config.clock = realRevisionPollClock{}
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if hub.closed || hub.poller != nil {
		return nil, fmt.Errorf("revision hub is closed or already has a poller")
	}
	ctx, cancel := context.WithCancel(ctx)
	p := &revisionPoller{hub: hub, source: source, config: config, cancel: cancel, done: make(chan struct{})}
	hub.poller = p
	go p.run(ctx)
	return p, nil
}

// Close shuts down this hub's subscriptions, cancels in-flight source work and
// joins the worker. Repeated/concurrent Close calls are safe. The source must
// honor context cancellation; Close does not abandon unjoined source goroutines.
func (p *revisionPoller) Close() {
	p.hub.Close()
	<-p.done
}

type revisionPollScope struct {
	scope sessions.RevisionScope
	entry *changeScope
}

func (h *changeHub) pollScopes() []revisionPollScope {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]revisionPollScope, 0, len(h.scopes))
	for scope, entry := range h.scopes {
		out = append(out, revisionPollScope{scope, entry})
	}
	return out
}

func (p *revisionPoller) run(ctx context.Context) {
	defer close(p.done)
	defer p.hub.Close()
	delay := p.config.Interval
	for ctx.Err() == nil {
		scopes := p.hub.pollScopes()
		if len(scopes) == 0 {
			delay = p.config.Interval
			select {
			case <-ctx.Done():
				return
			case <-p.hub.updated:
				continue
			}
		}
		if err := p.poll(ctx, scopes); err != nil {
			// Saturating doubling avoids duration overflow.
			if delay > p.config.MaxBackoff/2 {
				delay = p.config.MaxBackoff
			} else {
				delay *= 2
			}
		} else {
			delay = p.config.Interval
		}
		if !p.wait(ctx, delay) {
			return
		}
	}
}

func (p *revisionPoller) wait(ctx context.Context, delay time.Duration) bool {
	timer := p.config.clock.NewTimer(delay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-timer.C():
			return true
		case <-p.hub.updated:
			p.hub.mu.Lock()
			empty := len(p.hub.scopes) == 0
			p.hub.mu.Unlock()
			if empty {
				return true // idle has no timer and retains no poll snapshot
			}
			// Membership churn coalesces without resetting the interval or
			// turning many subscribe calls into many source transactions.
		}
	}
}

func (p *revisionPoller) poll(ctx context.Context, scopes []revisionPollScope) error {
	var firstErr error
	for start := 0; start < len(scopes); start += p.config.BatchSize {
		if err := ctx.Err(); err != nil {
			return err
		}
		batch := scopes[start:min(start+p.config.BatchSize, len(scopes))]
		keys := make([]sessions.RevisionScope, len(batch))
		for i, item := range batch {
			keys[i] = item.scope
		}
		revisions, err := p.source.Revisions(ctx, keys)
		if err == nil {
			err = p.apply(batch, revisions)
		}
		if firstErr == nil {
			firstErr = err
		}
		// An unavailable partition must not starve subsequent active batches.
	}
	return firstErr
}

func (p *revisionPoller) apply(batch []revisionPollScope, revisions []sessions.Revision) error {
	if len(revisions) != len(batch) {
		return fmt.Errorf("revision source returned %d entries for %d scopes", len(revisions), len(batch))
	}
	tokens := make(map[sessions.RevisionScope]string, len(revisions))
	for _, revision := range revisions {
		if _, duplicate := tokens[revision.Scope]; duplicate {
			return fmt.Errorf("revision source returned a duplicate scope")
		}
		tokens[revision.Scope] = revision.Token
	}
	for _, item := range batch {
		if _, present := tokens[item.scope]; !present {
			return fmt.Errorf("revision source omitted a requested scope")
		}
	}
	p.hub.mu.Lock()
	defer p.hub.mu.Unlock()
	for _, item := range batch {
		entry := p.hub.scopes[item.scope]
		if entry != item.entry {
			continue // dropped/recreated during the read; do not retain history
		}
		token := tokens[item.scope]
		if !entry.known || token != entry.token {
			entry.known, entry.token = true, token
			entry.signalLocked()
		}
	}
	return nil
}
