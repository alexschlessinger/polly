package sessions

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/alexschlessinger/pollytool/internal/coordindex"
)

type sqliteDerivedStore struct {
	ctx            context.Context
	conn           *sql.Conn
	parent         []byte
	stamp          int64
	changed        map[RevisionScope]bool
	displayDirty   map[RecordKey]bool
	displayScalars bool
}

func (s sqliteDerivedStore) get(key RecordKey) (json.RawMessage, error) {
	var b []byte
	nativeSQL(s.ctx)
	err := s.conn.QueryRowContext(s.ctx, `SELECT payload_json FROM swarm_records WHERE parent_id=? AND kind=? AND id=?`, s.parent, key.Kind, key.ID).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err == nil {
		nativeRows(s.ctx, 1)
		nativePayload(s.ctx, b)
	}
	return b, err
}
func (s sqliteDerivedStore) scan(index coordindex.Index, key, kind string, after *coordindex.Order, limit int) ([]derivedItem, error) {
	where := `parent_id=? AND index_name=? AND index_key=?`
	args := []any{s.parent, string(index), key}
	if kind != "" {
		where += ` AND kind=?`
		args = append(args, kind)
	}
	if after != nil {
		where += ` AND sort_key>?`
		args = append(args, recordOrderKey(*after))
	}
	args = append(args, limit)
	var out []derivedItem
	nativeSQL(s.ctx)
	err := queryEach(s.ctx, s.conn, `SELECT kind,id,sort_key FROM swarm_relations WHERE `+where+` ORDER BY sort_key LIMIT ?`, args, func(rows *sql.Rows) error {
		var item derivedItem
		var order []byte
		if err := rows.Scan(&item.key.Kind, &item.key.ID, &order); err != nil {
			return err
		}
		nativeRows(s.ctx, 1)
		position, err := decodeRecordOrderKey(order)
		if err != nil {
			return err
		}
		item.order = position
		out = append(out, item)
		return nil
	})
	return out, err
}
func (s sqliteDerivedStore) links(key RecordKey, index coordindex.Index) ([]coordindex.Relation, error) {
	var out []coordindex.Relation
	nativeSQL(s.ctx)
	err := queryEach(s.ctx, s.conn, `SELECT index_key FROM swarm_relations WHERE parent_id=? AND kind=? AND id=? AND index_name=?`, []any{s.parent, key.Kind, key.ID, string(index)}, func(rows *sql.Rows) error {
		rel := coordindex.Relation{Index: index}
		if err := rows.Scan(&rel.Key); err != nil {
			return err
		}
		nativeRows(s.ctx, 1)
		out = append(out, rel)
		return nil
	})
	return out, err
}
func (s sqliteDerivedStore) revision(rel coordindex.Relation, delta int) error {
	nativeSQL(s.ctx)
	_, err := s.conn.ExecContext(s.ctx, `INSERT INTO swarm_revisions(parent_id,index_name,index_key,stamp,record_count) VALUES(?,?,?,?,?) ON CONFLICT(parent_id,index_name,index_key) DO UPDATE SET stamp=excluded.stamp,record_count=swarm_revisions.record_count+?`, s.parent, string(rel.Index), rel.Key, s.stamp, max(delta, 0), delta)
	if err == nil && s.changed != nil && (isDerivedSelector(CoordinationIndex(rel.Index)) || isRevisionOnlyCoordinationIndex(CoordinationIndex(rel.Index))) {
		s.changed[RevisionScope{CoordinationIndex(rel.Index), rel.Key}] = true
	}
	if err == nil && rel.Index == semanticPendingMail && delta != 0 {
		return s.revision(coordindex.Relation{Index: coordindex.SemanticMail, Key: rel.Key}, 0)
	}
	return err
}
func (s sqliteDerivedStore) set(item derivedItem, rel coordindex.Relation, present bool) error {
	// The reverse covering index includes the relation PK suffix. An exact link
	// change seeks parent/kind/id/index/key, never scans a reader's receipt history.
	nativeSQL(s.ctx)
	var old []byte
	err := s.conn.QueryRowContext(s.ctx, `SELECT sort_key FROM swarm_relations WHERE parent_id=? AND kind=? AND id=? AND index_name=? AND index_key=?`, s.parent, item.key.Kind, item.key.ID, string(rel.Index), rel.Key).Scan(&old)
	found := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if found {
		nativeRows(s.ctx, 1)
	}
	fresh := recordOrderKey(item.order)
	if present && found && bytes.Equal(old, fresh) || !present && !found {
		return nil
	}
	delta := 0
	if found {
		nativeSQL(s.ctx)
		if _, err = s.conn.ExecContext(s.ctx, `DELETE FROM swarm_relations WHERE parent_id=? AND kind=? AND id=? AND index_name=? AND index_key=?`, s.parent, item.key.Kind, item.key.ID, string(rel.Index), rel.Key); err != nil {
			return err
		}
		delta--
	}
	if present {
		nativeSQL(s.ctx)
		if _, err = s.conn.ExecContext(s.ctx, `INSERT INTO swarm_relations(parent_id,index_name,index_key,sort_key,kind,id) VALUES(?,?,?,?,?,?)`, s.parent, string(rel.Index), rel.Key, fresh, item.key.Kind, item.key.ID); err != nil {
			return err
		}
		delta++
	}
	return s.revision(rel, delta)
}

