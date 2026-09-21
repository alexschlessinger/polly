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
		var parentID []byte
		var updatedNS, ttlNS int64
		err := conn.QueryRowContext(ctx, "SELECT parent_id, updated_ns, ttl_ns FROM sessions WHERE id = ?", id).Scan(&parentID, &updatedNS, &ttlNS)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrSessionNotFound
		}
		if err != nil {
			return err
		}
		if len(parentID) > 0 {
			return ErrSessionNotFound
		}
		if _, err := visibleSession(ctx, conn, id, updatedNS, ttlNS, time.Now().UnixNano()); err != nil {
			return err
		}
		state.Records, err = readSwarmRecords(ctx, conn, id)
		return err
	})
	return state, err
}
