package sessions

import (
	"context"
	"database/sql"
	"encoding/json"
)

// Migrations retain at most 128 descriptors and one selected source payload.
// All passes run inside the schema transaction; no half-built display is ever
// visible, and healthy schema-10 opens neither populate nor repair these facts.
func backfillDisplayFacts(ctx context.Context, conn *sql.Conn) error {
	each := func(table, kind string, fn func(sqliteDerivedStore, RecordKey) error) error {
		var parent []byte
		id := ""
		for {
			type entry struct {
				parent []byte
				id     string
				stamp  int64
			}
			var batch []entry
			where, args := "", []any{kind}
			if parent != nil {
				where = ` AND (r.parent_id,r.id)>(?,?)`
				args = append(args, parent, id)
			}
			err := queryEach(ctx, conn, `SELECT r.parent_id,r.id,s.coordination_stamp FROM `+table+` r JOIN sessions s ON s.id=r.parent_id WHERE r.kind=?`+where+` ORDER BY r.parent_id,r.id LIMIT 128`, args, func(rows *sql.Rows) error {
				var v entry
				if err := rows.Scan(&v.parent, &v.id, &v.stamp); err != nil {
					return err
				}
				batch = append(batch, v)
				return nil
			})
			if err != nil {
				return err
			}
			for _, v := range batch {
				s := sqliteDerivedStore{ctx: ctx, conn: conn, parent: v.parent, stamp: v.stamp}
				if err = fn(s, RecordKey{kind, v.id}); err != nil {
					return err
				}
			}
			if len(batch) < 128 {
				return nil
			}
			last := batch[len(batch)-1]
			parent, id = last.parent, last.id
		}
	}
	for _, kind := range []string{"member", "execution", "task", "workflow", "workflow_step", "mail", "parent_turn"} {
		if err := each("swarm_records", kind, func(s sqliteDerivedStore, k RecordKey) error {
			value, err := s.get(k)
			if err != nil {
				return err
			}
			return s.displaySourceChange(k, value)
		}); err != nil {
			return err
		}
	}
	for _, kind := range []string{"task", "mail", "workflow"} {
		if err := each("swarm_status_facts", kind, func(s sqliteDerivedStore, k RecordKey) error {
			var value []byte
			if err := conn.QueryRowContext(ctx, `SELECT value FROM swarm_status_facts WHERE parent_id=? AND kind=? AND id=?`, s.parent, k.Kind, k.ID).Scan(&value); err != nil {
				return err
			}
			var f statusFact
			if err := json.Unmarshal(value, &f); err != nil {
				return err
			}
			return s.displayDecisions(k, statusFact{}, f)
		}); err != nil {
			return err
		}
	}
	for _, kind := range []string{"member", "workflow", "execution"} {
		if err := each("swarm_display_sources", kind, func(s sqliteDerivedStore, k RecordKey) error {
			switch kind {
			case "member":
				return s.displayMember(k.ID)
			case "workflow":
				return s.displayWorkflow(k.ID)
			default:
				return s.displayLaunch(k.ID)
			}
		}); err != nil {
			return err
		}
	}
	// Parent-facing requests can name a sender outside the current roster.
	// Keep their annotation keys without inventing a Member source record.
	var decisionParent []byte
	decisionMember := ""
	for {
		type entry struct {
			parent []byte
			member string
			stamp  int64
		}
		var batch []entry
		where, args := "", []any{}
		if decisionParent != nil {
			where = ` AND (d.parent_id,d.member)>(?,?)`
			args = append(args, decisionParent, decisionMember)
		}
		if err := queryEach(ctx, conn, `SELECT d.parent_id,d.member,s.coordination_stamp FROM swarm_display_decisions d JOIN sessions s ON s.id=d.parent_id WHERE d.member<>''`+where+` GROUP BY d.parent_id,d.member ORDER BY d.parent_id,d.member LIMIT 128`, args, func(rows *sql.Rows) error {
			var v entry
			if err := rows.Scan(&v.parent, &v.member, &v.stamp); err != nil {
				return err
			}
			batch = append(batch, v)
			return nil
		}); err != nil {
			return err
		}
		for _, v := range batch {
			if err := (sqliteDerivedStore{ctx: ctx, conn: conn, parent: v.parent, stamp: v.stamp}).displayMember(v.member); err != nil {
				return err
			}
		}
		if len(batch) < 128 {
			break
		}
		last := batch[len(batch)-1]
		decisionParent, decisionMember = last.parent, last.member
	}
	var parent []byte
	for {
		type entry struct {
			parent []byte
			stamp  int64
		}
		var batch []entry
		where, args := "", []any{}
		if parent != nil {
			where = " AND id>?"
			args = append(args, parent)
		}
		if err := queryEach(ctx, conn, `SELECT id,coordination_stamp FROM sessions WHERE parent_id IS NULL`+where+` ORDER BY id LIMIT 128`, args, func(rows *sql.Rows) error {
			var v entry
			if err := rows.Scan(&v.parent, &v.stamp); err != nil {
				return err
			}
			batch = append(batch, v)
			return nil
		}); err != nil {
			return err
		}
		for _, v := range batch {
			if err := (sqliteDerivedStore{ctx: ctx, conn: conn, parent: v.parent, stamp: v.stamp}).displayHeader(); err != nil {
				return err
			}
		}
		if len(batch) < 128 {
			return nil
		}
		parent = batch[len(batch)-1].parent
	}
}
