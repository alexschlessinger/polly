package sessions

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"

	"github.com/alexschlessinger/pollytool/internal/coordindex"
	"github.com/alexschlessinger/pollytool/internal/coordsem"
)

func createSemanticSchema(ctx context.Context, conn *sql.Conn) error {
	return execAll(ctx, conn,
		`CREATE TABLE IF NOT EXISTS swarm_semantic_heads (
 parent_id BLOB NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
 kind TEXT NOT NULL, id TEXT NOT NULL, stamp INTEGER NOT NULL CHECK(stamp>=0),
 present INTEGER NOT NULL CHECK(present IN (0,1)), controlled INTEGER NOT NULL CHECK(controlled IN (0,1)), value BLOB NOT NULL,
 PRIMARY KEY(parent_id,kind,id)) STRICT, WITHOUT ROWID`,
		`CREATE TABLE IF NOT EXISTS swarm_semantic_history (
 parent_id BLOB NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
 kind TEXT NOT NULL, id TEXT NOT NULL, stamp INTEGER NOT NULL CHECK(stamp>=0),
 present INTEGER NOT NULL CHECK(present IN (0,1)), value BLOB NOT NULL,
 PRIMARY KEY(parent_id,kind,id,stamp)) STRICT, WITHOUT ROWID`)
}

func (s sqliteDerivedStore) semanticHead(key RecordKey) error {
	source := semanticSource{store: s, parent: hex.EncodeToString(s.parent)}
	value, controlled, err := source.news(key)
	if err != nil {
		return err
	}
	present := value != nil
	if value == nil {
		value = []byte{}
	}
	var old []byte
	var wasPresent, wasControlled bool
	nativeSQL(s.ctx)
	err = s.conn.QueryRowContext(s.ctx, `SELECT present,controlled,value FROM swarm_semantic_heads WHERE parent_id=? AND kind=? AND id=?`, s.parent, key.Kind, key.ID).Scan(&wasPresent, &wasControlled, &old)
	found := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if found {
		nativeRows(s.ctx, 1)
		nativePayload(s.ctx, old)
	}
	if !found && !present || found && present == wasPresent && controlled == wasControlled && bytes.Equal(value, old) {
		return nil
	}
	nativeSQL(s.ctx)
	_, err = s.conn.ExecContext(s.ctx, `INSERT INTO swarm_semantic_heads(parent_id,kind,id,stamp,present,controlled,value) VALUES(?,?,?,?,?,?,?) ON CONFLICT(parent_id,kind,id) DO UPDATE SET stamp=excluded.stamp,present=excluded.present,controlled=excluded.controlled,value=excluded.value`, s.parent, key.Kind, key.ID, s.stamp, present, controlled, value)
	if err != nil {
		return err
	}
	if !found || wasPresent != present || !bytes.Equal(value, old) {
		// Multiple source writes in one legacy callback share a stamp and are
		// unobservable until commit. Native Put/read/Put has distinct stamps.
		nativeSQL(s.ctx)
		if _, err = s.conn.ExecContext(s.ctx, `INSERT INTO swarm_semantic_history(parent_id,kind,id,stamp,present,value) VALUES(?,?,?,?,?,?) ON CONFLICT(parent_id,kind,id,stamp) DO UPDATE SET present=excluded.present,value=excluded.value`, s.parent, key.Kind, key.ID, s.stamp, present, value); err != nil {
			return err
		}
	}
	return s.revision(coordindex.Relation{Index: coordindex.SemanticNews}, 0)
}

