package sessions

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/internal/coordindex"
	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
)

func derivedIDs(t *testing.T, r CoordinationReader, index CoordinationIndex, key string) []string {
	t.Helper()
	var out []string
	after := ""
	for {
		p, err := r.Query(RecordQuery{Index: index, Key: key, After: after, Limit: 1, MaxBytes: 1, IncludeTotal: true})
		nativeMust(t, err)
		for _, v := range p.Records {
			out = append(out, v.Key.ID)
		}
		if p.Next == "" {
			break
		}
		if p.Next == after {
			t.Fatal("cursor did not advance")
		}
		after = p.Next
	}
	return out
}
func checkDerivedParity(t *testing.T, s *sqliteSession, index CoordinationIndex, key string, want ...string) {
	t.Helper()
	var got []string
	nativeMust(t, s.ReadRecords(t.Context(), func(r CoordinationReader) error { got = derivedIDs(t, r, index, key); return nil }))
	if !slices.Equal(got, want) {
		t.Fatalf("native %s/%q got=%q want=%q", index, key, got, want)
	}
	raw, err := s.ReadCoordination(t.Context())
	nativeMust(t, err)
	nativeMust(t, readLegacyRecords(t.Context(), raw, func(r CoordinationReader) error {
		legacy := derivedIDs(t, r, index, key)
		if !slices.Equal(legacy, want) {
			t.Fatalf("legacy %s/%q got=%q want=%q", index, key, legacy, want)
		}
		return nil
	}))
}
func TestDerivedPublicationPromotionReceiptsAndParity(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	s := nativeHandle(t, store, "root")
	put := func(kind, id, p string) { nativePut(t, s, kind, id, p) }
	put("format", "swarm", `{"version":2}`)
	put("execution", "e", `{"member":"reader","run":"r","status":"queued"}`)
	put("member", "reader", `{"execution":"e"}`)
	put("member", "author", `{"execution":"e"}`)
	put("member", "other", `{}`)
	put("publication", "a", `{"author":"author","run":"r","key":"k","value":"yes","posted":"2026-01-01T00:00:01Z"}`)
	put("publication", "b", `{"author":"other","run":"r","key":"k","value":"yes","duplicateOf":"a","posted":"2026-01-01T00:00:02Z"}`)
	put("publication", "z", `{"author":"author","run":"r","text":"later","posted":"2026-01-01T00:00:09Z"}`)
	audience := CoordinationIndexKey("run", "r")
	checkDerivedParity(t, s, CoordinationIndexPublicationActive, audience, "a", "b", "z")
	checkDerivedParity(t, s, CoordinationIndexPublicationSurvivor, audience, "a", "z")
	checkDerivedParity(t, s, CoordinationIndexPublicationPending, "reader", "a", "z")
	checkDerivedParity(t, s, CoordinationIndexPublicationPending, "author")
	checkDerivedParity(t, s, CoordinationIndexPublicationPending, "other")
	put("member", "reader", `{"execution":"e","publications":{"posted":"2026-01-01T00:00:09Z","id":"z"}}`)
	checkDerivedParity(t, s, CoordinationIndexPublicationPending, "reader")
	put("publication", "correction", `{"author":"author","run":"r","key":"k","value":"no","supersedes":"a","posted":"2026-01-01T00:00:10Z"}`)
	checkDerivedParity(t, s, CoordinationIndexPublicationPending, "reader", "b", "correction")
	checkDerivedParity(t, s, CoordinationIndexPublicationSurvivor, audience, "b", "z", "correction")
	// Corrections coalescing with another claim still deliver as nonduplicate events.
	put("publication", "again", `{"author":"author","run":"r","key":"k","value":"yes","supersedes":"correction","posted":"2026-01-01T00:00:11Z"}`)
	checkDerivedParity(t, s, CoordinationIndexPublicationPending, "reader", "b", "again")
	checkDerivedParity(t, s, CoordinationIndexPublicationSurvivor, audience, "b", "z")
	// Exact receipt suppresses an old promoted duplicate; high-water alone did not.
	put("publication", "b", `{"author":"other","run":"r","key":"k","value":"yes","duplicateOf":"a","admittedTo":["reader"],"posted":"2026-01-01T00:00:02Z"}`)
	checkDerivedParity(t, s, CoordinationIndexPublicationPending, "reader", "again")
	// Withdrawing the correction does not restore its predecessor: any recorded
	// supersedes edge withdraws its target, even if its source is itself withdrawn.
	put("publication", "last", `{"author":"author","run":"r","supersedes":"again","posted":"2026-01-01T00:00:12Z"}`)
	checkDerivedParity(t, s, CoordinationIndexPublicationActive, audience, "b", "z", "last")
	// Deleting the explicit withdrawal restores the target and its event.
	_, err := s.UpdateRecords(t.Context(), func(tx CoordinationTx) error { return tx.Delete(RecordKey{"publication", "last"}) })
	nativeMust(t, err)
	checkDerivedParity(t, s, CoordinationIndexPublicationPending, "reader", "again")
}

