package sessions

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/alexschlessinger/pollytool/internal/coordindex"
	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
)

// nativeSQL counts statements dispatched by the native record/page implementation,
// not SQLite VM steps, lease fencing, transaction control or maintenance SQL.
func nativeSQL(ctx context.Context) { runtimemetrics.Add(ctx, runtimemetrics.SQLStatements, 1) }
func nativeRows(ctx context.Context, n int) {
	runtimemetrics.Add(ctx, runtimemetrics.RowsRead, uint64(n))
}
func nativePayload(ctx context.Context, payload []byte) {
	runtimemetrics.Add(ctx, runtimemetrics.PayloadBytesRead, uint64(len(payload)))
}

// orderKey preserves coordindex.Less, including opaque non-UTF-8 keys and times
// outside UnixNano's range. Zero-escaped, terminated strings preserve byte order.
func recordOrderKey(order coordindex.Order) []byte {
	var b []byte
	text := func(s string) {
		for i := 0; i < len(s); i++ {
			if s[i] == 0 {
				b = append(b, 0, 255)
			} else {
				b = append(b, s[i])
			}
		}
		b = append(b, 0, 0)
	}
	number := func(n int64) { b = binary.BigEndian.AppendUint64(b, uint64(n)^(uint64(1)<<63)) }
	text(order.Kind)
	switch order.Kind {
	case "mail", "publication":
		number(order.Posted.Unix())
		number(int64(order.Posted.Nanosecond()))
	case "workflow_step":
		text(order.Workflow)
		number(order.Step)
	}
	text(order.ID)
	return b
}

// indexedBaseRecordChange is shared by native point writes, legacy whole-state
// updates and migration. It changes only record-local relations; the wrapper
// maintains cross-record derived relations without discarding unchanged links.
// A nil after deletes; callers have already compared exact payload bytes.
func indexedBaseRecordChange(ctx context.Context, conn *sql.Conn, parent []byte, key RecordKey, after []byte, stamp int64, changed map[RevisionScope]bool) error {
	old := map[RevisionScope]bool{}
	nativeSQL(ctx)
	err := queryEach(ctx, conn, `SELECT index_name,index_key FROM swarm_relations WHERE parent_id=? AND kind=? AND id=? AND index_name NOT LIKE 'derived.%'`, []any{parent, key.Kind, key.ID}, func(rows *sql.Rows) error {
		var scope RevisionScope
		if err := rows.Scan(&scope.Index, &scope.Key); err != nil {
			return err
		}
		nativeRows(ctx, 1)
		old[scope] = true
		return nil
	})
	if err != nil {
		return err
	}
	fresh := map[RevisionScope]bool{}
	var order coordindex.Order
	if after != nil {
		relations, position, err := coordindex.Extract(key.Kind, key.ID, after)
		runtimemetrics.Add(ctx, runtimemetrics.RecordsDecoded, 1)
		if err != nil {
			return err
		}
		order = position
		for _, r := range relations {
			fresh[RevisionScope{CoordinationIndex(r.Index), r.Key}] = true
		}
	}
	nativeSQL(ctx)
	if _, err = conn.ExecContext(ctx, `DELETE FROM swarm_relations WHERE parent_id=? AND kind=? AND id=? AND index_name NOT LIKE 'derived.%'`, parent, key.Kind, key.ID); err != nil {
		return err
	}
	nativeSQL(ctx)
	if after == nil {
		_, err = conn.ExecContext(ctx, `DELETE FROM swarm_records WHERE parent_id=? AND kind=? AND id=?`, parent, key.Kind, key.ID)
	} else {
		_, err = conn.ExecContext(ctx, `INSERT INTO swarm_records(parent_id,kind,id,payload_json) VALUES(?,?,?,?) ON CONFLICT(parent_id,kind,id) DO UPDATE SET payload_json=excluded.payload_json`, parent, key.Kind, key.ID, after)
	}
	if err != nil {
		return err
	}
	if key == (RecordKey{"format", "swarm"}) {
		var f struct {
			Version int64 `json:"version"`
		}
		if json.Unmarshal(after, &f) != nil {
			f.Version = 0
		}
		nativeSQL(ctx)
		if _, err = conn.ExecContext(ctx, `UPDATE sessions SET coordination_format=? WHERE id=?`, max(f.Version, 0), parent); err != nil {
			return err
		}
	}
	sortKey := recordOrderKey(order)
	for scope := range fresh {
		nativeSQL(ctx)
		if _, err = conn.ExecContext(ctx, `INSERT INTO swarm_relations(parent_id,index_name,index_key,sort_key,kind,id) VALUES(?,?,?,?,?,?)`, parent, string(scope.Index), scope.Key, sortKey, key.Kind, key.ID); err != nil {
			return err
		}
	}
	union := map[RevisionScope]bool{}
	for s := range old {
		union[s] = true
	}
	for s := range fresh {
		union[s] = true
	}
	for scope := range union {
		delta := 0
		if fresh[scope] {
			delta++
		}
		if old[scope] {
			delta--
		}
		nativeSQL(ctx)
		if _, err = conn.ExecContext(ctx, `INSERT INTO swarm_revisions(parent_id,index_name,index_key,stamp,record_count) VALUES(?,?,?,?,?) ON CONFLICT(parent_id,index_name,index_key) DO UPDATE SET stamp=excluded.stamp,record_count=swarm_revisions.record_count+?`, parent, string(scope.Index), scope.Key, stamp, max(delta, 0), delta); err != nil {
			return err
		}
		if changed != nil {
			changed[scope] = true
		}
	}
	return nil
}

