package sessions

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
)

// CoordinationSession is optional. Bare Session and SessionStore clients do
// not need to construct a swarm. Updates are fenced by the caller's lease.
type CoordinationSession interface {
	ViewIdentity
	ReadCoordination(context.Context) (*CoordinationState, error)
	UpdateCoordination(context.Context, func(*CoordinationState) error) error
	OpenPublishedArtifact(context.Context, string) (artifacts.Ref, io.ReadCloser, error)
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
	return execAll(ctx, conn, "migrate coordination",
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
	)
}

// loadCoordination reads the family state for a session whose lease the
// caller has already confirmed.
func (s *sqliteSession) loadCoordination(ctx context.Context, conn *sql.Conn) (*CoordinationState, []byte, error) {
	var parent []byte
	var sequence int64
	err := conn.QueryRowContext(ctx, `SELECT coalesce(parent_id,id),next_sequence FROM sessions WHERE id=?`, s.id).Scan(&parent, &sequence)
	if err != nil {
		return nil, nil, err
	}
	state := &CoordinationState{ActorID: s.ViewID(), ParentID: hex.EncodeToString(parent), Sequence: sequence}
	state.Records, err = readSwarmRecords(ctx, conn, parent)
	return state, parent, err
}

// readSwarmRecords loads the family's records grouped by kind, then id.
func readSwarmRecords(ctx context.Context, conn *sql.Conn, parent []byte) (map[string]map[string]json.RawMessage, error) {
	rows, err := conn.QueryContext(ctx, `SELECT kind,id,payload_json FROM swarm_records WHERE parent_id=? ORDER BY kind,id`, parent)
	if err != nil {
		return nil, err
	}
	records := map[string]map[string]json.RawMessage{}
	err = eachRow(rows, func() error {
		var kind, id string
		var value []byte
		if err := rows.Scan(&kind, &id, &value); err != nil {
			return err
		}
		if records[kind] == nil {
			records[kind] = map[string]json.RawMessage{}
		}
		records[kind][id] = value
		return nil
	})
	return records, err
}

func (s *sqliteSession) ReadCoordination(ctx context.Context) (*CoordinationState, error) {
	var state *CoordinationState
	err := s.leased(ctx, false, func(ctx context.Context, conn *sql.Conn) (err error) {
		state, _, err = s.loadCoordination(ctx, conn)
		return err
	})
	return state, err
}

// requireDirectChild fails unless child is a session spawned by parent.
func requireDirectChild(ctx context.Context, conn rowQuerier, child, parent []byte, what string) error {
	var direct bool
	if err := conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sessions WHERE id=? AND parent_id=?)`, child, parent).Scan(&direct); err != nil {
		return err
	}
	if !direct {
		return errors.New(what + " is not a direct child")
	}
	return nil
}

func (s *sqliteSession) UpdateCoordination(ctx context.Context, fn func(*CoordinationState) error) error {
	if fn == nil {
		return errors.New("coordination update is required")
	}
	return s.leased(ctx, true, func(opCtx context.Context, conn *sql.Conn) error {
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
		if len(state.Members) > 0 && state.ActorID != state.ParentID {
			return errors.New("only the parent can register members")
		}
		for _, id := range state.Members {
			member, err := decodeSessionID(id)
			if err != nil {
				return err
			}
			if err := requireDirectChild(opCtx, conn, member, parent, "member"); err != nil {
				return err
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
				owner, err = decodeSessionID(state.PinOwner)
				if err != nil {
					return err
				}
				if err := requireDirectChild(opCtx, conn, owner, parent, "artifact owner"); err != nil {
					return err
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
		if len(state.Append) == 0 {
			return nil
		}
		next, err := appendMessages(opCtx, conn, s.id, state.Sequence, state.Append)
		if err != nil {
			return err
		}
		return recordTurn(opCtx, conn, s.id, next, time.Now().UnixNano())
	})
}

func cloneRecords(records map[string]map[string]json.RawMessage) map[string]map[string]json.RawMessage {
	out := make(map[string]map[string]json.RawMessage, len(records))
	for kind, group := range records {
		out[kind] = maps.Clone(group)
	}
	return out
}

// OpenPublishedArtifact grants the caller a private reference only after the
// family publication pin is checked atomically. Arbitrary private blobs remain
// inaccessible even if a member guesses their content digest.
// Metadata uses the stored byte count and a bounded content-type probe; the
// returned reader starts at byte zero and does not load the entire artifact.
func (s *sqliteSession) OpenPublishedArtifact(ctx context.Context, id string) (artifacts.Ref, io.ReadCloser, error) {
	digest, err := artifactDigest(id)
	if err != nil {
		return artifacts.Ref{}, nil, err
	}
	ref := artifacts.Ref{ID: id}
	err = s.store.withWrite(ctx, func(conn *sql.Conn) error {
		if err := s.requireLease(ctx, conn); err != nil {
			return err
		}
		err := conn.QueryRowContext(ctx, `SELECT b.byte_count FROM artifact_blobs b
			JOIN swarm_artifacts a ON a.digest=b.digest
			WHERE a.digest=? AND a.parent_id=(SELECT coalesce(parent_id,id) FROM sessions WHERE id=?)`, digest, s.id).Scan(&ref.Bytes)
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("artifact is not published in this swarm")
		}
		if err != nil {
			return err
		}
		_, err = conn.ExecContext(ctx, `INSERT OR IGNORE INTO session_artifacts(session_id,digest) VALUES(?,?)`, s.id, digest)
		return err
	})
	if err != nil {
		return artifacts.Ref{}, nil, err
	}
	r, err := s.ArtifactStore().Open(ctx, id)
	if err != nil {
		return artifacts.Ref{}, nil, err
	}
	buffered := bufio.NewReaderSize(r, 512)
	probe, err := buffered.Peek(512)
	if err != nil && err != io.EOF {
		return artifacts.Ref{}, nil, errors.Join(err, r.Close())
	}
	ref.MIMEType = http.DetectContentType(probe)
	switch {
	case strings.HasPrefix(ref.MIMEType, "text/"):
		ref.Kind = artifacts.KindText
	case strings.HasPrefix(ref.MIMEType, "image/"):
		ref.Kind = artifacts.KindImage
		ref.ImageToken = "[image " + id + "]"
	default:
		ref.Kind = artifacts.KindBinary
	}
	return ref, &publishedArtifactReader{Reader: buffered, Closer: r}, nil
}

type publishedArtifactReader struct {
	io.Reader
	io.Closer
}