func (r *sqliteRecordTx) semanticToken() (semanticToken, error) {
	if err := r.ready(); err != nil {
		return semanticToken{}, err
	}
	invalid, err := r.statusStore().total("invalid-workflow", "")
	if err != nil {
		return semanticToken{}, err
	}
	if invalid != 0 {
		return semanticToken{}, fmt.Errorf("%w: missing workflow header or noncontiguous steps", ErrCoordinationFormat)
	}
	// Semantic reads never repair or create history. created_ns is the family's
	// durable history epoch: it survives promotion and changes on recreation.
	var created int64
	nativeSQL(r.ctx)
	if err := r.conn.QueryRowContext(r.ctx, `SELECT created_ns FROM sessions WHERE id=?`, r.parent).Scan(&created); err != nil {
		return semanticToken{}, err
	}
	nativeRows(r.ctx, 1)
	if r.format != 2 {
		var exists bool
		nativeSQL(r.ctx)
		if err := r.conn.QueryRowContext(r.ctx, `SELECT EXISTS(SELECT 1 FROM swarm_semantic_heads WHERE parent_id=?)`, r.parent).Scan(&exists); err != nil {
			return semanticToken{}, err
		}
		if exists || r.format != 0 {
			return semanticToken{}, fmt.Errorf("%w: semantic rules require format 2", ErrCoordinationFormat)
		}
	}
	return semanticToken{Root: r.identity.ParentID, Version: coordsem.Version, Epoch: strconv.FormatInt(created, 10), Stamp: r.stamp}, nil
}
func (*sqliteRecordTx) SemanticCapabilities() CoordinationSemanticCapabilities {
	return CoordinationSemanticCapabilities{NativeHistory: true}
}
func (r *sqliteRecordTx) SemanticMark() (SemanticMark, error) {
	token, err := r.semanticToken()
	if err != nil {
		return "", err
	}
	return SemanticMark(semanticEncode(token)), nil
}
func (r *sqliteRecordTx) SemanticChanges(q SemanticComparison) (SemanticChanges, error) {
	current, err := r.semanticToken()
	if err != nil {
		return SemanticChanges{}, err
	}
	baseline, err := semanticPosition(q.Baseline, current)
	if err != nil {
		return SemanticChanges{}, err
	}
	after, err := semanticAfter(q, current, baseline)
	if err != nil {
		return SemanticChanges{}, err
	}
	out := SemanticChanges{Observed: SemanticMark(semanticEncode(current))}
	// Head changes include control changes, even when the canonical value did
	// not change. The predecessor lookup is always at the ORIGINAL baseline.
	var lastKind, lastID string
	lastStamp := after
	for {
		type head struct {
			key                 RecordKey
			stamp               int64
			present, controlled bool
			value               []byte
		}
		var batch []head
		nativeSQL(r.ctx)
		err = queryEach(r.ctx, r.conn, `SELECT kind,id,stamp,present,controlled,value FROM swarm_semantic_heads WHERE parent_id=? AND stamp>? AND (stamp,kind,id)>(?,?,?) ORDER BY stamp,kind,id LIMIT 128`, []any{r.parent, after, lastStamp, lastKind, lastID}, func(rows *sql.Rows) error {
			var h head
			if err := rows.Scan(&h.key.Kind, &h.key.ID, &h.stamp, &h.present, &h.controlled, &h.value); err != nil {
				return err
			}
			nativeRows(r.ctx, 1)
			nativePayload(r.ctx, h.value)
			batch = append(batch, h)
			return nil
		})
		if err != nil {
			return SemanticChanges{}, err
		}
		for _, h := range batch {
			// A continuation excludes the entire observation stamp, including
			// lexically later IDs from that stamp on the first page.
			if h.stamp <= after {
				continue
			}
			if q.Mode == SemanticParentNews && (!h.present || h.controlled) {
				continue
			}
			var old []byte
			var present bool
			nativeSQL(r.ctx)
			err = r.conn.QueryRowContext(r.ctx, `SELECT present,value FROM swarm_semantic_history WHERE parent_id=? AND kind=? AND id=? AND stamp<=? ORDER BY stamp DESC LIMIT 1`, r.parent, h.key.Kind, h.key.ID, baseline.Stamp).Scan(&present, &old)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return SemanticChanges{}, err
			}
			if err == nil {
				nativeRows(r.ctx, 1)
				nativePayload(r.ctx, old)
			}
			if h.present != present || h.present && !bytes.Equal(h.value, old) {
				out.Different = true
				return out, nil
			}
		}
		if len(batch) < 128 {
			break
		}
		last := batch[len(batch)-1]
		lastStamp, lastKind, lastID = last.stamp, last.key.Kind, last.key.ID
	}
	out.Continue = semanticEncode(semanticContinuation{q.Baseline, q.Mode, out.Observed})
	return out, nil
}

// Backfill after base relations exist, retaining at most one keyset page. The
// migration transaction owns both history and the schema version change.
func backfillSemanticHistory(ctx context.Context, conn *sql.Conn) error {
	var parent []byte
	kind, id := "", ""
	for {
		type entry struct {
			parent []byte
			key    RecordKey
			stamp  int64
		}
		var batch []entry
		where, args := "", []any{}
		if parent != nil {
			where = " AND (r.parent_id,r.kind,r.id)>(?,?,?)"
			args = []any{parent, kind, id}
		}
		err := queryEach(ctx, conn, `SELECT r.parent_id,r.kind,r.id,s.coordination_stamp FROM swarm_records r JOIN sessions s ON s.id=r.parent_id WHERE r.kind IN ('task','member','workflow')`+where+` ORDER BY r.parent_id,r.kind,r.id LIMIT 128`, args, func(rows *sql.Rows) error {
			var v entry
			if err := rows.Scan(&v.parent, &v.key.Kind, &v.key.ID, &v.stamp); err != nil {
				return err
			}
			batch = append(batch, v)
			return nil
		})
		if err != nil {
			return err
		}
		for _, v := range batch {
			store := sqliteDerivedStore{ctx: ctx, conn: conn, parent: v.parent, stamp: v.stamp}
			if err := store.semanticHead(v.key); err != nil {
				return err
			}
		}
		if len(batch) < 128 {
			return nil
		}
		last := batch[len(batch)-1]
		parent, kind, id = last.parent, last.key.Kind, last.key.ID
	}
}

// Exposing the optional reader does not expose Put through a read callback.
type sqliteSemanticReader struct {
	CoordinationReader
	CoordinationSemanticReader
	CoordinationStatusReader
	CoordinationDisplayReader
	CoordinationUsageReader
}

func readOnlySQLiteRecords(r *sqliteRecordTx) CoordinationReader {
	return sqliteSemanticReader{r, r, r, r, r}
}
