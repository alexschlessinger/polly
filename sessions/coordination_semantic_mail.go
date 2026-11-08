package sessions

import (
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/alexschlessinger/pollytool/internal/coordindex"
)

const semanticPendingMail coordindex.Index = "derived.semantic.pending-mail"

func (s semanticSource) pendingMail(id string) error {
	m, err := semanticGet[struct {
		To        string
		Delivered bool
	}](s, "mail", id)
	if err != nil {
		return err
	}
	var keys []string
	if m != nil && !m.Delivered {
		f, err := semanticGet[struct {
			Refresh bool
			Phase   string
		}](s, "followup", id)
		if err != nil {
			return err
		}
		if f == nil || !f.Refresh || f.Phase == "launched" {
			keys = []string{m.To}
		}
	}
	item := derivedItem{key: RecordKey{"mail", id}, order: coordindex.Order{Kind: "mail", ID: id}}
	if m != nil {
		b, err := s.store.get(item.key)
		if err != nil {
			return err
		}
		_, item.order, err = coordindex.Extract("mail", id, b)
		if err != nil {
			return err
		}
	}
	return (derivedEngine{store: s.store, parent: s.parent}).replace(item, semanticPendingMail, keys)
}
func (r *sqliteRecordTx) SemanticPendingMail(actor string) (bool, error) {
	if _, err := r.semanticToken(); err != nil {
		return false, err
	}
	var count int
	nativeSQL(r.ctx)
	err := r.conn.QueryRowContext(r.ctx, `SELECT record_count FROM swarm_revisions WHERE parent_id=? AND index_name=? AND index_key=?`, r.parent, string(semanticPendingMail), actor).Scan(&count)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err == nil {
		nativeRows(r.ctx, 1)
	}
	return count > 0, err
}
func (r legacySemanticReader) SemanticPendingMail(actor string) (bool, error) {
	q := RecordQuery{Index: CoordinationIndexMailPending, Key: actor, Limit: 128}
	for {
		page, err := r.Query(q)
		if err != nil {
			return false, err
		}
		for _, m := range page.Records {
			f, found, err := r.Get(RecordKey{"followup", m.Key.ID})
			if err != nil {
				return false, err
			}
			if !found {
				return true, nil
			}
			var followup struct {
				Refresh bool
				Phase   string
			}
			if err := json.Unmarshal(f.Payload, &followup); err != nil {
				return false, err
			}
			if !followup.Refresh || followup.Phase == "launched" {
				return true, nil
			}
		}
		if page.Next == "" {
			return false, nil
		}
		q.After = page.Next
	}
}
