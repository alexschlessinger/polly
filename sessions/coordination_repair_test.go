package sessions

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
)

// nativePageQuery is the kind-filtered page selection production issues, with
// the selector arguments left as placeholders so EXPLAIN can plan it directly.
const nativePageQuery = `r.parent_id=? AND r.index_name=? AND r.index_key=? AND r.kind=? ORDER BY r.sort_key LIMIT ? OFFSET ?`

func seedNativePageFamily(t *testing.T, s *sqliteSession) {
	t.Helper()
	_, err := s.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
		nativeMust(t, tx.Put(RecordKey{"format", "swarm"}, json.RawMessage(`{"version":2}`)))
		for i := 0; i < 40; i++ {
			nativeMust(t, tx.Put(RecordKey{"mail", "m" + string(rune('a'+i%26)) + "-" + string(rune('0'+i/26))}, json.RawMessage(`{"to":"recipient","posted":"2026-01-01T00:00:00Z"}`)))
			nativeMust(t, tx.Put(RecordKey{"execution", "e" + string(rune('a'+i%26))}, json.RawMessage(`{"status":"running"}`)))
		}
		return nil
	})
	nativeMust(t, err)
}

// A kind-filtered page must seek the covering kind index. Without maintained
// statistics the planner otherwise prefers the partition primary key and visits
// every preceding row in the partition before filtering.
func TestRecordPageKindFilterUsesCoveringIndex(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	s := nativeHandle(t, store, "root")
	seedNativePageFamily(t, s)
	var plans []string
	nativeMust(t, queryEach(t.Context(), store.db, "EXPLAIN QUERY PLAN "+recordPageSQL("mail")+nativePageQuery, []any{s.id, string(CoordinationIndexRecords), "", "mail", 129, 0}, func(rows *sql.Rows) error {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			return err
		}
		plans = append(plans, detail)
		return nil
	}))
	plan := strings.Join(plans, "\n")
	if !strings.Contains(plan, "swarm_relations_kind_idx") {
		t.Fatalf("kind-filtered page did not use the covering kind index: %s", plan)
	}
	if strings.Contains(plan, "USING PRIMARY KEY") || strings.Contains(plan, "SCAN ") || strings.Contains(plan, "TEMP B-TREE") {
		t.Fatalf("kind-filtered page is not a single ordered index seek: %s", plan)
	}
	// The unfiltered page orders by the partition primary key and names no index.
	if strings.Contains(recordPageSQL(""), "INDEXED BY") {
		t.Fatalf("unfiltered page must not force an index: %s", recordPageSQL(""))
	}
}
