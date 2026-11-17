package sessions

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
)

// CoordinationSession is optional. Bare Session and SessionStore clients do
// not need to construct a swarm. Updates are fenced by the caller's lease.
// Managed execution requires this capability on both parent and child handles.
// UpdateCoordination invokes its callback at most once. Records, transcript
// appends, member retention and artifact pins commit together or not at all.
// An error may represent an uncertain commit; callers must inspect durable
// receipts rather than blindly retrying the callback or its external effects.
type CoordinationSession interface {
	ViewIdentity
	ReadCoordination(context.Context) (*CoordinationState, error)
	UpdateCoordination(context.Context, func(*CoordinationState) error) error
	// OpenPublishedArtifact opens an artifact the caller's family published
	// and grants the caller private ownership of its bytes, which then
	// outlive the author and the parent until every owner releases them.
	OpenPublishedArtifact(context.Context, string) (artifacts.Ref, io.ReadCloser, error)
}

// CoordinationState is a transaction-local copy. Runtime records are grouped
// by domain (run, task, member, mail, publication, workflow, execution). The
// store owns identity, atomicity and retention; the runtime owns transitions.
// Callbacks must not call back into the store or retain this value.
type CoordinationState struct {
	ActorID, ParentID string
	Records           map[string]map[string]json.RawMessage
	// Revision identifies the family's records within this store handle: two
	// states with the same nonzero Revision hold the same Records. Zero means
	// the store does not track it. After a successful UpdateCoordination the
	// state passed to the callback carries the committed records' Revision,
	// which is the one field a caller may read once the update returns.
	Revision uint64
	// Changes is how an UpdateCoordinationFrom callback reports its work,
	// kind then id; a nil payload deletes the record. Records is nil there.
	Changes map[string]map[string]json.RawMessage
	// Append and ExpectedSequence atomically checkpoint generated output and
	// admitted mail with the records containing its delivery receipts.
	Append           []messages.ChatMessage
	ExpectedSequence *int64
	Sequence         int64
	// Transcript binds the resulting conversation to an execution in this
	// transaction. The store fills its Sequence from the committed append.
	Transcript *TranscriptIdentity
	// Pins name artifacts the actor already owns. Publication pins survive
	// retirement of the actor and are collected with the parent.
	Pins []string
	// PinOwner may be a direct child when the parent records that child's
	// publication. A member can only pin its own bytes.
	PinOwner string
	// Members pins child conversations until the parent is removed. TTL
	// sweeps leave a parent with records, and the members it pins, alone
	// until the parent is deleted explicitly.
	Members []string
}

