package sessions

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/internal/coordindex"
	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
)

func (*sqliteSession) CoordinationCapabilities() CoordinationCapabilities {
	return CoordinationCapabilities{NativeIndexed: true, DurableRevisions: true}
}
func (*SQLiteStore) CoordinationCapabilities() CoordinationCapabilities {
	return CoordinationCapabilities{NativeIndexed: true, DurableRevisions: true}
}

type sqliteRecordTx struct {
	ctx            context.Context
	conn           *sql.Conn
	parent         []byte
	identity       CoordinationIdentity
	active         bool
	stamp          int64
	format         int64
	formatStamp    int64
	epoch          int64
	failure        error
	displayScalars bool
	changed        map[RevisionScope]bool
	effects        legacyRecordTx // reuses only the owned, accumulating effect envelope
}

func newSQLiteRecordTx(ctx context.Context, conn *sql.Conn, actor []byte) (*sqliteRecordTx, error) {
	r := &sqliteRecordTx{ctx: ctx, conn: conn, active: true}
	nativeSQL(ctx)
	err := conn.QueryRowContext(ctx, `SELECT coalesce(s.parent_id,s.id),s.next_sequence,p.coordination_stamp,p.coordination_format,coalesce(v.stamp,0),p.created_ns
 FROM sessions s JOIN sessions p ON p.id=coalesce(s.parent_id,s.id)
 LEFT JOIN swarm_revisions v ON v.parent_id=p.id AND v.index_name='record' AND v.index_key=? WHERE s.id=?`, CoordinationIndexKey("format", "swarm"), actor).Scan(&r.parent, &r.identity.Sequence, &r.stamp, &r.format, &r.formatStamp, &r.epoch)
	if err != nil {
		return nil, err
	}
	nativeRows(ctx, 1)
	r.identity.ActorID = hex.EncodeToString(actor)
	r.identity.ParentID = hex.EncodeToString(r.parent)
	r.effects = legacyRecordTx{ctx: ctx, active: true}
	return r, nil
}
func (r *sqliteRecordTx) close() { r.active = false; r.effects.close(); r.conn = nil }
func (r *sqliteRecordTx) ready() error {
	if !r.active {
		return ErrCoordinationClosed
	}
	if r.failure != nil {
		return r.failure
	}
	return r.ctx.Err()
}
func (r *sqliteRecordTx) Identity() (CoordinationIdentity, error) {
	if err := r.ready(); err != nil {
		return CoordinationIdentity{}, err
	}
	return r.identity, nil
}
func (r *sqliteRecordTx) checkKind(kind string) error {
	if coordindex.KnownKind(kind) && kind != "format" && kind != "parent_turn" && r.format != 2 {
		return fmt.Errorf("%w: version %d, require 2", ErrCoordinationFormat, r.format)
	}
	return nil
}
func (r *sqliteRecordTx) raw(key RecordKey) ([]byte, error) {
	nativeSQL(r.ctx)
	var payload []byte
	err := r.conn.QueryRowContext(r.ctx, `SELECT payload_json FROM swarm_records WHERE parent_id=? AND kind=? AND id=?`, r.parent, key.Kind, key.ID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	nativeRows(r.ctx, 1)
	nativePayload(r.ctx, payload)
	runtimemetrics.Add(r.ctx, runtimemetrics.CoordinationRecordsRead, 1)
	runtimemetrics.Add(r.ctx, runtimemetrics.CoordinationBytesRead, uint64(len(payload)))
	return payload, nil
}
func (r *sqliteRecordTx) Get(key RecordKey) (CoordinationRecord, bool, error) {
	if err := r.ready(); err != nil {
		return CoordinationRecord{}, false, err
	}
	if err := validCoordinationKey(key); err != nil {
		return CoordinationRecord{}, false, err
	}
	payload, err := r.raw(key)
	if err != nil || payload == nil {
		return CoordinationRecord{}, false, err
	}
	if err = r.checkKind(key.Kind); err != nil {
		return CoordinationRecord{}, false, err
	}
	// Extraction validates a selected payload, never any unrelated family row.
	if _, _, err = coordindex.Extract(key.Kind, key.ID, payload); err != nil {
		return CoordinationRecord{}, false, err
	}
	runtimemetrics.Add(r.ctx, runtimemetrics.RecordsDecoded, 1)
	rev, err := r.revision(RevisionScope{CoordinationIndexRecord, CoordinationIndexKey(key.Kind, key.ID)})
	return CoordinationRecord{Key: key, Payload: payload, Revision: rev}, err == nil, err
}
func (r *sqliteRecordTx) beginChanges() error {
	// Callback reads expose staged payloads and partition revisions. Allocate a
	// fresh durable stamp for every actual mutation, not just the first one, so
	// an earlier staged value cannot share a token with later committed data.
	// Put/Delete skip this path for no-ops; only touched scopes are restamped.
	if r.stamp == math.MaxInt64 {
		return fmt.Errorf("%w: coordination stamp overflow", ErrCoordinationRequest)
	}
	r.stamp++
	nativeSQL(r.ctx)
	if _, err := r.conn.ExecContext(r.ctx, `UPDATE sessions SET coordination_stamp=? WHERE id=?`, r.stamp, r.parent); err != nil {
		return err
	}
	if r.changed == nil {
		r.changed = map[RevisionScope]bool{}
	}
	return nil
}
func (r *sqliteRecordTx) change(key RecordKey, payload []byte) (err error) {
	// A swallowed SQL failure must never commit partially updated indexes.
	defer func() {
		if err != nil {
			r.failure = err
		}
	}()
	if err := r.beginChanges(); err != nil {
		return err
	}
	if err := indexedRecordChange(r.ctx, r.conn, r.parent, key, payload, r.stamp, r.changed); err != nil {
		return err
	}
	if key == (RecordKey{"format", "swarm"}) {
		var f struct {
			Version int64 `json:"version"`
		}
		if json.Unmarshal(payload, &f) != nil {
			f.Version = 0
		}
		r.format = max(f.Version, 0)
		r.formatStamp = r.stamp
	}
	return nil
}
func (r *sqliteRecordTx) Put(key RecordKey, payload json.RawMessage) error {
	if err := r.ready(); err != nil {
		return err
	}
	if err := validCoordinationKey(key); err != nil {
		return err
	}
	if !json.Valid(payload) {
		return fmt.Errorf("%w: invalid JSON", ErrCoordinationFormat)
	}
	before, err := r.raw(key)
	if err != nil {
		return err
	}
	if bytes.Equal(before, payload) {
		return nil
	}
	return r.change(key, payload)
}
func (r *sqliteRecordTx) Delete(key RecordKey) error {
	if err := r.ready(); err != nil {
		return err
	}
	if err := validCoordinationKey(key); err != nil {
		return err
	}
	before, err := r.raw(key)
	if err != nil || before == nil {
		return err
	}
	return r.change(key, nil)
}
func (r *sqliteRecordTx) Effects(effects CoordinationEffects) error {
	if err := r.ready(); err != nil {
		return err
	}
	return r.effects.Effects(effects)
}

func (s *sqliteSession) ReadRecords(ctx context.Context, fn func(CoordinationReader) error) error {
	if fn == nil {
		return fmt.Errorf("%w: callback required", ErrCoordinationRequest)
	}
	runtimemetrics.Add(ctx, runtimemetrics.CoordinationReads, 1)
	defer runtimemetrics.Start(ctx, runtimemetrics.CoordinationRead).Stop()
	return s.leased(ctx, false, func(ctx context.Context, conn *sql.Conn) error {
		r, err := newSQLiteRecordTx(ctx, conn, s.id)
		if err != nil {
			return err
		}
		defer r.close()
		if err = fn(readOnlySQLiteRecords(r)); err != nil {
			return err
		}
		return r.ready()
	})
}
func (s *sqliteSession) UpdateRecords(ctx context.Context, fn func(CoordinationTx) error) (CoordinationCommit, error) {
	if fn == nil {
		return CoordinationCommit{}, fmt.Errorf("%w: callback required", ErrCoordinationRequest)
	}
	runtimemetrics.Add(ctx, runtimemetrics.CoordinationUpdates, 1)
	defer runtimemetrics.Start(ctx, runtimemetrics.CoordinationUpdate).Stop()
	var commit CoordinationCommit
	err := s.leased(ctx, true, func(ctx context.Context, conn *sql.Conn) error {
		r, err := newSQLiteRecordTx(ctx, conn, s.id)
		if err != nil {
			return err
		}
		defer r.close()
		if err = fn(r); err != nil {
			return err
		}
		if err = r.ready(); err != nil {
			return err
		}
		e := r.effects.effects
		state := &CoordinationState{ActorID: r.identity.ActorID, ParentID: r.identity.ParentID, Sequence: r.identity.Sequence, Append: e.Append, ExpectedSequence: e.ExpectedSequence, Transcript: e.Transcript, Pins: e.Pins, PinOwner: e.PinOwner, Members: e.Members}
		next, err := s.persistCoordinationEffects(ctx, conn, r.parent, state)
		if err != nil {
			return err
		}
		commit = CoordinationCommit{Sequence: next, Changed: sortedScopes(r.changed)}
		return nil
	})
	if err != nil {
		return CoordinationCommit{}, err
	}
	return commit, nil
}

func (s *sqliteSession) persistCoordinationEffects(ctx context.Context, conn *sql.Conn, parent []byte, state *CoordinationState) (int64, error) {
	if state.ExpectedSequence != nil && *state.ExpectedSequence != state.Sequence {
		return 0, fmt.Errorf("%w: checkpoint sequence changed", ErrCoordinationRequest)
	}
	if state.Sequence < 0 || int64(len(state.Append)) > math.MaxInt64-state.Sequence {
		return 0, fmt.Errorf("%w: sequence overflow", ErrCoordinationRequest)
	}
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
	if len(state.Append) > 0 {
		if _, err := appendMessages(ctx, conn, s.id, state.Sequence, state.Append); err != nil {
			return 0, err
		}
		if err := recordTurn(ctx, conn, s.id, next, time.Now().UnixNano()); err != nil {
			return 0, err
		}
	}
	return next, nil
}

func (s *SQLiteStore) ReadRecordsView(ctx context.Context, root string, fn func(CoordinationReader) error) error {
	if fn == nil {
		return fmt.Errorf("%w: callback required", ErrCoordinationRequest)
	}
	if err := s.ensureOpen(); err != nil {
		return err
	}
	id, err := decodeSessionID(root)
	if err != nil {
		return err
	}
	return s.withRead(ctx, func(conn *sql.Conn) error {
		nativeSQL(ctx)
		var updated, ttl int64
		var parent []byte
		err := conn.QueryRowContext(ctx, `SELECT parent_id,updated_ns,ttl_ns FROM sessions WHERE id=?`, id).Scan(&parent, &updated, &ttl)
		if errors.Is(err, sql.ErrNoRows) || len(parent) > 0 {
			return ErrSessionNotFound
		}
		if err != nil {
			return err
		}
		nativeRows(ctx, 1)
		if _, err = visibleSession(ctx, conn, id, updated, ttl, time.Now().UnixNano()); err != nil {
			return err
		}
		r, err := newSQLiteRecordTx(ctx, conn, id)
		if err != nil {
			return err
		}
		defer r.close()
		if err = fn(readOnlySQLiteRecords(r)); err != nil {
			return err
		}
		return r.ready()
	})
}

type nativeRecordCursor struct {
	Version                 int
	Parent, Kind, Key, Sort []byte
	Index                   CoordinationIndex
}

// recordPageSQL selects the paged record window. A kind filter names the
// covering kind index: no statement in this store maintains ANALYZE statistics,
// and without them the planner prefers the partition primary key, visiting every
// preceding row in the partition before filtering. The index is created on open
// and shape-checked, so INDEXED BY cannot fail on a database that opened.
func recordPageSQL(kind string) string {
	from := `swarm_relations r`
	if kind != "" {
		from += ` INDEXED BY swarm_relations_kind_idx`
	}
	return `SELECT r.kind,r.id,r.sort_key,length(s.payload_json) FROM ` + from + ` JOIN swarm_records s ON s.parent_id=r.parent_id AND s.kind=r.kind AND s.id=r.id WHERE `
}

func (r *sqliteRecordTx) Query(q RecordQuery) (RecordPage, error) {
	if err := r.ready(); err != nil {
		return RecordPage{}, err
	}
	if err := validateCoordinationQuery(q); err != nil {
		return RecordPage{}, err
	}
	scope, err := normalizeCoordinationScope(RevisionScope{q.Index, q.Key})
	if err != nil {
		return RecordPage{}, err
	}
	if isRevisionOnlyCoordinationIndex(scope.Index) {
		return RecordPage{}, ErrCoordinationSelector
	}
	kind := selectorKind(scope)
	if q.Kind != "" {
		if kind != "" && kind != q.Kind {
			return RecordPage{}, ErrCoordinationSelector
		}
		kind = q.Kind
	}
	if err = r.checkScope(scope, kind); err != nil {
		return RecordPage{}, err
	}
	cursor := nativeRecordCursor{Version: 1, Parent: []byte(r.identity.ParentID), Kind: []byte(q.Kind), Index: scope.Index, Key: []byte(scope.Key)}
	if q.After != "" {
		b, e := base64.RawURLEncoding.DecodeString(q.After)
		var c nativeRecordCursor
		if e != nil || json.Unmarshal(b, &c) != nil || c.Version != 1 || !bytes.Equal(c.Parent, cursor.Parent) || !bytes.Equal(c.Kind, cursor.Kind) || !bytes.Equal(c.Key, cursor.Key) || c.Index != cursor.Index || len(c.Sort) == 0 {
			return RecordPage{}, ErrCoordinationCursor
		}
		cursor.Sort = c.Sort
	}
	where := `r.parent_id=? AND r.index_name=? AND r.index_key=?`
	args := []any{r.parent, string(scope.Index), scope.Key}
	if kind != "" {
		where += ` AND r.kind=?`
		args = append(args, kind)
	}
	page := RecordPage{Total: -1}
	if q.IncludeTotal {
		countScope := scope
		if scope.Index == CoordinationIndexRecords && kind != "" {
			countScope = RevisionScope{CoordinationIndexKind, kind}
		}
		if kind == "" || kind == selectorKind(countScope) {
			nativeSQL(r.ctx)
			e := r.conn.QueryRowContext(r.ctx, `SELECT record_count FROM swarm_revisions WHERE parent_id=? AND index_name=? AND index_key=?`, r.parent, string(countScope.Index), countScope.Key).Scan(&page.Total)
			if errors.Is(e, sql.ErrNoRows) {
				page.Total = 0
			} else if e != nil {
				return page, e
			} else {
				nativeRows(r.ctx, 1)
			}
		} else {
			nativeSQL(r.ctx)
			if e := r.conn.QueryRowContext(r.ctx, `SELECT count(*) FROM swarm_relations r WHERE `+where, args...).Scan(&page.Total); e != nil {
				return page, e
			}
			nativeRows(r.ctx, 1)
		}
	}
	if q.CountOnly {
		if q.IncludeLast {
			page.Last, err = r.lastKey(scope, kind)
			if err != nil {
				return RecordPage{}, err
			}
		}
		return page, r.ready()
	}
	if len(cursor.Sort) > 0 {
		where += ` AND r.sort_key>?`
		args = append(args, cursor.Sort)
	}
	args = append(args, q.Limit+1, q.Offset)
	type selection struct {
		key  RecordKey
		sort []byte
		size int
	}
	var selected []selection
	nativeSQL(r.ctx)
	err = queryEach(r.ctx, r.conn, recordPageSQL(kind)+where+` ORDER BY r.sort_key LIMIT ? OFFSET ?`, args, func(rows *sql.Rows) error {
		var v selection
		if err := rows.Scan(&v.key.Kind, &v.key.ID, &v.sort, &v.size); err != nil {
			return err
		}
		nativeRows(r.ctx, 1)
		selected = append(selected, v)
		return nil
	})
	if err != nil {
		return RecordPage{}, err
	}
	used := 0
	for _, v := range selected {
		if len(page.Records) == q.Limit {
			break
		}
		if q.MaxBytes > 0 && v.size > q.MaxBytes-used {
			if len(page.Records) > 0 {
				break
			}
			page.Oversized = true
		}
		record, found, err := r.Get(v.key)
		if err != nil {
			return RecordPage{}, err
		}
		if !found {
			return RecordPage{}, errors.New("coordination relation has no record")
		}
		page.Records = append(page.Records, record)
		cursor.Sort = v.sort
		used += v.size
		if page.Oversized {
			break
		}
	}
	if len(page.Records) < len(selected) && len(page.Records) > 0 {
		b, err := json.Marshal(cursor)
		if err != nil {
			return RecordPage{}, err
		}
		page.Next = base64.RawURLEncoding.EncodeToString(b)
	}
	return page, r.ready()
}

func (r *sqliteRecordTx) revisionToken(scope RevisionScope, stamp int64) string {
	return r.identity.ParentID + ":" + strconv.FormatInt(r.epoch, 10) + ":" + strconv.FormatInt(r.formatStamp, 10) + ":" + strconv.FormatInt(stamp, 10) + ":" + CoordinationIndexKey(string(scope.Index), scope.Key)
}
func (r *sqliteRecordTx) checkScope(scope RevisionScope, kind string) error {
	if scope.Index == CoordinationIndexOverviewDependency && r.format != 2 {
		return fmt.Errorf("%w: version %d, require 2", ErrCoordinationFormat, r.format)
	}
	return r.checkKind(kind)
}

func (r *sqliteRecordTx) revision(scope RevisionScope) (Revision, error) {
	var stamp int64
	nativeSQL(r.ctx)
	err := r.conn.QueryRowContext(r.ctx, `SELECT stamp FROM swarm_revisions WHERE parent_id=? AND index_name=? AND index_key=?`, r.parent, string(scope.Index), scope.Key).Scan(&stamp)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	} else if err == nil {
		nativeRows(r.ctx, 1)
	}
	return Revision{Scope: scope, Token: r.revisionToken(scope, stamp)}, err
}
func (r *sqliteRecordTx) Revisions(scopes []RevisionScope) ([]Revision, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	if len(scopes) > MaxCoordinationRevisionScopes {
		return nil, fmt.Errorf("%w: at most %d revision scopes", ErrCoordinationRequest, MaxCoordinationRevisionScopes)
	}
	out := make([]Revision, len(scopes))
	if len(scopes) == 0 {
		return out, nil
	}
	args := make([]any, 0, 3*len(scopes)+1)
	for i, scope := range scopes {
		normalized, err := normalizeCoordinationScope(scope)
		if err != nil {
			return nil, err
		}
		if err = r.checkScope(normalized, selectorKind(normalized)); err != nil {
			return nil, err
		}
		out[i].Scope = normalized
		args = append(args, i, string(normalized.Index), normalized.Key)
	}
	args = append(args, r.parent)
	// 769 parameters at the maximum, below even SQLite's historical 999 limit.
	// Each requested scope performs one primary-key probe in this snapshot.
	nativeSQL(r.ctx)
	err := queryEach(r.ctx, r.conn, `WITH requested(position,index_name,index_key) AS (VALUES `+strings.TrimSuffix(strings.Repeat("(?,?,?),", len(scopes)), ",")+`) SELECT q.position,coalesce(v.stamp,0) FROM requested q LEFT JOIN swarm_revisions v ON v.parent_id=? AND v.index_name=q.index_name AND v.index_key=q.index_key`, args, func(rows *sql.Rows) error {
		var i int
		var stamp int64
		if err := rows.Scan(&i, &stamp); err != nil {
			return err
		}
		nativeRows(r.ctx, 1)
		out[i].Token = r.revisionToken(out[i].Scope, stamp)
		return nil
	})
	return out, err
}