func TestDerivedPublicationScopesSnapshotAndReaderMoves(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	s := nativeHandle(t, store, "root")
	put := func(kind, id, p string) { nativePut(t, s, kind, id, p) }
	put("format", "swarm", `{"version":2}`)
	put("member", "m", `{"execution":"e"}`)
	put("publication", "local", `{"author":"a","run":"r","scope":"workspace","posted":"2026-01-01T00:00:01Z"}`)
	put("publication", "run", `{"author":"a","run":"r","posted":"2026-01-01T00:00:02Z"}`)
	put("publication", "host", `{"author":"a","run":"old","kind":"host","posted":"2026-01-01T00:00:03Z"}`)
	checkDerivedParity(t, s, CoordinationIndexPublicationPending, "m", "host")
	checkDerivedParity(t, s, CoordinationIndexPublicationPending, s.ViewID(), "local", "host") // implicit root
	put("execution", "e", `{"member":"m","run":"r","status":"running"}`)
	checkDerivedParity(t, s, CoordinationIndexPublicationPending, "m", "local", "run", "host")
	put("execution", "e", `{"member":"m","run":"different","status":"running"}`)
	checkDerivedParity(t, s, CoordinationIndexPublicationPending, "m", "host")
	for _, id := range []string{"s1", "s2"} {
		put("snapshot", id, fmt.Sprintf(`{"id":%q,"tree":"tree","source":"source","commit":"same"}`, id))
	}
	put("publication", "snap1", `{"author":"a","run":"r","scope":"snapshot","snapshot":"s1","key":"k","text":" same \n","posted":"2026-01-01T00:00:04Z"}`)
	put("publication", "snap2", `{"author":"a","run":"r","scope":"snapshot","snapshot":"s2","key":"k","text":"same","duplicateOf":"snap1","posted":"2026-01-01T00:00:05Z"}`)
	parent := CoordinationIndexKey("parent")
	checkDerivedParity(t, s, CoordinationIndexPublicationSurvivor, parent, "local", "host", "snap1")
	put("snapshot", "s2", `{"id":"s2","tree":"tree","source":"source","commit":"different"}`)
	checkDerivedParity(t, s, CoordinationIndexPublicationSurvivor, parent, "local", "host", "snap1", "snap2")
	checkDerivedParity(t, s, CoordinationIndexPublicationPending, s.ViewID(), "local", "host", "snap1", "snap2")
	put("snapshot", "s2", `{"id":"s2","tree":"tree","source":"source","commit":"same"}`)
	checkDerivedParity(t, s, CoordinationIndexPublicationSurvivor, parent, "local", "host", "snap1")
	put("parent", s.ViewID(), `{"publications":{"posted":"2026-01-01T00:00:09Z","id":"past"}}`)
	checkDerivedParity(t, s, CoordinationIndexPublicationPending, s.ViewID())
	// Unknown snapshots both resolve to the empty commit, as old publication helpers do.
	put("snapshot", "s2", `{"id":"wrong","commit":"same"}`)
	checkDerivedParity(t, s, CoordinationIndexPublicationSurvivor, parent, "local", "host", "snap1", "snap2")
	checkDerivedParity(t, s, CoordinationIndexPublicationPending, s.ViewID(), "snap2")
	// Fresh readers receive surviving duplicates even when their mark already passed them.
	put("member", "new", `{"execution":"r-execution","publications":{"posted":"2026-01-01T00:00:09Z"}}`)
	put("execution", "r-execution", `{"run":"r","status":"completed"}`)
	checkDerivedParity(t, s, CoordinationIndexPublicationPending, "new", "snap2")
}