func sortedScopes(changed map[RevisionScope]bool) []RevisionScope {
	out := make([]RevisionScope, 0, len(changed))
	for s := range changed {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Index != out[j].Index {
			return out[i].Index < out[j].Index
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// applySchemaV10 upgrades a genuine schema-9 database. A database that already
// carries any part of the schema-10 shape is refused before this runs.
func applySchemaV10(ctx context.Context, conn *sql.Conn) error {
	if err := validateBeforeV10(ctx, conn); err != nil {
		return err
	}
	if err := execAll(ctx, conn,
		`ALTER TABLE sessions ADD COLUMN transcript_epoch INTEGER NOT NULL DEFAULT 0 CHECK(transcript_epoch>=0)`,
		`ALTER TABLE sessions ADD COLUMN coordination_stamp INTEGER NOT NULL DEFAULT 0 CHECK(coordination_stamp>=0)`,
		`ALTER TABLE sessions ADD COLUMN coordination_format INTEGER NOT NULL DEFAULT 0 CHECK(coordination_format>=0)`,
	); err != nil {
		return err
	}
	if err := createSemanticSchema(ctx, conn); err != nil {
		return err
	}
	if err := createStatusSchema(ctx, conn); err != nil {
		return err
	}
	if err := createDisplaySchema(ctx, conn); err != nil {
		return err
	}
	if err := createTranscriptDisplaySchema(ctx, conn); err != nil {
		return err
	}
	if err := execAll(ctx, conn,
		`CREATE TABLE IF NOT EXISTS swarm_relations (
 parent_id BLOB NOT NULL, index_name TEXT NOT NULL, index_key TEXT NOT NULL,
 sort_key BLOB NOT NULL, kind TEXT NOT NULL, id TEXT NOT NULL,
 PRIMARY KEY(parent_id,index_name,index_key,sort_key),
 FOREIGN KEY(parent_id,kind,id) REFERENCES swarm_records(parent_id,kind,id) ON DELETE CASCADE
 ) STRICT, WITHOUT ROWID`,
		`CREATE TABLE IF NOT EXISTS swarm_revisions (
 parent_id BLOB NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
 index_name TEXT NOT NULL, index_key TEXT NOT NULL,
 stamp INTEGER NOT NULL CHECK(stamp>=0), record_count INTEGER NOT NULL CHECK(record_count>=0),
 PRIMARY KEY(parent_id,index_name,index_key)
 ) STRICT, WITHOUT ROWID`,
	); err != nil {
		return err
	}
	return createSchemaV10Indexes(ctx, conn)
}

// backfillDerivedState rebuilds relations for every stored record. Re-running it
// is harmless: a record whose relations already match derives a zero delta, so
// counts and stamps keep their committed values. Keyset batches bound retained
// payloads independently of family size; the caller's transaction owns the whole
// backfill, so any decoding or SQL error rolls back every derived row with it.
func backfillDerivedState(ctx context.Context, conn *sql.Conn) error {
	type row struct {
		parent []byte
		key    RecordKey
	}
	var parent []byte
	var kind, id string
	for {
		var batch []row
		query := `SELECT parent_id,kind,id FROM swarm_records`
		var args []any
		if parent != nil {
			query += ` WHERE (parent_id,kind,id)>(?,?,?)`
			args = []any{parent, kind, id}
		}
		query += ` ORDER BY parent_id,kind,id LIMIT 128`
		if err := queryEach(ctx, conn, query, args, func(rows *sql.Rows) error {
			var r row
			if err := rows.Scan(&r.parent, &r.key.Kind, &r.key.ID); err != nil {
				return err
			}
			batch = append(batch, r)
			return nil
		}); err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}
		for _, r := range batch {
			if _, err := conn.ExecContext(ctx, `UPDATE sessions SET coordination_stamp=max(coordination_stamp,1) WHERE id=?`, r.parent); err != nil {
				return err
			}
			// Fetch and release one payload at a time, including a single
			// arbitrarily oversized historical record.
			var payload []byte
			if err := conn.QueryRowContext(ctx, `SELECT payload_json FROM swarm_records WHERE parent_id=? AND kind=? AND id=?`, r.parent, r.key.Kind, r.key.ID).Scan(&payload); err != nil {
				return err
			}
			if err := indexedRecordChangeMode(ctx, conn, r.parent, r.key, payload, 1, nil, false); err != nil {
				return fmt.Errorf("backfill %s/%s: %w", r.key.Kind, r.key.ID, err)
			}
		}
		last := batch[len(batch)-1]
		parent = bytes.Clone(last.parent)
		kind, id = last.key.Kind, last.key.ID
	}
}
