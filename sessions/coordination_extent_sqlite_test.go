package sessions

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
)

func TestCoordinationExtentNativeReverseSeekPlans(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	s := nativeHandle(t, store, "root")
	var relationRoot, kindRoot int
	nativeMust(t, store.db.QueryRow(`SELECT rootpage FROM sqlite_master WHERE name='swarm_relations'`).Scan(&relationRoot))
	nativeMust(t, store.db.QueryRow(`SELECT rootpage FROM sqlite_master WHERE name='swarm_relations_kind_idx'`).Scan(&kindRoot))
	for _, test := range []struct {
		name, query string
		args        []any
		seekRoot    int
		seekFields  string
	}{
		{"partition", coordinationLastKeySQL, []any{s.id, string(CoordinationIndexWorkflowSteps), "w"}, relationRoot, "3"},
		{"kind-filtered", coordinationLastKindKeySQL, []any{s.id, string(CoordinationIndexOverviewDependency), CoordinationIndexKey("task", "t"), s.id, string(CoordinationIndexOverviewDependency), CoordinationIndexKey("task", "t"), "mail"}, kindRoot, "4"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var plans []string
			nativeMust(t, queryEach(t.Context(), store.db, "EXPLAIN QUERY PLAN "+test.query, test.args, func(rows *sql.Rows) error {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					return err
				}
				plans = append(plans, detail)
				return nil
			}))
			plan := strings.Join(plans, "\n")
			if !strings.Contains(plan, "SEARCH swarm_relations USING PRIMARY KEY") || strings.Contains(plan, "SCAN ") || strings.Contains(plan, "TEMP B-TREE") {
				t.Fatalf("not an ordered primary-key seek: %s", plan)
			}
			if test.seekRoot == kindRoot && !strings.Contains(plan, "USING COVERING INDEX swarm_relations_kind_idx (parent_id=? AND index_name=? AND index_key=? AND kind=?)") {
				t.Fatalf("filtered seek not covering: %s", plan)
			}
			// Inspect the actual SQLite VM program, not only returned-row counters.
			// The reverse seek must bind every partition (and filter) field; the
			// only opened B-trees are relations and its covering kind index.
			opened := map[int]int{}
			seek, limit := false, false
			nativeMust(t, queryEach(t.Context(), store.db, "EXPLAIN "+test.query, test.args, func(rows *sql.Rows) error {
				var addr, p1, p2, p3, p5 int
				var op string
				var p4, comment sql.NullString
				if err := rows.Scan(&addr, &op, &p1, &p2, &p3, &p4, &p5, &comment); err != nil {
					return err
				}
				switch op {
				case "OpenRead":
					if p2 != relationRoot && p2 != kindRoot {
						t.Fatalf("extent opened non-key B-tree root %d", p2)
					}
					opened[p1] = p2
				case "SeekLE":
					if opened[p1] == test.seekRoot && p4.String == test.seekFields {
						seek = true
					}
				case "DecrJumpZero":
					limit = true
				case "Sort", "SorterOpen", "Rewind", "Last":
					t.Fatalf("extent did not seek to partition end: %s", op)
				}
				return nil
			}))
			if !seek || !limit {
				t.Fatalf("missing reverse prefix seek/limit: seek=%v limit=%v plan=%s", seek, limit, plan)
			}
			t.Logf("actual production SQL plan:\n%s; VM SeekLE fields=%s, LIMIT enforced, key-only B-trees", plan, test.seekFields)
		})
	}
}

func TestCoordinationExtentNativeBoundedPayloadFree(t *testing.T) {
	for _, size := range []int{16, 1024} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			store, _ := openTestStore(t, ModeMemory, nil, 0)
			s := nativeHandle(t, store, "root")
			_, err := s.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
				nativeMust(t, tx.Put(RecordKey{"format", "swarm"}, json.RawMessage(`{"version":2}`)))
				for i := 0; i < size; i++ {
					// Grow both the selected partition and a different kind in the
					// same dependency partition, plus an unrelated report.
					nativeMust(t, tx.Put(RecordKey{"workflow_step", fmt.Sprintf("w/%d", i)}, json.RawMessage(`{"id":"s"}`)))
					nativeMust(t, tx.Put(RecordKey{"workflow_step", fmt.Sprintf("unrelated/%d", i)}, json.RawMessage(`{}`)))
					nativeMust(t, tx.Put(RecordKey{"member", fmt.Sprint(i)}, json.RawMessage(`{"task":"t"}`)))
				}
				return tx.Put(RecordKey{"mail", "selected"}, json.RawMessage(`{"task":"t","text":"`+strings.Repeat("x", 1<<20)+`"}`))
			})
			nativeMust(t, err)
			recorder := runtimemetrics.New()
			ctx := runtimemetrics.WithRecorder(t.Context(), recorder)
			nativeMust(t, s.ReadRecords(ctx, func(r CoordinationReader) error {
				for _, test := range []struct {
					q     RecordQuery
					total int
					last  RecordKey
				}{
					{RecordQuery{Index: CoordinationIndexWorkflowSteps, Key: "w"}, size, RecordKey{"workflow_step", fmt.Sprintf("w/%d", size-1)}},
					{RecordQuery{Index: CoordinationIndexOverviewDependency, Key: CoordinationIndexKey("task", "t"), Kind: "mail"}, 1, RecordKey{"mail", "selected"}},
				} {
					before := recorder.Snapshot()
					checkExtent(t, r, test.q, test.total, &test.last)
					after := recorder.Snapshot()
					checkExtentNoPayload(t, before, after)
					statements := after.Count(runtimemetrics.SQLStatements) - before.Count(runtimemetrics.SQLStatements)
					rows := after.Count(runtimemetrics.RowsRead) - before.Count(runtimemetrics.RowsRead)
					if statements != 2 || rows != 2 {
						t.Fatalf("extent size=%d dispatched statements=%d returned rows=%d", size, statements, rows)
					}
					t.Logf("size=%d index=%s: extent-only SQL=2 rows=2 payload/decodes=0 (excludes callback setup, lease/transaction SQL and hidden index entries)", size, test.q.Index)
				}
				return nil
			}))
		})
	}
}