func TestDerivedSelectorsNewsLifecycleAndOpaqueCursor(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	s := nativeHandle(t, store, "root")
	nativePut(t, s, "format", "swarm", `{"version":2}`)
	nativePut(t, s, "run", "done", `{"status":"completed"}`)
	nativePut(t, s, "run", "live", `{"status":"running"}`)
	nativePut(t, s, "followup", "old", `{"member":"m","refresh":true,"phase":"started"}`)
	nativePut(t, s, "followup", "prepare", `{"member":"m","refresh":true,"phase":"preparing"}`)
	nativePut(t, s, "apply", "receipt", `{"tasks":[{"task":"t","revision":2}]}`)
	checkDerivedParity(t, s, CoordinationIndexRunStatus, "running", "live")
	checkDerivedParity(t, s, CoordinationIndexFollowupPreparingMember, "m", "prepare")
	checkDerivedParity(t, s, CoordinationIndexOverviewDependency, CoordinationIndexKey("task", "t"), "receipt")
	nativePut(t, s, "execution", "e", `{"status":"queued","generation":1}`)
	nativePut(t, s, "member", "m", `{"execution":"e","label":"before"}`)
	scopes := []RevisionScope{{CoordinationIndexAdmissionNews, CoordinationIndexKey("member", "m")}, {CoordinationIndexAdmissionNews, CoordinationIndexKey("execution", "e")}, {CoordinationIndexRecord, CoordinationIndexKey("member", "m")}, {CoordinationIndexRecord, CoordinationIndexKey("execution", "e")}}
	before := nativeRevisions(t, s, scopes...)
	nativePut(t, s, "member", "m", `{"execution":"e","label":"after"}`)
	lifecycleBefore := nativeRevisions(t, s, RevisionScope{CoordinationIndexMemberLifecycle, "active"})
	nativePut(t, s, "execution", "e", `{"status":"running","generation":1}`)
	lifecycleAfter := nativeRevisions(t, s, RevisionScope{CoordinationIndexMemberLifecycle, "active"})
	if lifecycleBefore[0] != lifecycleAfter[0] {
		t.Fatal("unchanged derived lifecycle invalidated by queued->running")
	}
	after := nativeRevisions(t, s, scopes...)
	if before[0] != after[0] || before[1] != after[1] || before[2] == after[2] || before[3] == after[3] {
		t.Fatalf("news/raw stamps before=%v after=%v", before, after)
	}
	checkDerivedParity(t, s, CoordinationIndexMemberLifecycle, "active", "m")
	nativePut(t, s, "execution", "e", `{"status":"waiting","generation":1}`)
	checkDerivedParity(t, s, CoordinationIndexMemberLifecycle, "waiting", "m")
	nativePut(t, s, "member", "m", `{"execution":"e","control":"stopped"}`)
	checkDerivedParity(t, s, CoordinationIndexMemberLifecycle, "paused", "m")
	nativePut(t, s, "execution", "irrelevant", `{"member":"m","status":"completed"}`)
	checkDerivedParity(t, s, CoordinationIndexMemberLifecycle, "paused", "m")
	raw, err := s.ReadCoordination(t.Context())
	nativeMust(t, err)
	legacy := newLegacyRecordTx(t.Context(), raw)
	defer legacy.close()
	rev, err := legacy.Revisions(scopes)
	nativeMust(t, err)
	nativeMust(t, legacy.Put(RecordKey{"member", "m"}, json.RawMessage(`{"execution":"e","control":"stopped","label":"another"}`)))
	rev2, err := legacy.Revisions(scopes)
	nativeMust(t, err)
	if rev[0] != rev2[0] {
		t.Fatal("legacy label invalidated news")
	}
	ids := []string{string([]byte{0xff}), string([]byte{0xff, 0, 'z'})}
	for _, id := range ids {
		nativePut(t, s, "publication", id, `{"kind":"host","author":"writer","posted":"2026-01-01T00:00:00Z"}`)
	}
	checkDerivedParity(t, s, CoordinationIndexPublicationPending, s.ViewID(), ids...)
	nativeMust(t, s.ReadRecords(t.Context(), func(r CoordinationReader) error {
		page, err := r.Query(RecordQuery{Index: CoordinationIndexPublicationPending, Key: s.ViewID(), Limit: 1})
		nativeMust(t, err)
		_, err = r.Query(RecordQuery{Index: CoordinationIndexPublicationPending, Key: "different", After: page.Next, Limit: 1})
		if err != ErrCoordinationCursor {
			t.Fatalf("cursor binding: %v", err)
		}
		return nil
	}))
}

