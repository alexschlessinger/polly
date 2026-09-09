package sessions

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
)

// CoordinationSession is optional. Bare Session and SessionStore clients do
// not need to construct a swarm. Updates are fenced by the caller's lease.
type CoordinationSession interface {
	ViewIdentity
	ReadCoordination(context.Context) (*CoordinationState, error)
	UpdateCoordination(context.Context, func(*CoordinationState) error) error
	OpenPublishedArtifact(context.Context, string) (io.ReadCloser, error)
}

// CoordinationState is a transaction-local copy. Runtime records are grouped
// by domain (run, task, member, mail, publication, workflow, execution). The
// store owns identity, atomicity and retention; the runtime owns transitions.
// Callbacks must not call back into the store or retain this value.
type CoordinationState struct {
	ActorID, ParentID string
	Records           map[string]map[string]json.RawMessage
	// Append and ExpectedSequence atomically checkpoint generated output and
	// admitted mail with the records containing its delivery receipts.
	Append           []messages.ChatMessage
	ExpectedSequence *int64
	Sequence         int64
	// Pins name artifacts the actor already owns. Publication pins survive
	// retirement of the actor and are collected with the parent.
	Pins []string
	// PinOwner may be a direct child when the parent records that child's
	// publication. A member can only pin its own bytes.
	PinOwner string
	// Members pins child conversations until the parent is removed.
	Members []string
}

func applySchemaV5(ctx context.Context, conn *sql.Conn) error {
	for _, stmt := range []string{
		`CREATE TABLE swarm_records (
		 parent_id BLOB NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
		 kind TEXT NOT NULL, id TEXT NOT NULL, payload_json BLOB NOT NULL,
		 PRIMARY KEY(parent_id,kind,id)) STRICT`,
		`CREATE TABLE swarm_artifacts (
		 parent_id BLOB NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
		 digest BLOB NOT NULL REFERENCES artifact_blobs(digest) ON DELETE CASCADE,
		 PRIMARY KEY(parent_id,digest)) STRICT`,
		`CREATE TABLE swarm_members (
		 parent_id BLOB NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
		 member_id BLOB NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
		 PRIMARY KEY(parent_id,member_id)) STRICT`,
		`PRAGMA user_version = 5`,
	} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migrate coordination: %w", err)
		}
	}
	return nil
}

var schemaV5Tables = map[string]schemaTableSpec{
	"swarm_records":   {columns: []schemaColumnSpec{{"parent_id", "BLOB", 1, 1, ""}, {"kind", "TEXT", 1, 2, ""}, {"id", "TEXT", 1, 3, ""}, {"payload_json", "BLOB", 1, 0, ""}}},
	"swarm_artifacts": {columns: []schemaColumnSpec{{"parent_id", "BLOB", 1, 1, ""}, {"digest", "BLOB", 1, 2, ""}}},
	"swarm_members":   {columns: []schemaColumnSpec{{"parent_id", "BLOB", 1, 1, ""}, {"member_id", "BLOB", 1, 2, ""}}},
}

func (s *sqliteSession) loadCoordination(ctx context.Context, conn *sql.Conn) (*CoordinationState, []byte, error) {
	if err := s.requireLease(ctx, conn); err != nil {
		return nil, nil, err
	}
	var parent []byte
	var sequence int64
	if err := conn.QueryRowContext(ctx, `SELECT coalesce(parent_id,id),next_sequence FROM sessions WHERE id=?`, s.id).Scan(&parent, &sequence); err != nil {
		return nil, nil, err
	}
	state := &CoordinationState{ActorID: s.ViewID(), ParentID: hex.EncodeToString(parent), Records: map[string]map[string]json.RawMessage{}, Sequence: sequence}
	rows, err := conn.QueryContext(ctx, `SELECT kind,id,payload_json FROM swarm_records WHERE parent_id=? ORDER BY kind,id`, parent)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, id string
		var value []byte
		if err := rows.Scan(&kind, &id, &value); err != nil {
			return nil, nil, err
		}
		if state.Records[kind] == nil {
			state.Records[kind] = map[string]json.RawMessage{}
		}
		state.Records[kind][id] = value
	}
	return state, parent, rows.Err()
}

func (s *sqliteSession) ReadCoordination(ctx context.Context) (*CoordinationState, error) {
	opCtx, cleanup, err := s.operationContext(ctx)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	var state *CoordinationState
	err = s.store.withRead(opCtx, func(conn *sql.Conn) error { var e error; state, _, e = s.loadCoordination(opCtx, conn); return e })
	return state, s.mapError(ctx, err)
}

