// Package runtimescale contains observational test adapters. It does not supply
// native storage capabilities or implement a mixed-workload stress runner.
package runtimescale

import (
	"context"

	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

// LegacyStore deliberately exposes only SessionStore, hiding any native optional
// fast paths on the supplied store. All operations still use that exact backend.
// Counts describe API materialization, not SQL work, allocations, or decoding.
// Observation is enabled only by a runtimemetrics recorder on the call context.
type LegacyStore struct{ sessions.SessionStore }

func CountLegacyStore(store sessions.SessionStore) *LegacyStore {
	return &LegacyStore{SessionStore: store}
}

func (s *LegacyStore) Acquire(ctx context.Context, name string, opts sessions.AcquireOptions) (sessions.Session, error) {
	handle, err := s.SessionStore.Acquire(ctx, name, opts)
	if err != nil {
		return nil, err
	}
	return CountLegacySession(handle), nil
}

func (s *LegacyStore) ListSummaries(ctx context.Context) ([]sessions.SessionSummary, error) {
	runtimemetrics.Add(ctx, runtimemetrics.SummaryReads, 1)
	rows, err := s.SessionStore.ListSummaries(ctx)
	if err == nil {
		runtimemetrics.Add(ctx, runtimemetrics.SummariesRead, uint64(len(rows)))
	}
	return rows, err
}

type legacySession struct{ sessions.Session }

type legacyCoordinationSession struct {
	*legacySession
	sessions.CoordinationSession
}

// CountLegacySession hides future optional record/page capabilities, while
// retaining CoordinationSession iff the supplied handle implements it. It does
// not discover a native path or replace the host's persistence implementation.
func CountLegacySession(session sessions.Session) sessions.Session {
	s := &legacySession{Session: session}
	if coordination, ok := session.(sessions.CoordinationSession); ok {
		return &legacyCoordinationSession{legacySession: s, CoordinationSession: coordination}
	}
	return s
}

func (s *legacySession) GetHistory(ctx context.Context) ([]messages.ChatMessage, error) {
	runtimemetrics.Add(ctx, runtimemetrics.TranscriptReads, 1)
	history, err := s.Session.GetHistory(ctx)
	if err == nil {
		runtimemetrics.Add(ctx, runtimemetrics.TranscriptMessagesRead, uint64(len(history)))
	}
	return history, err
}

func (s *legacyCoordinationSession) ReadCoordination(ctx context.Context) (*sessions.CoordinationState, error) {
	defer runtimemetrics.Start(ctx, runtimemetrics.CoordinationRead).Stop()
	runtimemetrics.Add(ctx, runtimemetrics.CoordinationReads, 1)
	state, err := s.CoordinationSession.ReadCoordination(ctx)
	if err == nil {
		countState(ctx, state)
	}
	return state, err
}

func (s *legacyCoordinationSession) UpdateCoordination(ctx context.Context, fn func(*sessions.CoordinationState) error) error {
	defer runtimemetrics.Start(ctx, runtimemetrics.CoordinationUpdate).Stop()
	runtimemetrics.Add(ctx, runtimemetrics.CoordinationUpdates, 1)
	// Forward nil unchanged; wrapping it would change the backend's validation.
	if fn == nil {
		return s.CoordinationSession.UpdateCoordination(ctx, nil)
	}
	return s.CoordinationSession.UpdateCoordination(ctx, func(state *sessions.CoordinationState) error {
		countState(ctx, state)
		return fn(state)
	})
}

func countState(ctx context.Context, state *sessions.CoordinationState) {
	if state == nil || runtimemetrics.FromContext(ctx) == nil {
		return
	}
	var records, bytes uint64
	for _, group := range state.Records {
		records += uint64(len(group))
		for _, payload := range group {
			bytes += uint64(len(payload))
		}
	}
	runtimemetrics.Add(ctx, runtimemetrics.CoordinationRecordsRead, records)
	runtimemetrics.Add(ctx, runtimemetrics.CoordinationBytesRead, bytes)
}
