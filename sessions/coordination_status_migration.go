package sessions

import (
	"context"
	"database/sql"
)

// Base relations are complete before this pass. Primitive contributions precede
// their consumers; each pass retains at most 128 keys and one selected item's
// actual dependency component. No migration-sized dirty set is constructed.
func backfillStatusFacts(ctx context.Context, conn *sql.Conn) error {
	for _, kind := range []string{"execution", "mail", "workflow_step", "workflow", "apply", "task", "member", "run"} {
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
			err := queryEach(ctx, conn, `SELECT r.parent_id,r.id,s.coordination_stamp FROM swarm_records r JOIN sessions s ON s.id=r.parent_id WHERE r.kind=?`+where+` ORDER BY r.parent_id,r.id LIMIT 128`, args, func(rows *sql.Rows) error {
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
				s := statusStore{sqliteDerivedStore{ctx: ctx, conn: conn, parent: v.parent, stamp: v.stamp}}
				key := RecordKey{kind, v.id}
				f, err := s.derive(key)
				if err != nil {
					return err
				}
				if err = s.save(key, f); err != nil {
					return err
				}
				if kind == "run" {
					if err = s.syncBudget(); err != nil {
						return err
					}
				}
			}
			if len(batch) < 128 {
				break
			}
			last := batch[len(batch)-1]
			parent, id = last.parent, last.id
		}
	}
	return nil
}