func indexedRecordChange(ctx context.Context, conn *sql.Conn, parent []byte, key RecordKey, after []byte, stamp int64, changed map[RevisionScope]bool) error {
	return indexedRecordChangeMode(ctx, conn, parent, key, after, stamp, changed, true)
}

// Migration builds base selectors first and derives semantic facts in ordered
// bounded batches afterwards, rather than retaining a family-sized dirty set.
func indexedRecordChangeMode(ctx context.Context, conn *sql.Conn, parent []byte, key RecordKey, after []byte, stamp int64, changed map[RevisionScope]bool, semantics bool) error {
	store := sqliteDerivedStore{ctx: ctx, conn: conn, parent: parent, stamp: stamp, changed: changed, displayDirty: map[RecordKey]bool{}}
	d := derivedEngine{store: store, parent: hex.EncodeToString(parent)}
	before, err := store.get(key)
	if err != nil {
		return err
	}
	status := statusStore{store}
	var statusBefore statusGates
	statusAffected := map[RecordKey]bool{}
	statusChanged := false
	if semantics {
		statusChanged, err = statusInputsChanged(key, before, after)
		if err != nil {
			return err
		}
	}
	if statusChanged {
		statusBefore, err = status.gates()
		if err != nil {
			return err
		}
		if err = status.affected(key, statusAffected); err != nil {
			return err
		}
	}
	semantic := semanticSource{store: store, parent: d.parent}
	affected := map[RecordKey]bool{}
	if semantics {
		if err = semantic.newsAffected(key, before, affected); err != nil {
			return err
		}
	}
	pubs, groups := map[string]bool{}, map[string]bool{}
	readers := map[string]*coordindex.PublicationReaderState{}
	switch key.Kind {
	case "publication":
		pubs[key.ID] = true
		for _, b := range [][]byte{before, after} {
			if b != nil {
				var p coordindex.Publication
				if err = json.Unmarshal(b, &p); err != nil {
					return err
				}
				if p.Supersedes != "" {
					pubs[p.Supersedes] = true
				}
			}
		}
		links, err := store.links(key, coordindex.PublicationEquivalent)
		if err != nil {
			return err
		}
		for _, rel := range links {
			groups[rel.Key] = true
		}
	case "snapshot":
		if err = d.each(coordindex.PublicationSnapshot, key.ID, "publication", func(item derivedItem) error { pubs[item.key.ID] = true; return nil }); err != nil {
			return err
		}
	case "member", "parent":
		readers[key.ID], err = d.reader(key.ID)
		if err != nil {
			return err
		}
	case "execution":
		if err = d.each(coordindex.OverviewDependency, coordindex.Key("execution", key.ID), "member", func(item derivedItem) error {
			var err error
			readers[item.key.ID], err = d.reader(item.key.ID)
			return err
		}); err != nil {
			return err
		}
	}
	// Derived relations survive ordinary payload replacement. A real payload
	// change invalidates its existing selector scopes even if membership stays
	// unchanged; pure dependency refreshes do not invalidate unchanged results.
	// Deletions remove links explicitly so counts and tombstones stay truthful.
	if before != nil {
		nativeSQL(ctx)
		var links []coordindex.Relation
		if err = queryEach(ctx, conn, `SELECT index_name,index_key FROM swarm_relations WHERE parent_id=? AND kind=? AND id=? AND index_name LIKE 'derived.%'`, []any{parent, key.Kind, key.ID}, func(rows *sql.Rows) error {
			var rel coordindex.Relation
			if err := rows.Scan(&rel.Index, &rel.Key); err != nil {
				return err
			}
			nativeRows(ctx, 1)
			links = append(links, rel)
			return nil
		}); err != nil {
			return err
		}
		for _, rel := range links {
			if after == nil {
				err = store.set(derivedItem{key: key}, rel, false)
			} else {
				err = store.revision(rel, 0)
			}
			if err != nil {
				return err
			}
		}
	}
	if err = indexedBaseRecordChange(ctx, conn, parent, key, after, stamp, changed); err != nil {
		return err
	}
	if key.Kind == "mail" || key.Kind == "followup" {
		if err = semantic.pendingMail(key.ID); err != nil {
			return err
		}
	}
	if semantics {
		if err = semantic.newsAffected(key, after, affected); err != nil {
			return err
		}
		for item := range affected {
			if err = store.semanticHead(item); err != nil {
				return err
			}
		}
	}
	if len(pubs) > 0 {
		if err = d.publications(pubs, groups); err != nil {
			return err
		}
	}
	for id, old := range readers {
		if err = d.readerChanged(id, old); err != nil {
			return err
		}
		if err = d.memberLifecycle(id); err != nil {
			return err
		}
	}
	news, err := derivedNewsChanged(key.Kind, before, after)
	if err != nil {
		return err
	}
	if news {
		if err = store.revision(coordindex.Relation{Index: coordindex.AdmissionNews, Key: coordindex.Key(key.Kind, key.ID)}, 0); err != nil {
			return err
		}
	}
	if !semantics {
		return nil
	}
	if statusChanged {
		if err = status.affected(key, statusAffected); err != nil {
			return err
		}
		if err = status.refresh(statusAffected, statusBefore); err != nil {
			return err
		}
	}
	return store.refreshDisplay(key, before, after, statusAffected)
}

