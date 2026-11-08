package sessions

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/alexschlessinger/pollytool/internal/coordindex"
	"github.com/alexschlessinger/pollytool/internal/coordsem"
)

type statusItem struct {
	Section, Actor string
	Order          []byte
	Decision       *coordsem.DecisionItem
	Working        *coordsem.WorkingItem
}
type statusFact struct {
	Counts map[string]int
	Items  []statusItem
}
type statusStore struct{ sqliteDerivedStore }

func statusCountKey(name, key string) string { return coordindex.Key(name, key) }
func (f *statusFact) add(name, key string) {
	if f.Counts == nil {
		f.Counts = map[string]int{}
	}
	f.Counts[statusCountKey(name, key)]++
}
func (f *statusFact) decision(section, actor string, group byte, order coordindex.Order, v coordsem.DecisionItem) {
	f.Items = append(f.Items, statusItem{Section: section, Actor: actor, Order: append([]byte{group}, recordOrderKey(order)...), Decision: &v})
	f.add(section, actor)
}
func (f *statusFact) working(section string, group byte, id string, v coordsem.WorkingItem) {
	f.Items = append(f.Items, statusItem{Section: section, Order: append([]byte{group}, recordOrderKey(coordindex.Order{Kind: "status", ID: id})...), Working: &v})
	f.add(section, "")
}
func (s statusStore) total(name, key string) (int, error) {
	var n int
	nativeSQL(s.ctx)
	query := `SELECT total FROM swarm_status_totals WHERE parent_id=? AND name=?`
	if s.displayScalars {
		query = `SELECT coalesce((` + query + `),0)`
	}
	err := s.conn.QueryRowContext(s.ctx, query, s.parent, statusCountKey(name, key)).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err == nil {
		nativeRows(s.ctx, 1)
	}
	return n, err
}
func (s statusStore) sourceCount(index coordindex.Index, key string) (int, error) {
	var n int
	nativeSQL(s.ctx)
	query := `SELECT record_count FROM swarm_revisions WHERE parent_id=? AND index_name=? AND index_key=?`
	if s.displayScalars {
		query = `SELECT coalesce((` + query + `),0)`
	}
	err := s.conn.QueryRowContext(s.ctx, query, s.parent, string(index), key).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err == nil {
		nativeRows(s.ctx, 1)
	}
	return n, err
}
func (s statusStore) source() semanticSource {
	return semanticSource{store: s.sqliteDerivedStore, parent: fmt.Sprintf("%x", s.parent)}
}
func (s statusStore) first(index coordindex.Index, key string) (string, error) {
	return (derivedEngine{store: s.sqliteDerivedStore}).first(index, key)
}
func (s statusStore) run() (string, error) {
	a, err := s.first(coordindex.RunStatus, "running")
	if err != nil {
		return "", err
	}
	b, err := s.first(coordindex.RunStatus, "paused")
	if err != nil {
		return "", err
	}
	if a == "" || b != "" && b < a {
		a = b
	}
	return a, nil
}
func (s statusStore) running() (bool, error) {
	for _, status := range []string{"queued", "running"} {
		n, err := s.sourceCount(coordindex.ExecutionStatus, status)
		if err != nil {
			return false, err
		}
		if n > 0 {
			return true, nil
		}
	}
	n, err := s.total("host-running", "")
	return n > 0, err
}
func (s statusStore) save(key RecordKey, f statusFact) error {
	fresh, err := json.Marshal(f)
	if err != nil {
		return err
	}
	var old []byte
	nativeSQL(s.ctx)
	err = s.conn.QueryRowContext(s.ctx, `SELECT value FROM swarm_status_facts WHERE parent_id=? AND kind=? AND id=?`, s.parent, key.Kind, key.ID).Scan(&old)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		nativeRows(s.ctx, 1)
		nativePayload(s.ctx, old)
	}
	if bytes.Equal(old, fresh) || old == nil && len(f.Counts) == 0 && len(f.Items) == 0 {
		return nil
	}
	if s.displayDirty != nil {
		s.displayDirty[RecordKey{"status", "parent"}] = true
	}
	var before statusFact
	if old != nil {
		if err := json.Unmarshal(old, &before); err != nil {
			return err
		}
	}
	if err := s.sqliteDerivedStore.displayDecisions(key, before, f); err != nil {
		return err
	}
	deltas := map[string]int{}
	for k, n := range before.Counts {
		deltas[k] -= n
	}
	for k, n := range f.Counts {
		deltas[k] += n
	}
	for k, n := range deltas {
		if n == 0 {
			continue
		}
		nativeSQL(s.ctx)
		if _, err := s.conn.ExecContext(s.ctx, `INSERT INTO swarm_status_totals(parent_id,name,total) VALUES(?,?,?) ON CONFLICT(parent_id,name) DO UPDATE SET total=total+?`, s.parent, k, max(n, 0), n); err != nil {
			return err
		}
	}
	nativeSQL(s.ctx)
	if _, err := s.conn.ExecContext(s.ctx, `DELETE FROM swarm_status_items WHERE parent_id=? AND kind=? AND id=?`, s.parent, key.Kind, key.ID); err != nil {
		return err
	}
	for _, item := range f.Items {
		var value []byte
		if item.Decision != nil {
			value, err = json.Marshal(item.Decision)
		} else {
			value, err = json.Marshal(item.Working)
		}
		if err != nil {
			return err
		}
		nativeSQL(s.ctx)
		if _, err := s.conn.ExecContext(s.ctx, `INSERT INTO swarm_status_items(parent_id,section,actor,sort_key,kind,id,value) VALUES(?,?,?,?,?,?,?)`, s.parent, item.Section, item.Actor, item.Order, key.Kind, key.ID, value); err != nil {
			return err
		}
	}
	nativeSQL(s.ctx)
	if len(f.Counts) == 0 && len(f.Items) == 0 {
		_, err = s.conn.ExecContext(s.ctx, `DELETE FROM swarm_status_facts WHERE parent_id=? AND kind=? AND id=?`, s.parent, key.Kind, key.ID)
	} else {
		_, err = s.conn.ExecContext(s.ctx, `INSERT INTO swarm_status_facts(parent_id,kind,id,value) VALUES(?,?,?,?) ON CONFLICT(parent_id,kind,id) DO UPDATE SET value=excluded.value`, s.parent, key.Kind, key.ID, fresh)
	}
	return err
}

func createStatusSchema(ctx context.Context, conn *sql.Conn) error {
	return execAll(ctx, conn,
		`CREATE TABLE IF NOT EXISTS swarm_status_facts(parent_id BLOB NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,kind TEXT NOT NULL,id TEXT NOT NULL,value BLOB NOT NULL,PRIMARY KEY(parent_id,kind,id)) STRICT, WITHOUT ROWID`,
		`CREATE TABLE IF NOT EXISTS swarm_status_totals(parent_id BLOB NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,name TEXT NOT NULL,total INTEGER NOT NULL CHECK(total>=0),PRIMARY KEY(parent_id,name)) STRICT, WITHOUT ROWID`,
		`CREATE TABLE IF NOT EXISTS swarm_status_items(parent_id BLOB NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,section TEXT NOT NULL,actor TEXT NOT NULL,sort_key BLOB NOT NULL,kind TEXT NOT NULL,id TEXT NOT NULL,value BLOB NOT NULL,PRIMARY KEY(parent_id,section,actor,sort_key)) STRICT, WITHOUT ROWID`)
}
