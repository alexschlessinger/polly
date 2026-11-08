package sessions

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
)

func TestSemanticSchema9Backfill(t *testing.T) {
	path, root := nativeSchema9Fixture(t, false)
	id, err := hex.DecodeString(root)
	nativeMust(t, err)
	raw, err := sql.Open("sqlite", sqliteDataSource(ModeDisk, path))
	nativeMust(t, err)
	for key, payload := range map[RecordKey]string{
		{"run", "r"}:       `{"id":"r","status":"running","limit":10,"starts":2}`,
		{"task", "t"}:      `{"id":"t","run":"r","status":"pending"}`,
		{"member", "m"}:    `{"id":"m","execution":"e"}`,
		{"execution", "e"}: `{"id":"e","member":"m","status":"running"}`,
	} {
		_, err = raw.Exec(`INSERT INTO swarm_records VALUES(?,?,?,?)`, id, key.Kind, key.ID, []byte(payload))
		nativeMust(t, err)
	}
	nativeMust(t, raw.Close())
	store, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path})
	nativeMust(t, err)
	defer store.Close()
	s := nativeHandle(t, store, "root")
	baseline := semanticMarkFor(t, s)
	nativeMust(t, s.ReadRecords(t.Context(), func(r CoordinationReader) error {
		h, err := r.(CoordinationStatusReader).SemanticStatus(root, true)
		if err != nil {
			return err
		}
		if h.Counts.NeedsDecision != 1 || h.Counts.Working != 1 || h.Budget == nil || h.Budget.Used != 2 || h.ParentDisposition != "" {
			t.Fatalf("backfill header=%+v", h)
		}
		p, err := r.(CoordinationStatusReader).SemanticStatusPage(SemanticStatusQuery{Actor: root, Section: "decisions", Limit: 1})
		if err == nil && (p.Total != 1 || p.Decisions[0].ID != "t") {
			t.Fatal(p)
		}
		return err
	}))
	nativePut(t, s, "task", "t", `{"id":"t","run":"r","status":"blocked"}`)
	if !semanticCompareFor(t, s, SemanticComparison{Baseline: baseline, Mode: SemanticParentNews}).Different {
		t.Fatal("backfill lost original value")
	}
	nativePut(t, s, "task", "t", `{"id":"t","run":"r","status":"pending"}`)
	if semanticCompareFor(t, s, SemanticComparison{Baseline: baseline, Mode: SemanticParentNews}).Different {
		t.Fatal("backfilled ABA differs")
	}
}

func TestSemanticDerivationFailureRollsBackBothWriters(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, table := range []string{"swarm_semantic_history", "swarm_status_items"} {
			t.Run(fmt.Sprintf("legacy=%t/%s", legacy, table), func(t *testing.T) {
				store, _ := openTestStore(t, ModeMemory, nil, 0)
				s := nativeHandle(t, store, "root")
				nativePut(t, s, "format", "swarm", `{"version":2}`)
				baseline := semanticMarkFor(t, s)
				_, err := store.db.Exec(`CREATE TRIGGER fail_semantic BEFORE INSERT ON ` + table + ` BEGIN SELECT RAISE(ABORT,'injected semantic failure'); END`)
				nativeMust(t, err)
				calls := 0
				if legacy {
					err = s.UpdateCoordination(t.Context(), func(raw *CoordinationState) error {
						calls++
						raw.Records["task"] = map[string]json.RawMessage{"t": json.RawMessage(`{"id":"t","status":"pending"}`)}
						return nil
					})
				} else {
					_, err = s.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
						calls++
						_ = tx.Put(RecordKey{"task", "t"}, json.RawMessage(`{"id":"t","status":"pending"}`))
						return nil // swallowing a failed Put still cannot commit
					})
				}
				if err == nil || calls != 1 {
					t.Fatalf("calls=%d err=%v", calls, err)
				}
				_, err = store.db.Exec(`DROP TRIGGER fail_semantic`)
				nativeMust(t, err)
				if semanticMarkFor(t, s) != baseline {
					t.Fatal("failed mutation changed mark")
				}
				nativeMust(t, s.ReadRecords(t.Context(), func(r CoordinationReader) error {
					_, found, err := r.Get(RecordKey{"task", "t"})
					if found {
						t.Fatal("partial source record")
					}
					if err != nil {
						return err
					}
					h, err := r.(CoordinationStatusReader).SemanticStatus(hex.EncodeToString(s.id), true)
					if h.Counts.NeedsDecision != 0 {
						t.Fatal("partial counts", h)
					}
					return err
				}))
			})
		}
	}
}

