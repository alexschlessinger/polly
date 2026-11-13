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
// appends, member retention and artifact pins commit together or not at all,
// except that a store answering from memory may register members ahead of the
// rest: a member can then be retained by a family that holds no record of it.
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
	// state passed to the callback carries the committed records' Revision.
	// It and Written are the fields a caller may read once the update returns.
	Revision uint64
	// Written is set by an UpdateCoordinationFrom that returned while a crash
	// of the process could still lose its changes. It returns once one no
	// longer can, or with the reason that will never be so. A store that
	// logs its updates returns from it when the log holds the update, which
	// may be before the database does. Nil means there is nothing to wait
	// for.
	Written func(context.Context) error
	// Changes is how an UpdateCoordinationFrom callback reports its work,
	// kind then id; a nil payload deletes the record. Records is nil there.
	Changes map[string]map[string]json.RawMessage
	// Volatile names records among Changes, kind then id, whose new value
	// says what the caller is doing at the moment and need not outlive the
	// process. A store may keep such a value in memory only: its own readers
	// see it at once, and it is written with the record's next change that
	// is not volatile, if there is one. A store is free to write it anyway.
	Volatile map[string]map[string]bool
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
	state.Records, state.Revision, err = s.store.coordination.records(ctx, conn, parent, s.store.drives)
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
	if family, _, ok := s.remembered(ctx, true); ok {
		if err := s.alive(ctx); err != nil {
			return nil, err
		}
		state := s.rememberedState(family)
		state.Records = copyRecords(family.records)
		return state, nil
	}
	state, err := s.reload(ctx)
	if err != nil {
		return nil, err
	}
	state.Records = copyRecords(state.Records)
	return state, nil
}

// reload reads the family through the database and remembers this session's
// row. The caller holds the cache, which owns the records returned.
func (s *sqliteSession) reload(ctx context.Context) (*CoordinationState, error) {
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
	return state, nil
}

// rememberedFamily returns the family's records when this session can answer
// from memory: its lease cannot have been taken, the writer is working, and
// this store drives the family. It also returns the handle on the family's
// root. The caller holds the cache.
func (s *sqliteSession) rememberedFamily() (cachedFamily, *sqliteSession, bool) {
	if s.coord.parent == nil || !s.leaseFresh() || !s.store.writer.healthy() {
		return cachedFamily{}, nil, false
	}
	root := s.store.driver(s, string(s.coord.parent))
	if root == nil {
		return cachedFamily{}, nil, false
	}
	family, ok := s.store.coordination.family(s.coord.parent)
	return family, root, ok
}

// remembered is rememberedFamily for a handle that may not know its row yet:
// one just acquired, or one whose row a transaction has written since. Such a
// handle first reads its root and, when sequence is set, its transcript's
// next sequence, so that a family this store drives is answered from memory
// from the handle's first call on. With sequence set, ok also says the next
// sequence is known. The caller holds the cache.
func (s *sqliteSession) remembered(ctx context.Context, sequence bool) (cachedFamily, *sqliteSession, bool) {
	if !s.leaseFresh() || !s.store.writer.healthy() {
		return cachedFamily{}, nil, false
	}
	if s.coord.parent == nil || sequence && !s.coord.known && s.coord.writers == 0 {
		if s.coord.parent != nil && s.store.driver(s, string(s.coord.parent)) == nil {
			return cachedFamily{}, nil, false
		}
		if err := s.learn(ctx); err != nil {
			return cachedFamily{}, nil, false
		}
	}
	family, root, ok := s.rememberedFamily()
	return family, root, ok && (!sequence || s.coord.known)
}

