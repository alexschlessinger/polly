package sessions

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
)

func semanticMarkFor(t *testing.T, access CoordinationRecords) SemanticMark {
	t.Helper()
	var mark SemanticMark
	nativeMust(t, access.ReadRecords(t.Context(), func(r CoordinationReader) error {
		var err error
		mark, err = SemanticReader(r).SemanticMark()
		return err
	}))
	return mark
}
func semanticCompareFor(t *testing.T, access CoordinationRecords, q SemanticComparison) SemanticChanges {
	t.Helper()
	var out SemanticChanges
	nativeMust(t, access.ReadRecords(t.Context(), func(r CoordinationReader) error {
		var err error
		out, err = SemanticReader(r).SemanticChanges(q)
		return err
	}))
	return out
}
func TestSemanticNewsCases(t *testing.T) {
	for _, writer := range []string{"native", "legacy-api", "forced-legacy"} {
		t.Run(writer, func(t *testing.T) {
			for _, tc := range []struct {
				name          string
				before, after []string
				want, full    bool
			}{
				{"same", []string{`{"id":"t","status":"blocked"}`}, nil, false, false},
				{"label only", []string{`{"id":"t","status":"blocked"}`}, []string{`{"id":"t","status":"blocked","description":"new"}`}, false, false},
				{"changed", []string{`{"id":"t","status":"blocked"}`}, []string{`{"id":"t","status":"pending"}`}, true, false},
				{"ABA", []string{`{"id":"t","status":"blocked"}`}, []string{`{"id":"t","status":"pending"}`, `{"id":"t","status":"blocked"}`}, false, false},
				{"delete", []string{`{"id":"t","status":"blocked"}`}, []string{""}, false, false},
				{"full deletion", []string{`{"id":"t","status":"blocked"}`}, []string{""}, true, true},
				{"create delete", nil, []string{`{"id":"t","status":"blocked"}`, ""}, false, false},
				{"recreate identical", []string{`{"id":"t","status":"blocked"}`}, []string{"", `{"id":"t","status":"blocked"}`}, false, false},
				{"new", nil, []string{`{"id":"t","status":"blocked"}`}, true, false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					store, _ := openTestStore(t, ModeMemory, nil, 0)
					s := nativeHandle(t, store, "root")
					nativePut(t, s, "format", "swarm", `{"version":2}`)
					var access CoordinationRecords = s
					if writer == "forced-legacy" {
						a, err := OpenCoordinationAccess(struct{ CoordinationSession }{s})
						nativeMust(t, err)
						access = a
					}
					put := func(payload string) {
						if writer == "legacy-api" {
							nativeMust(t, s.UpdateCoordination(t.Context(), func(raw *CoordinationState) error {
								if raw.Records["task"] == nil {
									raw.Records["task"] = map[string]json.RawMessage{}
								}
								if payload == "" {
									delete(raw.Records["task"], "t")
								} else {
									raw.Records["task"]["t"] = json.RawMessage(payload)
								}
								return nil
							}))
							return
						}
						_, err := access.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
							if payload == "" {
								return tx.Delete(RecordKey{"task", "t"})
							}
							return tx.Put(RecordKey{"task", "t"}, json.RawMessage(payload))
						})
						nativeMust(t, err)
					}
					for _, v := range tc.before {
						put(v)
					}
					q := SemanticComparison{Baseline: semanticMarkFor(t, access), Mode: SemanticParentNews}
					if tc.full {
						q.Mode = SemanticCoordination
					}
					first := semanticCompareFor(t, access, q)
					if first.Different {
						t.Fatal("new baseline differs")
					}
					q.After = first.Continue
					for _, v := range tc.after {
						put(v)
					}
					out := semanticCompareFor(t, access, q)
					if out.Different != tc.want {
						t.Fatalf("got %+v want difference=%v", out, tc.want)
					}
					if !out.Different {
						q.After = out.Continue
						if semanticCompareFor(t, access, q).Different {
							t.Fatal("false continuation changed baseline")
						}
					}
				})
			}
		})
	}
}