func TestSemanticStatusFanoutAndOffsetCosts(t *testing.T) {
	for _, n := range []int{16, 128} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			store, _ := openTestStore(t, ModeMemory, nil, 0)
			s := nativeHandle(t, store, "root")
			nativePut(t, s, "format", "swarm", `{"version":2}`)
			actor := hex.EncodeToString(s.id)
			_, err := s.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
				for i := 0; i < n; i++ {
					id := fmt.Sprintf("t%04d", i)
					if err := tx.Put(RecordKey{"task", id}, json.RawMessage(fmt.Sprintf(`{"id":%q,"status":"pending","dependencies":["root"]}`, id))); err != nil {
						return err
					}
				}
				return tx.Put(RecordKey{"task", "root"}, json.RawMessage(`{"id":"root","status":"pending"}`))
			})
			nativeMust(t, err)
			recorder := runtimemetrics.New()
			_, err = s.UpdateRecords(runtimemetrics.WithRecorder(t.Context(), recorder), func(tx CoordinationTx) error {
				return tx.Put(RecordKey{"task", "root"}, json.RawMessage(`{"id":"root","status":"done"}`))
			})
			nativeMust(t, err)
			cost := recorder.Snapshot()
			t.Logf("actual dependents=%d update SQL=%d returned rows=%d payload bytes=%d (intentional reverse-dependency fanout)", n, cost.Count(runtimemetrics.SQLStatements), cost.Count(runtimemetrics.RowsRead), cost.Count(runtimemetrics.PayloadBytesRead))
			nativeMust(t, s.ReadRecords(t.Context(), func(r CoordinationReader) error {
				page, err := r.(CoordinationStatusReader).SemanticStatusPage(SemanticStatusQuery{Actor: actor, Section: "decisions", Offset: n - 1, Limit: 1})
				if err == nil && (page.Total != n || len(page.Decisions) != 1 || page.Decisions[0].ID != fmt.Sprintf("t%04d", n-1)) {
					t.Fatal(page)
				}
				return err
			}))
			// SQL EXPLAIN exposes the offset walk that returned-row counters omit.
			nativeMust(t, store.withRead(t.Context(), func(conn *sql.Conn) error {
				for _, query := range []string{
					`SELECT total FROM swarm_status_totals WHERE parent_id=? AND name='count'`,
					`SELECT sort_key,length(value) FROM swarm_status_items WHERE parent_id=? AND section='decisions' AND actor='' ORDER BY sort_key LIMIT 1 OFFSET 127`,
				} {
					if err := queryEach(t.Context(), conn, "EXPLAIN QUERY PLAN "+query, []any{s.id}, func(rows *sql.Rows) error {
						var a, b, c int
						var detail string
						if err := rows.Scan(&a, &b, &c, &detail); err != nil {
							return err
						}
						if !strings.Contains(detail, "SEARCH") || strings.Contains(detail, "TEMP B-TREE") {
							t.Fatal(detail)
						}
						return nil
					}); err != nil {
						return err
					}
				}
				offsetWalk := false
				if err := queryEach(t.Context(), conn, `EXPLAIN SELECT sort_key FROM swarm_status_items WHERE parent_id=? AND section='decisions' AND actor='' ORDER BY sort_key LIMIT 1 OFFSET ?`, []any{s.id, n - 1}, func(rows *sql.Rows) error {
					var addr, p1, p2, p3, p5 int
					var op string
					var p4, comment any
					if err := rows.Scan(&addr, &op, &p1, &p2, &p3, &p4, &p5, &comment); err != nil {
						return err
					}
					offsetWalk = offsetWalk || op == "IfPos"
					return nil
				}); err != nil {
					return err
				}
				if !offsetWalk {
					t.Fatal("expected proportional offset traversal")
				}
				t.Logf("deep offset=%d: indexed range still walks skipped entries; not constant SQLite work", n-1)
				return nil
			}))
		})
	}
}
