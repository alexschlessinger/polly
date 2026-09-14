package sessions

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// CoordinationViewStore is a trusted-host display extension. It reads one
// stable root identity without acquiring a lease or loading any transcript.
// It is not a model tool and grants no coordination mutation authority.
type CoordinationViewStore interface {
	ReadCoordinationView(context.Context, string) (*CoordinationState, error)
}

func (s *SQLiteStore) ReadCoordinationView(ctx context.Context, rootID string) (*CoordinationState, error) {
	if err := s.ensureOpen(); err != nil {
		return nil, err
	}
	id, err := decodeSessionID(rootID)
	if err != nil {
		return nil, err
	}
	state := &CoordinationState{ActorID: rootID, ParentID: rootID}
	err = s.withRead(ctx, func(conn *sql.Conn) error {
		snap, err := scanSnapshot(ctx, conn, id)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrSessionNotFound
		}
		if err != nil {
			return err
		}
		if len(snap.parentID) > 0 {
			return ErrSessionNotFound
		}
		now := time.Now().UnixNano()
		if snap.ttlNS > 0 && now >= snap.updatedNS && now-snap.updatedNS >= snap.ttlNS {
			var visible bool
			if err := conn.QueryRowContext(ctx, `SELECT (`+swarmPinnedSQL+`) OR EXISTS(SELECT 1 FROM session_leases WHERE session_id=? AND expires_ns>?) FROM sessions WHERE id=?`, id, now, id).Scan(&visible); err != nil {
				return err
			}
			if !visible {
				return ErrSessionNotFound
			}
		}
		state.Records, err = readSwarmRecords(ctx, conn, id)
		return err
	})
	return state, err
}
