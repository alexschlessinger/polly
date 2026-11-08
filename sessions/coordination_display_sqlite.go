package sessions

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/alexschlessinger/pollytool/internal/coordindex"
)

type displayToken struct {
	Root, Epoch, Nonce string
	Version            int
	Stamp              int64
}

func createDisplaySchema(ctx context.Context, conn *sql.Conn) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS swarm_display_marks(parent_id BLOB NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,stamp INTEGER NOT NULL CHECK(stamp>=0),nonce TEXT NOT NULL,PRIMARY KEY(parent_id,stamp)) STRICT, WITHOUT ROWID`,
		`CREATE TABLE IF NOT EXISTS swarm_display_sources(parent_id BLOB NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,kind TEXT NOT NULL,id TEXT NOT NULL,subject TEXT NOT NULL,sort_key BLOB NOT NULL,value BLOB NOT NULL,PRIMARY KEY(parent_id,kind,id)) STRICT, WITHOUT ROWID`,
		`CREATE TABLE IF NOT EXISTS swarm_display_facts(parent_id BLOB NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,kind TEXT NOT NULL,id TEXT NOT NULL,stamp INTEGER NOT NULL CHECK(stamp>=0),value BLOB,PRIMARY KEY(parent_id,kind,id)) STRICT, WITHOUT ROWID`,
		`CREATE TABLE IF NOT EXISTS swarm_display_heads(parent_id BLOB NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,stamp INTEGER NOT NULL CHECK(stamp>=0),PRIMARY KEY(parent_id)) STRICT, WITHOUT ROWID`,
		`CREATE TABLE IF NOT EXISTS swarm_display_decisions(parent_id BLOB NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,kind TEXT NOT NULL,id TEXT NOT NULL,member TEXT NOT NULL,workflow TEXT NOT NULL,sort_key BLOB NOT NULL,value BLOB NOT NULL,PRIMARY KEY(parent_id,kind,id)) STRICT, WITHOUT ROWID`,
	}
	return execAll(ctx, conn, statements...)
}

func (s sqliteDerivedStore) displaySource(key RecordKey) (json.RawMessage, error) {
	var value []byte
	nativeSQL(s.ctx)
	err := s.conn.QueryRowContext(s.ctx, `SELECT value FROM swarm_display_sources WHERE parent_id=? AND kind=? AND id=?`, s.parent, key.Kind, key.ID).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err == nil {
		nativeRows(s.ctx, 1)
		nativePayload(s.ctx, value)
	}
	return value, err
}
func (s sqliteDerivedStore) displaySave(key RecordKey, value any) error {
	var fresh []byte
	var err error
	if value != nil {
		fresh, err = json.Marshal(value)
		if err != nil {
			return err
		}
	}
	var old []byte
	nativeSQL(s.ctx)
	err = s.conn.QueryRowContext(s.ctx, `SELECT value FROM swarm_display_facts WHERE parent_id=? AND kind=? AND id=?`, s.parent, key.Kind, key.ID).Scan(&old)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		nativeRows(s.ctx, 1)
		nativePayload(s.ctx, old)
	}
	if bytes.Equal(old, fresh) {
		return nil
	}
	nativeSQL(s.ctx)
	if _, err = s.conn.ExecContext(s.ctx, `INSERT INTO swarm_display_facts(parent_id,kind,id,stamp,value) VALUES(?,?,?,?,?) ON CONFLICT(parent_id,kind,id) DO UPDATE SET stamp=excluded.stamp,value=excluded.value`, s.parent, key.Kind, key.ID, s.stamp, fresh); err != nil {
		return err
	}
	nativeSQL(s.ctx)
	if _, err = s.conn.ExecContext(s.ctx, `INSERT OR IGNORE INTO swarm_display_marks(parent_id,stamp,nonce) VALUES(?,?,?)`, s.parent, s.stamp, rand.Text()); err != nil {
		return err
	}
	nativeSQL(s.ctx)
	if _, err = s.conn.ExecContext(s.ctx, `INSERT INTO swarm_display_heads(parent_id,stamp) VALUES(?,?) ON CONFLICT(parent_id) DO UPDATE SET stamp=excluded.stamp`, s.parent, s.stamp); err != nil {
		return err
	}
	if err = s.revision(coordindex.Relation{Index: coordindex.Display}, 0); err != nil {
		return err
	}
	return s.revision(coordindex.Relation{Index: coordindex.DisplayFact, Key: coordindex.Key(key.Kind, key.ID)}, 0)
}
func (s sqliteDerivedStore) displaySourceChange(key RecordKey, payload []byte) error {
	compact, err := CompactDisplayRecord(key.Kind, payload)
	if err != nil {
		return err
	}
	if compact == nil && payload != nil {
		return nil
	}
	var v struct{ Member, From, To, Kind string }
	if compact != nil {
		if err = json.Unmarshal(compact, &v); err != nil {
			return err
		}
	}
	save := func(kind, subject string, value []byte, order []byte) error {
		nativeSQL(s.ctx)
		if value == nil {
			_, err = s.conn.ExecContext(s.ctx, `DELETE FROM swarm_display_sources WHERE parent_id=? AND kind=? AND id=?`, s.parent, kind, key.ID)
		} else {
			_, err = s.conn.ExecContext(s.ctx, `INSERT INTO swarm_display_sources(parent_id,kind,id,subject,sort_key,value) VALUES(?,?,?,?,?,?) ON CONFLICT(parent_id,kind,id) DO UPDATE SET subject=excluded.subject,sort_key=excluded.sort_key,value=excluded.value`, s.parent, kind, key.ID, subject, order, value)
		}
		return err
	}
	order := recordOrderKey(coordindex.Order{Kind: key.Kind, ID: key.ID})
	subject := ""
	if compact != nil {
		_, position, e := coordindex.Extract(key.Kind, key.ID, payload)
		if e != nil {
			return e
		}
		order = recordOrderKey(position)
		switch key.Kind {
		case "execution":
			subject = v.Member
		case "mail":
			subject = v.From
		case "workflow_step":
			subject = position.Workflow
		}
	}
	if key.Kind == "mail" && v.Kind == "info" {
		compact = nil
	}
	if err = save(key.Kind, subject, compact, order); err != nil {
		return err
	}
	if key.Kind == "mail" {
		return save("mail_to", v.To, compact, order)
	}
	return nil
}

func (r *sqliteRecordTx) DisplayChanges(mark string) (out DisplayChanges, err error) {
	token, err := r.semanticToken()
	if err != nil {
		return out, err
	}
	current := displayToken{Root: token.Root, Epoch: token.Epoch, Version: 1}
	nativeSQL(r.ctx)
	err = r.conn.QueryRowContext(r.ctx, `SELECT h.stamp,m.nonce FROM swarm_display_heads h JOIN swarm_display_marks m ON m.parent_id=h.parent_id AND m.stamp=h.stamp WHERE h.parent_id=?`, r.parent).Scan(&current.Stamp, &current.Nonce)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	if err != nil {
		return out, err
	}
	nativeRows(r.ctx, 1)
	var old displayToken
	if mark == "" || semanticDecode(mark, &old) != nil || old.Root != current.Root || old.Epoch != current.Epoch || old.Version != current.Version || old.Stamp > current.Stamp {
		out.Reset = true
	}
	out.Mark = semanticEncode(current)
	if !out.Reset && current == old {
		return out, nil
	}
	if !out.Reset {
		// A mark observed in a rolled-back callback must not alias a later write
		// that happens to reuse the same integer stamp (or skip its changes).
		var nonce string
		nativeSQL(r.ctx)
		err = r.conn.QueryRowContext(r.ctx, `SELECT nonce FROM swarm_display_marks WHERE parent_id=? AND stamp=?`, r.parent, old.Stamp).Scan(&nonce)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return out, err
		}
		out.Reset = errors.Is(err, sql.ErrNoRows) || nonce != old.Nonce
	}
	where, index, order := "value IS NOT NULL", "swarm_display_facts_present_idx", "kind,id"
	args := []any{r.parent}
	if !out.Reset {
		where, index, order = "stamp>?", "swarm_display_facts_changed_idx", "stamp,kind,id"
		args = append(args, old.Stamp)
	}
	nativeSQL(r.ctx)
	err = queryEach(r.ctx, r.conn, `SELECT kind,id,value FROM swarm_display_facts INDEXED BY `+index+` WHERE parent_id=? AND `+where+` ORDER BY `+order, args, func(rows *sql.Rows) error {
		var v DisplayRecord
		var value []byte
		if err := rows.Scan(&v.Kind, &v.ID, &value); err != nil {
			return err
		}
		v.Value = value
		nativeRows(r.ctx, 1)
		nativePayload(r.ctx, v.Value)
		out.Records = append(out.Records, v)
		return nil
	})
	return out, err
}

func (s sqliteDerivedStore) displayHeader() error {
	// Header computation is a writer-side operation. A workflow's header and
	// ordered steps may be temporarily incomplete inside a multi-Put callback;
	// only readers validate that shape, never intermediate writer projections.
	var format int64
	nativeSQL(s.ctx)
	if err := s.conn.QueryRowContext(s.ctx, `SELECT coordination_format FROM sessions WHERE id=?`, s.parent).Scan(&format); err != nil {
		return err
	}
	r := &sqliteRecordTx{ctx: s.ctx, conn: s.conn, parent: s.parent, identity: CoordinationIdentity{ParentID: fmt.Sprintf("%x", s.parent)}, active: true, stamp: s.stamp, format: format, displayScalars: true}
	h, err := r.semanticStatusHeader(r.identity.ParentID, false)
	if err != nil {
		return err
	}
	out := DisplayHeader{Counts: h.Counts, HasRuns: h.HasRuns, ParentDisposition: h.ParentDisposition, ColdCompatibility: h.ColdCompatibility}
	out.First, err = r.statusStore().firstItem("decisions", "")
	if err != nil {
		return err
	}
	p, err := s.displaySource(RecordKey{"parent_turn", r.identity.ParentID})
	if err != nil {
		return err
	}
	if p != nil {
		var turn struct{ RetryPause json.RawMessage }
		if err = json.Unmarshal(p, &turn); err != nil {
			return err
		}
		out.RetryPause = turn.RetryPause
	}
	return s.displaySave(RecordKey{"status", "parent"}, out)
}