// learn reads this session's root and next sequence and remembers them. It
// does not settle: the row is as of the last checkpoint, and the messages
// pending for the session come after it. The caller holds the cache.
func (s *sqliteSession) learn(ctx context.Context) error {
	start, tail := s.store.writer.pendingTail(s.id)
	var parent []byte
	var sequence int64
	err := s.leasedRead(ctx, func(ctx context.Context, conn *sql.Conn) error {
		return conn.QueryRowContext(ctx, `SELECT coalesce(parent_id,id),next_sequence FROM sessions WHERE id=?`, s.id).Scan(&parent, &sequence)
	})
	if err != nil {
		return err
	}
	if len(tail) > 0 {
		sequence = max(sequence, start+int64(len(tail)))
	}
	s.remember(parent, sequence)
	return nil
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
	// fresh and this store drives the family.
	if parent := s.family.Load(); parent != nil && s.leaseFresh() && s.store.writer.healthy() && s.store.driver(s, *parent) != nil {
		if revision, ok := cache.peek(*parent); ok {
			if err := s.alive(ctx); err != nil {
				return 0, err
			}
			return revision, nil
		}
	}
	if err := cache.hold(ctx); err != nil {
		return 0, err
	}
	defer cache.release()
	if family, _, ok := s.remembered(ctx, false); ok {
		if err := s.alive(ctx); err != nil {
			return 0, err
		}
		return family.revision, nil
	}
	state, err := s.reload(ctx)
	if err != nil {
		return 0, err
	}
	return state.Revision, nil
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
	// A store may answer before it has written the update: every later read
	// sees it, and the callback's state carries Written for a caller that
	// must not act on the update before a crash could no longer undo it.
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
		if family, root, ok := s.remembered(ctx, true); ok {
			return s.updateRemembered(ctx, *known, family, root, fn)
		}
	}
	// A family this store does not drive is what the database holds, and a
	// store that is gone may have left its last updates in the log.
	if s.coord.parent == nil || s.store.driver(s, string(s.coord.parent)) == nil {
		if err := s.store.recoverLogs(ctx, true); err != nil {
			return s.mapError(ctx, err)
		}
	}
	var parent []byte
	// committed is the state fn accepted, set once the transaction starts
	// writing it.
	var committed *CoordinationState
	var before map[string]map[string]json.RawMessage
	var changed bool
	var next int64
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
		committed = state
		if known != nil {
			if changed, err = checkChanges(state.Changes); err == nil {
				err = writeRecordDelta(opCtx, conn, parent, state.Changes)
			}
		} else {
			changed, err = writeRecordChanges(opCtx, conn, parent, before, state.Records)
		}
		if err != nil {
			return err
		}
		if changed {
			if err := s.requireUndriven(opCtx, conn, parent); err != nil {
				return err
			}
		}
		next, err = s.writeRest(opCtx, conn, parent, state)
		return err
	})
	if err != nil {
		s.coord.known = false
		if committed != nil {
			// The commit may or may not have landed; the table decides.
			cache.forget(parent)
		}
		return err
	}
	s.remember(parent, next)
	if changed {
		records := committed.Records
		if known != nil {
			// The family's maps belong to the cache; only the changed records move.
			applyRecordChanges(before, committed.Changes)
			records = before
		}
		committed.Revision = cache.replace(parent, records)
	}
	return nil
}

// ErrFamilyDriven reports a change to a family's records made through a
// store handle that does not hold the family's root session while another
// one does. The handle that holds the root answers for the family from
// memory, so the records in the database are not the ones to change.
var ErrFamilyDriven = fmt.Errorf("%w: another store handle drives its family", ErrSessionInUse)

// requireUndriven fails with ErrFamilyDriven when a handle of another store
// holds the root of the family this session's transaction is changing.
func (s *sqliteSession) requireUndriven(ctx context.Context, conn rowQuerier, parent []byte) error {
	var owner []byte
	err := conn.QueryRowContext(ctx, `SELECT owner_token FROM session_leases WHERE session_id = ? AND expires_ns > ?`, parent, time.Now().UnixNano()).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if root := s.store.handle(string(parent)); root != nil && bytes.Equal(root.ownerToken, owner) {
		return nil
	}
	return ErrFamilyDriven
}

// writeRest writes what an update asks for beyond its records: members, pins,
// the transcript identity and the appended messages. It returns the
// transcript's next sequence.
func (s *sqliteSession) writeRest(ctx context.Context, conn *sql.Conn, parent []byte, state *CoordinationState) (int64, error) {
	if err := registerMembers(ctx, conn, parent, state); err != nil {
		return 0, err
	}
	if err := s.pinArtifacts(ctx, conn, parent, state); err != nil {
		return 0, err
	}
	next := state.Sequence + int64(len(state.Append))
	if state.Transcript != nil {
		if err := writeTranscriptIdentity(ctx, conn, s.id, state.Transcript, next); err != nil {
			return 0, err
		}
	}
	if len(state.Append) == 0 {
		return next, nil
	}
	if _, err := appendMessages(ctx, conn, s.id, state.Sequence, state.Append); err != nil {
		return 0, err
	}
	return next, recordTurn(ctx, conn, s.id, next, time.Now().UnixNano())
}