// Decode only the persisted ordering key, not the publication payload, when
// walking dependency/index pages. Encoding is the inverse of recordOrderKey.
func decodeRecordOrderKey(b []byte) (coordindex.Order, error) {
	readText := func() (string, error) {
		var out []byte
		for len(b) > 0 {
			v := b[0]
			b = b[1:]
			if v != 0 {
				out = append(out, v)
				continue
			}
			if len(b) == 0 {
				break
			}
			next := b[0]
			b = b[1:]
			if next == 0 {
				return string(out), nil
			}
			if next != 255 {
				break
			}
			out = append(out, 0)
		}
		return "", fmt.Errorf("invalid coordination order key")
	}
	var out coordindex.Order
	var err error
	if out.Kind, err = readText(); err != nil {
		return out, err
	}
	readNumber := func() (int64, error) {
		if len(b) < 8 {
			return 0, fmt.Errorf("invalid coordination order key")
		}
		n := int64(binary.BigEndian.Uint64(b[:8]) ^ (uint64(1) << 63))
		b = b[8:]
		return n, nil
	}
	switch out.Kind {
	case "publication", "mail":
		sec, err := readNumber()
		if err != nil {
			return out, err
		}
		nano, err := readNumber()
		if err != nil {
			return out, err
		}
		out.Posted = time.Unix(sec, nano).UTC()
	case "workflow_step":
		if out.Workflow, err = readText(); err != nil {
			return out, err
		}
		if out.Step, err = readNumber(); err != nil {
			return out, err
		}
	}
	if out.ID, err = readText(); err != nil {
		return out, err
	}
	if len(b) != 0 {
		return out, fmt.Errorf("invalid coordination order suffix")
	}
	return out, nil
}