func (s *sqliteSession) UpdateCoordination(ctx context.Context, fn func(*CoordinationState) error) error {
	if fn == nil {
		return errors.New("coordination update is required")
	}
	opCtx, cleanup, err := s.operationContext(ctx)
	if err != nil {
		return err
	}
	defer cleanup()
	err = s.store.withWrite(opCtx, func(conn *sql.Conn) error {
		state, parent, err := s.loadCoordination(opCtx, conn)
		if err != nil {
			return err
		}
		before := cloneRecords(state.Records)
		if err := fn(state); err != nil {
			return err
		}
		if state.ExpectedSequence != nil && *state.ExpectedSequence != state.Sequence {
			return errors.New("checkpoint sequence changed")
		}
		// Only records the callback changed are written. A checkpoint touches
		// one execution and a few messages, never the family's whole record set.
		for kind, group := range before {
			for id := range group {
				if state.Records[kind][id] == nil {
					if _, err := conn.ExecContext(opCtx, `DELETE FROM swarm_records WHERE parent_id=? AND kind=? AND id=?`, parent, kind, id); err != nil {
						return err
					}
				}
			}
		}
		for kind, group := range state.Records {
			for id, value := range group {
				if !json.Valid(value) || kind == "" || id == "" {
					return errors.New("invalid coordination record")
				}
				if previous, ok := before[kind][id]; ok && bytes.Equal(previous, value) {
					continue
				}
				if _, err := conn.ExecContext(opCtx, `INSERT OR REPLACE INTO swarm_records VALUES(?,?,?,?)`, parent, kind, id, []byte(value)); err != nil {
					return err
				}
			}
		}
		for _, id := range state.Members {
			if state.ActorID != state.ParentID {
				return errors.New("only the parent can register members")
			}
			member, err := hex.DecodeString(id)
			if err != nil {
				return err
			}
			var valid bool
			if err := conn.QueryRowContext(opCtx, `SELECT EXISTS(SELECT 1 FROM sessions WHERE id=? AND parent_id=?)`, member, parent).Scan(&valid); err != nil {
				return err
			}
			if !valid {
				return errors.New("member is not a direct child")
			}
			if _, err := conn.ExecContext(opCtx, `INSERT OR IGNORE INTO swarm_members VALUES(?,?)`, parent, member); err != nil {
				return err
			}
		}
		for _, id := range state.Pins {
			digest, err := artifactDigest(id)
			if err != nil {
				return err
			}
			owner := s.id
			if state.PinOwner != "" && state.PinOwner != state.ActorID {
				if state.ActorID != state.ParentID {
					return errors.New("members may publish only their own artifacts")
				}
				owner, err = hex.DecodeString(state.PinOwner)
				if err != nil {
					return err
				}
				var child bool
				if err = conn.QueryRowContext(opCtx, `SELECT EXISTS(SELECT 1 FROM sessions WHERE id=? AND parent_id=?)`, owner, parent).Scan(&child); err != nil {
					return err
				}
				if !child {
					return errors.New("artifact owner is not a direct child")
				}
			}
			var owns bool
			if err := conn.QueryRowContext(opCtx, `SELECT EXISTS(SELECT 1 FROM session_artifacts WHERE session_id=? AND digest=?)`, owner, digest).Scan(&owns); err != nil {
				return err
			}
			if !owns {
				return errors.New("cannot publish an artifact the caller does not own")
			}
			if _, err := conn.ExecContext(opCtx, `INSERT OR IGNORE INTO swarm_artifacts VALUES(?,?)`, parent, digest); err != nil {
				return err
			}
		}
		for i, msg := range state.Append {
			payload, err := json.Marshal(msg)
			if err != nil {
				return err
			}
			if _, err := conn.ExecContext(opCtx, `INSERT INTO messages(session_id,sequence,payload_json) VALUES(?,?,?)`, s.id, state.Sequence+int64(i), payload); err != nil {
				return err
			}
		}
		if len(state.Append) > 0 {
			_, err := conn.ExecContext(opCtx, `UPDATE sessions SET next_sequence=?,updated_ns=max(updated_ns+1,?),has_turn=1 WHERE id=?`, state.Sequence+int64(len(state.Append)), time.Now().UnixNano(), s.id)
			return err
		}
		return nil
	})
	return s.mapError(ctx, err)
}

func cloneRecords(records map[string]map[string]json.RawMessage) map[string]map[string]json.RawMessage {
	out := make(map[string]map[string]json.RawMessage, len(records))
	for kind, group := range records {
		copied := make(map[string]json.RawMessage, len(group))
		for id, value := range group {
			copied[id] = value
		}
		out[kind] = copied
	}
	return out
}

// OpenPublishedArtifact grants the caller a private reference only after the
// family publication pin is checked atomically. Arbitrary private blobs remain
// inaccessible even if a member guesses their content digest.
func (s *sqliteSession) OpenPublishedArtifact(ctx context.Context, id string) (io.ReadCloser, error) {
	digest, err := artifactDigest(id)
	if err != nil {
		return nil, err
	}
	err = s.store.withWrite(ctx, func(conn *sql.Conn) error {
		if err := s.requireLease(ctx, conn); err != nil {
			return err
		}
		var published bool
		if err := conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM swarm_artifacts WHERE digest=? AND parent_id=(SELECT coalesce(parent_id,id) FROM sessions WHERE id=?))`, digest, s.id).Scan(&published); err != nil {
			return err
		}
		if !published {
			return errors.New("artifact is not published in this swarm")
		}
		_, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO session_artifacts(session_id,digest) VALUES(?,?)`, s.id, digest)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.ArtifactStore().Open(ctx, id)
}
