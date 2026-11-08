package sessions

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
)

// CoordinationIDQuery selects one kind in bytewise ID order, independently of
// its domain ordering (mail and publication ordinarily use posting time).
// After is opaque, root/kind-bound and live across callbacks. The first item
// exceeding MaxBytes is returned intact and alone.
type CoordinationIDQuery struct {
	Kind, After     string
	Limit, MaxBytes int
}

// CoordinationIDReader is an optional observation-only capability. It grants
// neither a lease nor mutation authority.
type CoordinationIDReader interface {
	QueryRecordsByID(CoordinationIDQuery) (RecordPage, error)
}

type coordinationIDCursor struct {
	Version        int
	Root, Kind, ID []byte
}

func idPageCursor(r CoordinationReader, q CoordinationIDQuery) (coordinationIDCursor, error) {
	if q.Kind == "" || q.Limit < 1 || q.Limit > MaxCoordinationPageLimit || q.MaxBytes < 0 {
		return coordinationIDCursor{}, fmt.Errorf("%w: ID page kind, limit and byte budget", ErrCoordinationRequest)
	}
	identity, err := r.Identity()
	if err != nil {
		return coordinationIDCursor{}, err
	}
	cursor := coordinationIDCursor{Version: 1, Root: []byte(identity.ParentID), Kind: []byte(q.Kind)}
	if q.After != "" {
		data, err := base64.RawURLEncoding.DecodeString(q.After)
		var c coordinationIDCursor
		if err != nil || json.Unmarshal(data, &c) != nil || c.Version != cursor.Version || !bytes.Equal(c.Root, cursor.Root) || !bytes.Equal(c.Kind, cursor.Kind) || len(c.ID) == 0 {
			return cursor, ErrCoordinationCursor
		}
		cursor.ID = c.ID
	}
	return cursor, nil
}
func idPageNext(c coordinationIDCursor, id string) string {
	c.ID = []byte(id)
	data, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(data)
}

// QueryRecordsByID uses only the supplied reader. Compatibility is deliberately
// cold: it enumerates and sorts the requested kind without reopening a backend.
func QueryRecordsByID(r CoordinationReader, q CoordinationIDQuery) (RecordPage, error) {
	if native, ok := r.(CoordinationIDReader); ok {
		return native.QueryRecordsByID(q)
	}
	cursor, err := idPageCursor(r, q)
	if err != nil {
		return RecordPage{}, err
	}
	var all []CoordinationRecord
	query := RecordQuery{Index: CoordinationIndexKind, Key: q.Kind, Kind: q.Kind, Limit: MaxCoordinationPageLimit}
	for {
		page, err := r.Query(query)
		if err != nil {
			return RecordPage{}, err
		}
		all = append(all, page.Records...)
		if page.Next == "" {
			break
		}
		if page.Next == query.After {
			return RecordPage{}, ErrCoordinationCursor
		}
		query.After = page.Next
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Key.ID < all[j].Key.ID })
	page := RecordPage{Total: len(all)}
	used := 0
	for _, record := range all {
		if len(cursor.ID) > 0 && record.Key.ID <= string(cursor.ID) {
			continue
		}
		if len(page.Records) == q.Limit || q.MaxBytes > 0 && len(page.Records) > 0 && len(record.Payload) > q.MaxBytes-used {
			page.Next = idPageNext(cursor, page.Records[len(page.Records)-1].Key.ID)
			break
		}
		if q.MaxBytes > 0 && len(record.Payload) > q.MaxBytes-used {
			page.Oversized = true
		}
		page.Records = append(page.Records, record)
		used += len(record.Payload)
	}
	return page, nil
}

func (r sqliteSemanticReader) QueryRecordsByID(q CoordinationIDQuery) (RecordPage, error) {
	return r.CoordinationReader.(CoordinationIDReader).QueryRecordsByID(q)
}
func (r *sqliteRecordTx) QueryRecordsByID(q CoordinationIDQuery) (RecordPage, error) {
	if err := r.ready(); err != nil {
		return RecordPage{}, err
	}
	cursor, err := idPageCursor(r, q)
	if err != nil {
		return RecordPage{}, err
	}
	total, err := r.Query(RecordQuery{Index: CoordinationIndexKind, Key: q.Kind, Kind: q.Kind, CountOnly: true, IncludeTotal: true})
	if err != nil {
		return RecordPage{}, err
	}
	page := RecordPage{Total: total.Total}
	type selectedID struct {
		id   string
		size int
	}
	var selected []selectedID
	where := `parent_id=? AND kind=?`
	args := []any{r.parent, q.Kind}
	if len(cursor.ID) > 0 {
		where += ` AND id>?`
		args = append(args, string(cursor.ID))
	}
	args = append(args, q.Limit+1)
	nativeSQL(r.ctx)
	err = queryEach(r.ctx, r.conn, `SELECT id,length(payload_json) FROM swarm_records WHERE `+where+` ORDER BY id LIMIT ?`, args, func(rows *sql.Rows) error {
		var v selectedID
		if err := rows.Scan(&v.id, &v.size); err != nil {
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
		if len(page.Records) == q.Limit || q.MaxBytes > 0 && len(page.Records) > 0 && v.size > q.MaxBytes-used {
			break
		}
		if q.MaxBytes > 0 && v.size > q.MaxBytes-used {
			page.Oversized = true
		}
		record, found, err := r.Get(RecordKey{Kind: q.Kind, ID: v.id})
		if err != nil {
			return RecordPage{}, err
		}
		if !found {
			return RecordPage{}, fmt.Errorf("%w: ID page record disappeared", ErrCoordinationFormat)
		}
		page.Records = append(page.Records, record)
		used += v.size
		if page.Oversized {
			break
		}
	}
	if len(page.Records) > 0 && len(page.Records) < len(selected) {
		page.Next = idPageNext(cursor, page.Records[len(page.Records)-1].Key.ID)
	}
	return page, r.ready()
}
