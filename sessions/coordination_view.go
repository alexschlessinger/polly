package sessions

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
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
	id, err := hex.DecodeString(rootID)
	if err != nil || len(id) != 16 {
		return nil, ErrSessionNotFound
	}
	state := &CoordinationState{ActorID: rootID, ParentID: rootID, Records: map[string]map[string]json.RawMessage{}}
	err = s.withRead(ctx, func(conn *sql.Conn) error {
		snap, _, err := scanSnapshot(ctx, conn, id)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrSessionNotFound
		}
		if err != nil {
			return err
		}
		var parent []byte
		if err := conn.QueryRowContext(ctx, "SELECT parent_id FROM sessions WHERE id=?", id).Scan(&parent); err != nil {
			return err
		}
		if len(parent) > 0 {
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
		rows, err := conn.QueryContext(ctx, "SELECT kind,id,payload_json FROM swarm_records WHERE parent_id=? ORDER BY kind,id", id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var kind, key string
			var value []byte
			if err := rows.Scan(&kind, &key, &value); err != nil {
				return err
			}
			if state.Records[kind] == nil {
				state.Records[kind] = map[string]json.RawMessage{}
			}
			state.Records[kind][key] = value
		}
		return rows.Err()
	})
	return state, err
}