func TestDerivedPendingBoundedAccumulatedReceipts(t *testing.T) {
	var baseline runtimemetrics.Snapshot
	for _, size := range []int{16, 1024} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			store, _ := openTestStore(t, ModeMemory, nil, 0)
			s := nativeHandle(t, store, "root")
			nativePut(t, s, "format", "swarm", `{"version":2}`)
			nativePut(t, s, "execution", "e", `{"run":"r","status":"running"}`)
			nativePut(t, s, "member", "reader", `{"execution":"e","publications":{"posted":"2026-01-01T00:00:00Z","id":"past"}}`)
			_, err := s.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
				for i := 0; i < size; i++ {
					// These former confirmations survive but are already receipted. Keeping
					// them active defeats LIMIT + NOT EXISTS queries over a global partition.
					id := fmt.Sprintf("old-%04d", i)
					p := fmt.Sprintf(`{"run":"r","author":"writer","key":%q,"value":"same","duplicateOf":"removed","admittedTo":["reader"],"posted":"2025-01-01T00:00:00Z"}`, id)
					if err := tx.Put(RecordKey{"publication", id}, json.RawMessage(p)); err != nil {
						return err
					}
					if err := tx.Put(RecordKey{"publication", "other-" + id}, json.RawMessage(`{"run":"unrelated","author":"writer"}`)); err != nil {
						return err
					}
				}
				return tx.Put(RecordKey{"publication", "promoted"}, json.RawMessage(`{"run":"r","author":"writer","key":"target","value":"same","duplicateOf":"removed","posted":"2025-01-01T00:00:01Z"}`))
			})
			nativeMust(t, err)
			recorder := runtimemetrics.New()
			ctx := runtimemetrics.WithRecorder(t.Context(), recorder)
			nativeMust(t, s.ReadRecords(ctx, func(r CoordinationReader) error {
				page, err := r.Query(RecordQuery{Index: CoordinationIndexPublicationPending, Key: "reader", Limit: 1, IncludeTotal: true})
				if len(page.Records) != 1 || page.Records[0].Key.ID != "promoted" || page.Total != 1 {
					t.Fatalf("pending %+v", page)
				}
				return err
			}))
			got := recorder.Snapshot()
			if size == 16 {
				baseline = got
			} else {
				for _, counter := range []runtimemetrics.Counter{runtimemetrics.SQLStatements, runtimemetrics.RowsRead, runtimemetrics.PayloadBytesRead, runtimemetrics.RecordsDecoded} {
					if got.Count(counter) != baseline.Count(counter) {
						t.Fatalf("work grew counter %v: %d -> %d", counter, baseline.Count(counter), got.Count(counter))
					}
				}
			}
			var pending int
			nativeMust(t, store.db.QueryRow(`SELECT count(*) FROM swarm_relations WHERE parent_id=? AND index_name=? AND index_key=?`, s.id, string(CoordinationIndexPublicationPending), "reader").Scan(&pending))
			if pending != 1 {
				t.Fatal(pending)
			}
			t.Logf("historical=%d unrelated=%d reader-pending-links=%d query statements=%d rows=%d bytes=%d decodes=%d", size, size, pending, got.Count(runtimemetrics.SQLStatements), got.Count(runtimemetrics.RowsRead), got.Count(runtimemetrics.PayloadBytesRead), got.Count(runtimemetrics.RecordsDecoded))
			// Cover the actual query/seek access paths, not only returned-row counters.
			for _, q := range []string{
				`SELECT kind,id FROM swarm_relations WHERE parent_id=? AND index_name=? AND index_key=? AND sort_key>? ORDER BY sort_key LIMIT 2`,
				`DELETE FROM swarm_relations WHERE parent_id=? AND index_name=? AND index_key=? AND kind='publication' AND id='promoted'`,
			} {
				args := []any{s.id, string(CoordinationIndexPublicationPending), "reader"}
				if strings.Contains(q, "sort_key>?") {
					args = append(args, recordOrderKey(coordindex.Order{Kind: "publication", Posted: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), ID: "past"}))
				}
				var plan []string
				nativeMust(t, queryEach(t.Context(), store.db, "EXPLAIN QUERY PLAN "+q, args, func(rows *sql.Rows) error {
					var a, b, c int
					var text string
					if err := rows.Scan(&a, &b, &c, &text); err != nil {
						return err
					}
					plan = append(plan, text)
					return nil
				}))
				joined := strings.Join(plan, ";")
				if strings.Contains(joined, "SCAN ") || strings.Contains(joined, "TEMP B-TREE") || !strings.Contains(joined, "SEARCH") {
					t.Fatal(joined)
				}
				t.Log(joined)
			}
		})
	}
}
