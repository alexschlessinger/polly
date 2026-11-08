package sessions

import (
	"database/sql"
	"errors"
)

// The relation primary key is the declared partition order and contains the
// record key, so this reverse seek never visits swarm_records or payloads.
const coordinationLastKeySQL = `SELECT kind,id FROM swarm_relations
WHERE parent_id=? AND index_name=? AND index_key=?
ORDER BY sort_key DESC LIMIT 1`

// The kind index covers the filtered reverse seek, but does not contain id.
// Resolve its single sort key through the relation primary key rather than
// scanning other kinds or loading the record payload. All selectors are bound.
const coordinationLastKindKeySQL = `SELECT kind,id FROM swarm_relations
WHERE parent_id=? AND index_name=? AND index_key=? AND sort_key=(
 SELECT sort_key FROM swarm_relations
 WHERE parent_id=? AND index_name=? AND index_key=? AND kind=?
 ORDER BY sort_key DESC LIMIT 1
)`

func (r *sqliteRecordTx) lastKey(scope RevisionScope, kind string) (*RecordKey, error) {
	query := coordinationLastKeySQL
	args := []any{r.parent, string(scope.Index), scope.Key}
	if kind != "" && kind != selectorKind(scope) {
		query = coordinationLastKindKeySQL
		args = append(args, r.parent, string(scope.Index), scope.Key, kind)
	}
	var key RecordKey
	nativeSQL(r.ctx)
	err := r.conn.QueryRowContext(r.ctx, query, args...).Scan(&key.Kind, &key.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	nativeRows(r.ctx, 1)
	return &key, nil
}