func applySchemaV5(ctx context.Context, conn *sql.Conn) error {
	return execAll(ctx, conn,
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
// caller has already confirmed. The caller holds the coordination cache; the
// records it returns belong to the cache and must not be modified.
func (s *sqliteSession) loadCoordination(ctx context.Context, conn *sql.Conn) (*CoordinationState, []byte, error) {
	var parent []byte
	var sequence int64
	err := conn.QueryRowContext(ctx, `SELECT coalesce(parent_id,id),next_sequence FROM sessions WHERE id=?`, s.id).Scan(&parent, &sequence)
	if err != nil {
		return nil, nil, err
	}
	state := &CoordinationState{ActorID: s.ViewID(), ParentID: hex.EncodeToString(parent), Sequence: sequence}
	state.Records, state.Revision, err = s.store.coordination.records(ctx, conn, parent)
	return state, parent, err
}

// readSwarmRecords loads the family's records grouped by kind, then id.
func readSwarmRecords(ctx context.Context, conn *sql.Conn, parent []byte) (map[string]map[string]json.RawMessage, error) {
	records := map[string]map[string]json.RawMessage{}
	err := queryEach(ctx, conn, `SELECT kind,id,payload_json FROM swarm_records WHERE parent_id=? ORDER BY kind,id`, []any{parent}, func(rows *sql.Rows) error {
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
	cache := s.store.coordination
	if err := cache.hold(ctx); err != nil {
		return nil, err
	}
	defer cache.release()
	if family, ok := s.rememberedFamily(); ok && s.coord.known && s.coord.writers == 0 {
		_, cleanup, err := s.operationContext(ctx)
		if err != nil {
			return nil, err
		}
		cleanup()
		state := s.rememberedState(family)
		state.Records = copyRecords(family.records)
		return state, nil
	}
	var state *CoordinationState
	err := s.leasedUntracked(ctx, false, func(ctx context.Context, conn *sql.Conn) (err error) {
		var parent []byte
		state, parent, err = s.loadCoordination(ctx, conn)
		if err == nil {
			s.remember(parent, state.Sequence)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	state.Records = copyRecords(state.Records)
	return state, nil
}

// rememberedFamily returns the family's records when this session can answer
// from memory: its lease cannot have been taken, the writer is working, and
// the database confirmed the records recently. The caller holds the cache.
func (s *sqliteSession) rememberedFamily() (cachedFamily, bool) {
	if s.coord.parent == nil || !s.leaseFresh() || !s.store.writer.healthy() {
		return cachedFamily{}, false
	}
	return s.store.coordination.current(s.coord.parent)
}

// rememberedState is the state loadCoordination would read, without Records.
func (s *sqliteSession) rememberedState(family cachedFamily) *CoordinationState {
	return &CoordinationState{ActorID: s.ViewID(), ParentID: hex.EncodeToString(s.coord.parent), Sequence: s.coord.sequence, Revision: family.revision}
}

// remember keeps what a transaction just read or wrote of this session's
// row. The caller holds the cache.
func (s *sqliteSession) remember(parent []byte, sequence int64) {
	s.coord.parent, s.coord.sequence = parent, sequence
	if known := s.family.Load(); known == nil || *known != string(parent) {
		family := string(parent)
		s.family.Store(&family)
	}
	// A write in flight on the row may land after this transaction.
	s.coord.known = s.coord.writers == 0
}

// CoordinationRevisionSession is optional. It answers whether the family's
// records changed since a state the caller still holds, without copying them.
type CoordinationRevisionSession interface {
	// CoordinationRevision returns the Revision ReadCoordination would.
	CoordinationRevision(context.Context) (uint64, error)
}

func (s *sqliteSession) CoordinationRevision(ctx context.Context) (uint64, error) {
	cache := s.store.coordination
	// The usual answer needs no turn at the cache: this session's lease is
	// fresh and the family was confirmed a moment ago.
	if parent := s.family.Load(); parent != nil && s.leaseFresh() && s.store.writer.healthy() {
		if revision, ok := cache.peek(*parent); ok {
			if cause := context.Cause(s.ctx); cause != nil {
				return 0, cause
			}
			if ctx != nil {
				if cause := context.Cause(ctx); cause != nil {
					return 0, cause
				}
			}
			return revision, nil
		}
	}
	if err := cache.hold(ctx); err != nil {
		return 0, err
	}
	defer cache.release()
	if family, ok := s.rememberedFamily(); ok {
		_, cleanup, err := s.operationContext(ctx)
		if err != nil {
			return 0, err
		}
		cleanup()
		return family.revision, nil
	}
	var revision uint64
	err := s.leasedUntracked(ctx, false, func(ctx context.Context, conn *sql.Conn) error {
		state, parent, err := s.loadCoordination(ctx, conn)
		if err == nil {
			revision = state.Revision
			s.remember(parent, state.Sequence)
		}
		return err
	})
	return revision, err
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
	return s.updateCoordination(ctx, nil, fn)
}

// ErrCoordinationStale reports that the family's records are no longer at the
// revision an UpdateCoordinationFrom caller holds. Nothing ran or changed.
var ErrCoordinationStale = errors.New("coordination records changed")

// CoordinationDeltaSession is optional. It serves a caller that holds the
// family's records in memory and so needs neither a copy of them nor a
// comparison to find what it changed.
type CoordinationDeltaSession interface {
	// UpdateCoordinationFrom is UpdateCoordination for a caller holding the
	// records at revision known. The callback receives no Records and reports
	// the records it changed in Changes. When the family is no longer at
	// known, it returns ErrCoordinationStale without invoking the callback.
	UpdateCoordinationFrom(ctx context.Context, known uint64, fn func(*CoordinationState) error) error
}

func (s *sqliteSession) UpdateCoordinationFrom(ctx context.Context, known uint64, fn func(*CoordinationState) error) error {
	return s.updateCoordination(ctx, &known, fn)
}

// updateCoordination runs one coordination transaction. With known set the
// callback reports changes against that revision; otherwise it edits a copy
// of the records and the difference is what gets written.
func (s *sqliteSession) updateCoordination(ctx context.Context, known *uint64, fn func(*CoordinationState) error) error {
	if fn == nil {
		return errors.New("coordination update is required")
	}
	cache := s.store.coordination
	if err := cache.hold(ctx); err != nil {
		return err
	}
	defer cache.release()
	if known != nil {
		if family, ok := s.rememberedFamily(); ok && s.coord.known && s.coord.writers == 0 {
			return s.updateRemembered(ctx, *known, family, fn)
		}
	}
	var parent []byte
	var committed *CoordinationState
	var before map[string]map[string]json.RawMessage
	changed, wrote := false, false
	next := int64(-1)
	err := s.leasedUntracked(ctx, true, func(opCtx context.Context, conn *sql.Conn) error {
		state, family, err := s.loadCoordination(opCtx, conn)
		if err != nil {
			return err
		}
		parent, before = family, state.Records
		if known != nil {
			if state.Revision != *known {
				return ErrCoordinationStale
			}
			state.Records = nil
		} else {
			state.Records = copyRecords(before)
		}
		if err := fn(state); err != nil {
			return err
		}
		if state.ExpectedSequence != nil && *state.ExpectedSequence != state.Sequence {
			return errors.New("checkpoint sequence changed")
		}
		wrote = true
		if known != nil {
			changed, err = writeRecordDelta(opCtx, conn, parent, state.Changes)
		} else {
			changed, err = writeRecordChanges(opCtx, conn, parent, before, state.Records)
		}
		if err != nil {
			return err
		}
		committed = state
		if err := registerMembers(opCtx, conn, parent, state); err != nil {
			return err
		}
		if err := s.pinArtifacts(opCtx, conn, parent, state); err != nil {
			return err
		}
		if state.Transcript != nil {
			if err := writeTranscriptIdentity(opCtx, conn, s.id, state.Transcript, state.Sequence+int64(len(state.Append))); err != nil {
				return err
			}
		}
		next = state.Sequence
		if len(state.Append) == 0 {
			return nil
		}
		next, err = appendMessages(opCtx, conn, s.id, state.Sequence, state.Append)
		if err != nil {
			return err
		}
		return recordTurn(opCtx, conn, s.id, next, time.Now().UnixNano())
	})
	if err == nil && next >= 0 {
		s.remember(parent, next)
	} else {
		s.coord.known = false
	}
	switch {
	case err == nil && changed && known != nil:
		// The family's maps belong to the cache; only the changed records move.
		for kind, group := range committed.Changes {
			for id, value := range group {
				if value == nil {
					delete(before[kind], id)
					continue
				}
				if before[kind] == nil {
					before[kind] = map[string]json.RawMessage{}
				}
				before[kind][id] = value
			}
		}
		committed.Revision = cache.replace(parent, before)
	case err == nil && changed:
		committed.Revision = cache.replace(parent, committed.Records)
	case err != nil && wrote:
		// The commit may or may not have landed; the table decides.
		cache.forget(parent)
	}
	return err
}

// updateRemembered is UpdateCoordinationFrom answered from memory. The
// callback sees what a transaction would have read. Its changes go to the
// family in memory and to the writer's queue, and the update returns without
// touching the database; an update that pins artifacts or registers members
// has rows to check and is written before it returns. The caller holds the
// cache.
func (s *sqliteSession) updateRemembered(ctx context.Context, known uint64, family cachedFamily, fn func(*CoordinationState) error) error {
	opCtx, cleanup, err := s.operationContext(ctx)
	if err != nil {
		return err
	}
	defer cleanup()
	if family.revision != known {
		return ErrCoordinationStale
	}
	cache, parent := s.store.coordination, s.coord.parent
	state := s.rememberedState(family)
	if err := fn(state); err != nil {
		return err
	}
	if state.ExpectedSequence != nil && *state.ExpectedSequence != state.Sequence {
		return errors.New("checkpoint sequence changed")
	}
	changed := false
	for kind, group := range state.Changes {
		for id, value := range group {
			if value != nil && (!json.Valid(value) || kind == "" || id == "") {
				return errors.New("invalid coordination record")
			}
			changed = true
		}
	}
	if state.Transcript != nil && (state.Transcript.Execution == "" || state.Transcript.Generation <= 0) {
		return errors.New("invalid transcript execution identity")
	}
	next := state.Sequence + int64(len(state.Append))
	if len(state.Pins) > 0 || len(state.Members) > 0 {
		err := s.leasedUntracked(ctx, true, func(opCtx context.Context, conn *sql.Conn) error {
			if _, err := writeRecordDelta(opCtx, conn, parent, state.Changes); err != nil {
				return err
			}
			if err := registerMembers(opCtx, conn, parent, state); err != nil {
				return err
			}
			if err := s.pinArtifacts(opCtx, conn, parent, state); err != nil {
				return err
			}
			if state.Transcript != nil {
				if err := writeTranscriptIdentity(opCtx, conn, s.id, state.Transcript, next); err != nil {
					return err
				}
			}
			if len(state.Append) == 0 {
				return nil
			}
			if _, err := appendMessages(opCtx, conn, s.id, state.Sequence, state.Append); err != nil {
				return err
			}
			return recordTurn(opCtx, conn, s.id, next, time.Now().UnixNano())
		})
		if err != nil {
			// The commit may or may not have landed; the table decides.
			cache.forget(parent)
			s.coord.known = false
			return err
		}
	} else if changed || len(state.Append) > 0 || state.Transcript != nil {
		update := queuedUpdate{parent: parent, changes: state.Changes, session: s.id, sequence: state.Sequence, nowNS: time.Now().UnixNano()}
		if state.Transcript != nil {
			identity := *state.Transcript
			update.transcript = &identity
		}
		for i, message := range state.Append {
			payload, err := json.Marshal(message)
			if err != nil {
				return fmt.Errorf("encode message %d: %w", i, err)
			}
			update.appended = append(update.appended, payload)
		}
		if err := s.store.writer.accept(opCtx, update); err != nil {
			return s.mapError(ctx, err)
		}
	}
	s.coord.sequence = next
	if changed {
		// The family's maps belong to the cache; only the changed records move.
		for kind, group := range state.Changes {
			for id, value := range group {
				if value == nil {
					delete(family.records[kind], id)
					continue
				}
				if family.records[kind] == nil {
					family.records[kind] = map[string]json.RawMessage{}
				}
				family.records[kind][id] = value
			}
		}
		state.Revision = cache.replace(parent, family.records)
	}
	return nil
}

// writeRecordDelta writes the records a callback named: a nil payload deletes.
func writeRecordDelta(ctx context.Context, conn *sql.Conn, parent []byte, changes map[string]map[string]json.RawMessage) (changed bool, err error) {
	for kind, group := range changes {
		for id, value := range group {
			if value == nil {
				if _, err := conn.ExecContext(ctx, `DELETE FROM swarm_records WHERE parent_id=? AND kind=? AND id=?`, parent, kind, id); err != nil {
					return false, err
				}
				changed = true
				continue
			}
			if !json.Valid(value) || kind == "" || id == "" {
				return false, errors.New("invalid coordination record")
			}
			if _, err := conn.ExecContext(ctx, `INSERT OR REPLACE INTO swarm_records VALUES(?,?,?,?)`, parent, kind, id, []byte(value)); err != nil {
				return false, err
			}
			changed = true
		}
	}
	return changed, nil
}

// writeRecordChanges writes only the records the callback changed: it
// deletes the ones removed and upserts the ones added or altered. A checkpoint
// touches one execution and a few messages, never the family's whole set.
func writeRecordChanges(ctx context.Context, conn *sql.Conn, parent []byte, before, after map[string]map[string]json.RawMessage) (changed bool, err error) {
	for kind, group := range before {
		for id := range group {
			if after[kind][id] == nil {
				if _, err := conn.ExecContext(ctx, `DELETE FROM swarm_records WHERE parent_id=? AND kind=? AND id=?`, parent, kind, id); err != nil {
					return false, err
				}
				changed = true
			}
		}
	}
	for kind, group := range after {
		for id, value := range group {
			// An unchanged value came from the table and was checked when
			// it was written.
			if previous, ok := before[kind][id]; ok && bytes.Equal(previous, value) {
				continue
			}
			if !json.Valid(value) || kind == "" || id == "" {
				return false, errors.New("invalid coordination record")
			}
			if _, err := conn.ExecContext(ctx, `INSERT OR REPLACE INTO swarm_records VALUES(?,?,?,?)`, parent, kind, id, []byte(value)); err != nil {
				return false, err
			}
			changed = true
		}
	}
	return changed, nil
}

// registerMembers pins state.Members, direct children of the parent, to the
// family. Only the parent registers members.
func registerMembers(ctx context.Context, conn *sql.Conn, parent []byte, state *CoordinationState) error {
	if len(state.Members) > 0 && state.ActorID != state.ParentID {
		return errors.New("only the parent can register members")
	}
	for _, id := range state.Members {
		member, err := decodeSessionID(id)
		if err != nil {
			return err
		}
		if err := requireDirectChild(ctx, conn, member, parent, "member"); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO swarm_members VALUES(?,?)`, parent, member); err != nil {
			return err
		}
	}
	return nil
}

// pinArtifacts publishes state.Pins to the family. A member publishes only
// artifacts it owns; the parent may also publish a direct child's.
func (s *sqliteSession) pinArtifacts(ctx context.Context, conn *sql.Conn, parent []byte, state *CoordinationState) error {
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
			if err := requireDirectChild(ctx, conn, owner, parent, "artifact owner"); err != nil {
				return err
			}
		}
		var owns bool
		if err := conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_artifacts WHERE session_id=? AND digest=?)`, owner, digest).Scan(&owns); err != nil {
			return err
		}
		if !owns {
			return errors.New("cannot publish an artifact the caller does not own")
		}
		if _, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO swarm_artifacts VALUES(?,?)`, parent, digest); err != nil {
			return err
		}
	}
	return nil
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
	err = s.leased(ctx, true, func(ctx context.Context, conn *sql.Conn) error {
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