func TestSemanticDependenciesAndStagedRollback(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	s := nativeHandle(t, store, "root")
	for _, v := range []struct{ kind, id, payload string }{
		{"format", "swarm", `{"version":2}`},
		{"workflow", "w", `{"id":"w","status":"running"}`},
		{"execution", "e", `{"id":"e","member":"m","status":"queued","workflow":"w","generation":1}`},
		{"member", "m", `{"id":"m","execution":"e","controller":"w"}`},
		{"task", "t", `{"id":"t","execution":"e","status":"blocked"}`},
	} {
		nativePut(t, s, v.kind, v.id, v.payload)
	}
	q := SemanticComparison{Baseline: semanticMarkFor(t, s), Mode: SemanticParentNews}
	nativePut(t, s, "execution", "e", `{"id":"e","member":"m","status":"running","workflow":"w","generation":1}`)
	if semanticCompareFor(t, s, q).Different {
		t.Fatal("queued-running is not news")
	}
	nativePut(t, s, "task", "t", `{"id":"t","execution":"e","status":"pending"}`)
	out := semanticCompareFor(t, s, q)
	if out.Different {
		t.Fatal("controlled task is not parent news")
	}
	q.After = out.Continue
	full := q
	full.Mode = SemanticCoordination
	full.After = ""
	if !semanticCompareFor(t, s, full).Different {
		t.Fatal("full mode must include controlled changes")
	}
	nativePut(t, s, "execution", "e", `{"id":"e","member":"m","status":"running","generation":1}`)
	if !semanticCompareFor(t, s, q).Different {
		t.Fatal("control release must compare against original baseline")
	}
	baseline := semanticMarkFor(t, s)
	veto := errors.New("veto")
	_, err := s.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
		semantic := SemanticReader(tx)
		if err := tx.Put(RecordKey{"task", "t"}, json.RawMessage(`{"id":"t","execution":"e","status":"blocked"}`)); err != nil {
			return err
		}
		a, err := semantic.SemanticMark()
		if err != nil {
			return err
		}
		if err := tx.Put(RecordKey{"task", "t"}, json.RawMessage(`{"id":"t","execution":"e","status":"pending"}`)); err != nil {
			return err
		}
		b, err := semantic.SemanticMark()
		if err != nil {
			return err
		}
		if a == b {
			t.Fatal("staged states alias")
		}
		out, err := semantic.SemanticChanges(SemanticComparison{Baseline: a, Mode: SemanticParentNews})
		if err != nil {
			return err
		}
		if !out.Different {
			t.Fatal("staged comparison missed change")
		}
		return veto
	})
	if !errors.Is(err, veto) {
		t.Fatal(err)
	}
	if got := semanticMarkFor(t, s); got != baseline {
		t.Fatal("rollback changed durable watermark")
	}
}

func TestSemanticNoticeIDAndMailSuppression(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	s := nativeHandle(t, store, "root")
	nativePut(t, s, "format", "swarm", `{"version":2}`)
	nativePut(t, s, "task", "t", `{"id":"t","status":"blocked","execution":"e","revision":1}`)
	nativePut(t, s, "mail", "z", `{"id":"z","task":"t","execution":"e","revision":1,"delivered":true,"posted":"2000-01-01T00:00:00Z"}`)
	baseline := semanticMarkFor(t, s)
	nativePut(t, s, "mail", "a", fmt.Sprintf(`{"id":"a","task":"t","execution":"e","revision":1,"to":%q,"posted":"2020-01-01T00:00:00Z"}`, hex.EncodeToString(s.id)))
	if !semanticCompareFor(t, s, SemanticComparison{Baseline: baseline, Mode: SemanticParentNews}).Different {
		t.Fatal("notice must select by ID, not posting time")
	}
	pending := func(want bool) {
		t.Helper()
		nativeMust(t, s.ReadRecords(t.Context(), func(r CoordinationReader) error {
			got, err := SemanticReader(r).SemanticPendingMail(hex.EncodeToString(s.id))
			if err == nil && got != want {
				t.Fatalf("pending=%v want %v", got, want)
			}
			return err
		}))
	}
	pending(true)
	nativePut(t, s, "followup", "a", `{"refresh":true,"phase":"preparing"}`)
	pending(false)
	nativePut(t, s, "followup", "a", `{"refresh":true,"phase":"launched"}`)
	pending(true)
}