// updateRemembered is UpdateCoordinationFrom answered from memory. The
// callback sees what a transaction would have read. Its changes go to the
// family in memory and to the writer, and the update returns without
// touching the database, leaving in Written the wait for the update to be
// safe from a crash. An update that pins artifacts has rows to check and is
// written before it returns; one that only registers members writes the
// registration first and queues the rest. root is the handle on the family's
// root. The caller holds the cache.
func (s *sqliteSession) updateRemembered(ctx context.Context, known uint64, family cachedFamily, root *sqliteSession, fn func(*CoordinationState) error) error {
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
	changed, err := checkChanges(state.Changes)
	if err != nil {
		return err
	}
	if state.Transcript != nil {
		if err := state.Transcript.validate(); err != nil {
			return err
		}
	}
	// What is volatile changes the family in memory and goes no further.
	kept := keptChanges(state.Changes, state.Volatile)
	next := state.Sequence + int64(len(state.Append))
	if len(state.Pins) == 0 && len(state.Members) > 0 {
		// Registering a session that exists as a member keeps its
		// conversation with the family's. It has a row to check and none
		// that a queued update writes, so it is written by itself, ahead of
		// the records, which go the usual way.
		err := s.store.withTransactionNow(opCtx, "BEGIN IMMEDIATE", func(conn *sql.Conn) error {
			if err := s.requireLease(opCtx, conn); err != nil {
				return err
			}
			return registerMembers(opCtx, conn, parent, state)
		})
		if err != nil {
			return s.mapError(ctx, err)
		}
	}
	if len(state.Pins) > 0 {
		err := s.leasedUntracked(ctx, true, func(opCtx context.Context, conn *sql.Conn) error {
			if err := writeRecordDelta(opCtx, conn, parent, state.Changes); err != nil {
				return err
			}
			_, err := s.writeRest(opCtx, conn, parent, state)
			return err
		})
		if err != nil {
			// The commit may or may not have landed; the table decides.
			cache.forget(parent)
			s.coord.known = false
			return err
		}
	} else if len(kept) > 0 || len(state.Append) > 0 || state.Transcript != nil {
		update := &queuedUpdate{
			parent: parent, parentToken: root.ownerToken, changes: kept,
			session: s.id, sessionToken: s.ownerToken, sequence: state.Sequence, nowNS: time.Now().UnixNano(),
		}
		if state.Transcript != nil {
			identity := *state.Transcript
			identity.Sequence = next
			update.transcript = &identity
		}
		if update.appended, err = encodeMessages(state.Append); err != nil {
			return err
		}
		writer := s.store.writer
		place, kept, err := writer.accept(opCtx, update)
		if err != nil {
			return s.mapError(ctx, err)
		}
		if !kept {
			state.Written = func(ctx context.Context) error { return writer.wait(ctx, place) }
		}
	}
	s.coord.sequence = next
	if changed {
		// The family's maps belong to the cache; only the changed records move.
		applyRecordChanges(family.records, state.Changes)
		state.Revision = cache.replace(parent, family.records)
	}
	return nil
}

// keptChanges returns the changes that are not volatile, nil when none is.
func keptChanges(changes map[string]map[string]json.RawMessage, volatile map[string]map[string]bool) map[string]map[string]json.RawMessage {
	var kept map[string]map[string]json.RawMessage
	for kind, group := range changes {
		passing := volatile[kind]
		for id, value := range group {
			if passing[id] {
				continue
			}
			if len(passing) == 0 {
				// Nothing of this kind is volatile: the group is kept whole.
				if kept == nil {
					kept = make(map[string]map[string]json.RawMessage, len(changes))
				}
				kept[kind] = group
				break
			}
			if kept == nil {
				kept = make(map[string]map[string]json.RawMessage, len(changes))
			}
			if kept[kind] == nil {
				kept[kind] = map[string]json.RawMessage{}
			}
			kept[kind][id] = value
		}
	}
	return kept
}

// checkRecord reports whether value may be stored as the record kind/id.
func checkRecord(kind, id string, value json.RawMessage) error {
	if !json.Valid(value) || kind == "" || id == "" {
		return errors.New("invalid coordination record")
	}
	return nil
}

// checkChanges validates the records a callback named, where a nil payload
// deletes, and reports whether it named any.
func checkChanges(changes map[string]map[string]json.RawMessage) (changed bool, err error) {
	for kind, group := range changes {
		for id, value := range group {
			if value != nil {
				if err := checkRecord(kind, id, value); err != nil {
					return false, err
				}
			}
			changed = true
		}
	}
	return changed, nil
}

func deleteRecord(ctx context.Context, conn *sql.Conn, parent []byte, kind, id string) error {
	_, err := conn.ExecContext(ctx, `DELETE FROM swarm_records WHERE parent_id=? AND kind=? AND id=?`, parent, kind, id)
	return err
}

func putRecord(ctx context.Context, conn *sql.Conn, parent []byte, kind, id string, value json.RawMessage) error {
	// A record that exists is changed where it lies: replacing the row would
	// delete it and its key and insert both again.
	_, err := conn.ExecContext(ctx, `INSERT INTO swarm_records VALUES(?,?,?,?)
		ON CONFLICT(parent_id,kind,id) DO UPDATE SET payload_json = excluded.payload_json`, parent, kind, id, []byte(value))
	return err
}

// writeRecordDelta writes changes that checkChanges accepted.
func writeRecordDelta(ctx context.Context, conn *sql.Conn, parent []byte, changes map[string]map[string]json.RawMessage) error {
	for kind, group := range changes {
		for id, value := range group {
			var err error
			if value == nil {
				err = deleteRecord(ctx, conn, parent, kind, id)
			} else {
				err = putRecord(ctx, conn, parent, kind, id, value)
			}
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// writeRecordChanges writes only the records the callback changed: it
// deletes the ones removed and upserts the ones added or altered. A checkpoint
// touches one execution and a few messages, never the family's whole set.
func writeRecordChanges(ctx context.Context, conn *sql.Conn, parent []byte, before, after map[string]map[string]json.RawMessage) (changed bool, err error) {
	for kind, group := range before {
		for id := range group {
			if after[kind][id] == nil {
				if err := deleteRecord(ctx, conn, parent, kind, id); err != nil {
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
			if err := checkRecord(kind, id, value); err != nil {
				return false, err
			}
			if err := putRecord(ctx, conn, parent, kind, id, value); err != nil {
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