func TestSemanticBoundedHistoryAndMarks(t *testing.T) {
	for _, n := range []int{16, 1024} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			store, _ := openTestStore(t, ModeMemory, nil, 0)
			s := nativeHandle(t, store, "root")
			nativePut(t, s, "format", "swarm", `{"version":2}`)
			_, err := s.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
				for i := 0; i < n; i++ {
					id := fmt.Sprintf("t%04d", i)
					if err := tx.Put(RecordKey{"task", id}, json.RawMessage(fmt.Sprintf(`{"id":%q,"status":"done"}`, id))); err != nil {
						return err
					}
				}
				return nil
			})
			nativeMust(t, err)
			q := SemanticComparison{Baseline: semanticMarkFor(t, s), Mode: SemanticParentNews}
			out := semanticCompareFor(t, s, q)
			q.After = out.Continue
			recorder := runtimemetrics.New()
			ctx := runtimemetrics.WithRecorder(t.Context(), recorder)
			nativeMust(t, s.ReadRecords(ctx, func(r CoordinationReader) error {
				out, err := SemanticReader(r).SemanticChanges(q)
				if out.Different {
					t.Fatal("unchanged family differs")
				}
				return err
			}))
			stats := recorder.Snapshot()
			if stats.Count(runtimemetrics.PayloadBytesRead) != 0 || stats.Count(runtimemetrics.RowsRead) > 4 {
				t.Fatalf("unchanged history work: %+v", stats)
			}
			for _, bad := range []SemanticComparison{{Baseline: "!", Mode: SemanticParentNews}, {Baseline: q.Baseline, Mode: "bad"}, {Baseline: q.Baseline, Mode: SemanticCoordination, After: q.After}} {
				err := s.ReadRecords(t.Context(), func(r CoordinationReader) error { _, err := SemanticReader(r).SemanticChanges(bad); return err })
				if err == nil {
					t.Fatalf("accepted bad mark: %+v", bad)
				}
			}
			nativeMust(t, store.withRead(t.Context(), func(conn *sql.Conn) error {
				for _, query := range []string{
					`EXPLAIN QUERY PLAN SELECT kind,id,value FROM swarm_semantic_heads WHERE parent_id=? AND stamp>? ORDER BY stamp,kind,id LIMIT 128`,
					`EXPLAIN QUERY PLAN SELECT present,value FROM swarm_semantic_history WHERE parent_id=? AND kind=? AND id=? AND stamp<=? ORDER BY stamp DESC LIMIT 1`,
				} {
					args := []any{s.id, 0}
					if strings.Contains(query, "history") {
						args = []any{s.id, "task", "t0000", 100}
					}
					if err := queryEach(t.Context(), conn, query, args, func(rows *sql.Rows) error {
						var a, b, c int
						var detail string
						if err := rows.Scan(&a, &b, &c, &detail); err != nil {
							return err
						}
						if !strings.Contains(detail, "SEARCH") || strings.Contains(detail, "TEMP B-TREE") {
							t.Fatalf("unbounded plan: %s", detail)
						}
						return nil
					}); err != nil {
						return err
					}
				}
				return nil
			}))
		})
	}
}

func TestSemanticCancellationAndClosedReader(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	s := nativeHandle(t, store, "root")
	var retained CoordinationSemanticReader
	nativeMust(t, s.ReadRecords(t.Context(), func(r CoordinationReader) error { retained = SemanticReader(r); return nil }))
	if _, err := retained.SemanticMark(); !errors.Is(err, ErrCoordinationClosed) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.ReadRecords(ctx, func(CoordinationReader) error { t.Fatal("canceled callback ran"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
