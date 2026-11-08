package sessions

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
)

func TestNotificationRevisionAtomicRollback(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, index := range []CoordinationIndex{CoordinationIndexSemanticNews, CoordinationIndexSemanticMail, CoordinationIndexDisplay, CoordinationIndexDisplayFact} {
			t.Run(fmt.Sprintf("legacy=%t/%s", legacy, index), func(t *testing.T) {
				store, _ := openTestStore(t, ModeMemory, nil, 0)
				s := nativeHandle(t, store, "root")
				nativePut(t, s, "format", "swarm", `{"version":2}`)
				nativePut(t, s, "member", "m", `{"id":"m","name":"old"}`)
				scopes := notificationScopes()
				before := nativeRevisions(t, s, scopes...)
				display := displayReadFor(t, s, "")
				semantic := semanticMarkFor(t, s)
				key, payload := RecordKey{"member", "m"}, `{"id":"m","name":"new","control":"paused"}`
				if index == CoordinationIndexSemanticMail {
					key, payload = RecordKey{"mail", "mail"}, `{"id":"mail","to":"a"}`
				}
				_, err := store.db.Exec(`CREATE TEMP TRIGGER fail_notification BEFORE INSERT ON swarm_revisions WHEN NEW.index_name='` + string(index) + `' BEGIN SELECT RAISE(ABORT,'injected notification revision failure'); END`)
				nativeMust(t, err)
				if legacy {
					err = s.UpdateCoordination(t.Context(), func(state *CoordinationState) error {
						if state.Records[key.Kind] == nil {
							state.Records[key.Kind] = map[string]json.RawMessage{}
						}
						state.Records[key.Kind][key.ID] = json.RawMessage(payload)
						return nil
					})
				} else {
					var commit CoordinationCommit
					commit, err = s.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
						_ = tx.Put(key, json.RawMessage(payload)) // swallowed SQL error still vetoes commit
						return nil
					})
					if len(commit.Changed) != 0 {
						t.Fatalf("failed commit exposed scopes: %+v", commit)
					}
				}
				if err == nil {
					t.Fatal("injected revision failure committed")
				}
				_, err = store.db.Exec(`DROP TRIGGER fail_notification`)
				nativeMust(t, err)
				if !reflect.DeepEqual(before, nativeRevisions(t, s, scopes...)) || semanticMarkFor(t, s) != semantic || displayReadFor(t, s, display.Mark).Mark != display.Mark {
					t.Fatal("failed revision write changed durable projections")
				}
				// Fail after all record/revision writes, in the effects phase.
				badSequence := int64(99)
				if legacy {
					err = s.UpdateCoordination(t.Context(), func(state *CoordinationState) error {
						state.Records["member"]["m"] = json.RawMessage(`{"id":"m","name":"new","control":"paused"}`)
						state.ExpectedSequence = &badSequence
						return nil
					})
				} else {
					_, err = s.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
						if err := tx.Put(RecordKey{"member", "m"}, json.RawMessage(`{"id":"m","name":"new","control":"paused"}`)); err != nil {
							return err
						}
						staged, err := tx.Revisions(scopes)
						if err != nil {
							return err
						}
						if staged[0] == before[0] || staged[1] == before[1] || staged[2] == before[2] {
							t.Fatal("staged semantic/display revisions did not change")
						}
						return tx.Effects(CoordinationEffects{ExpectedSequence: &badSequence})
					})
				}
				if !errors.Is(err, ErrCoordinationRequest) || !reflect.DeepEqual(before, nativeRevisions(t, s, scopes...)) {
					t.Fatalf("effects rollback failed: %v", err)
				}
			})
		}
	}
}

func TestNotificationRevisionPromotionReopenAndIncarnation(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	s := nativeHandle(t, store, "root")
	seed := func() {
		nativePut(t, s, "format", "swarm", `{"version":2}`)
		nativePut(t, s, "member", "m", `{"id":"m","name":"member"}`)
		nativePut(t, s, "mail", "mail", `{"id":"mail","to":"a"}`)
	}
	seed()
	scopes := append(notificationScopes(), RevisionScope{CoordinationIndexRecord, CoordinationIndexKey("member", "m")})
	before := nativeRevisions(t, s, scopes...)
	path := filepath.Join(t.TempDir(), "notifications.db")
	nativeMust(t, store.Promote(t.Context(), path))
	if !reflect.DeepEqual(before, nativeRevisions(t, s, scopes...)) {
		t.Fatal("promotion changed scope tokens")
	}
	nativeMust(t, s.Close())
	nativeMust(t, store.Close())
	reopened, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path})
	nativeMust(t, err)
	defer reopened.Close()
	s = nativeHandle(t, reopened, "root")
	if !reflect.DeepEqual(before, nativeRevisions(t, s, scopes...)) {
		t.Fatal("reopen changed scope tokens")
	}
	oldID := s.ViewID()
	nativeMust(t, s.Close())
	nativeMust(t, reopened.withWrite(t.Context(), func(conn *sql.Conn) error {
		_, err := conn.ExecContext(t.Context(), `DELETE FROM sessions WHERE id=?`, s.id)
		return err
	}))
	s = nativeHandle(t, reopened, "root")
	nativeMust(t, s.Close())
	// Normal creation chooses a random ID. Simulate a restore/recreation that
	// retains the old public ID but has a fresh created_ns incarnation.
	oldRoot, err := hex.DecodeString(oldID)
	nativeMust(t, err)
	nativeMust(t, reopened.withWrite(t.Context(), func(conn *sql.Conn) error {
		_, err := conn.ExecContext(t.Context(), `UPDATE sessions SET id=? WHERE id=?`, oldRoot, s.id)
		return err
	}))
	s = nativeHandle(t, reopened, "root")
	seed()
	for i, rev := range nativeRevisions(t, s, scopes...) {
		if rev == before[i] {
			t.Fatalf("recreated root aliased %+v", scopes[i])
		}
	}
	before = nativeRevisions(t, s, scopes...)
	nativePut(t, s, "format", "swarm", `{"version":3}`)
	for i, rev := range nativeRevisions(t, s, scopes[:len(scopes)-1]...) {
		if rev == before[i] {
			t.Fatal("format transition did not invalidate projection token")
		}
	}
}

func TestNotificationRevisionSchema9AndExistingZero(t *testing.T) {
	path, root := nativeSchema9Fixture(t, false)
	id, err := hex.DecodeString(root)
	nativeMust(t, err)
	raw, err := sql.Open("sqlite", sqliteDataSource(ModeDisk, path))
	nativeMust(t, err)
	for key, payload := range map[RecordKey]string{
		{"member", "m"}:  `{"id":"m","name":"old"}`,
		{"mail", "mail"}: `{"id":"mail","to":"a"}`,
	} {
		_, err = raw.Exec(`INSERT INTO swarm_records VALUES(?,?,?,?)`, id, key.Kind, key.ID, []byte(payload))
		nativeMust(t, err)
	}
	nativeMust(t, raw.Close())
	store, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path})
	nativeMust(t, err)
	s := nativeHandle(t, store, "root")
	scopes := notificationScopes()
	before := nativeRevisions(t, s, scopes...)
	// Current unreleased v10 databases may already have projections but no new
	// revision rows. Opening them is read-only with respect to those rows: zero
	// is a valid initial observation, not an invitation to repair old history.
	nativeMust(t, s.Close())
	nativeMust(t, store.Close())
	raw, err = sql.Open("sqlite", sqliteDataSource(ModeDisk, path))
	nativeMust(t, err)
	_, err = raw.Exec(`DELETE FROM swarm_revisions WHERE index_name IN ('semantic.news','semantic.mail','display','display.fact')`)
	nativeMust(t, err)
	nativeMust(t, raw.Close())
	store, err = OpenStore(StoreConfig{Mode: ModeDisk, Path: path})
	nativeMust(t, err)
	defer store.Close()
	s = nativeHandle(t, store, "root")
	zero := nativeRevisions(t, s, scopes...)
	for _, i := range []int{0, 1, 2, 5, 7} {
		if before[i] == zero[i] {
			t.Fatalf("schema9 did not backfill scope %+v", scopes[i])
		}
	}
	var n int
	nativeMust(t, store.db.QueryRow(`SELECT count(*) FROM swarm_revisions WHERE index_name IN ('semantic.news','semantic.mail','display','display.fact')`).Scan(&n))
	if n != 0 {
		t.Fatal("healthy current open repaired optional revision rows")
	}
	nativePut(t, s, "member", "m", `{"id":"m","name":"old"}`)
	if !reflect.DeepEqual(zero, nativeRevisions(t, s, scopes...)) {
		t.Fatal("no-op advanced zero revisions")
	}
	commit := nativePut(t, s, "member", "m", `{"id":"m","name":"new","control":"paused"}`)
	for _, i := range []int{0, 1, 2} {
		if nativeRevisions(t, s, scopes...)[i] == zero[i] || !slices.Contains(commit.Changed, scopes[i]) {
			t.Fatalf("first mutation failed to advance zero scope %+v", scopes[i])
		}
	}
}

func TestNotificationRevisionBoundedReadsAndChangedFanout(t *testing.T) {
	var updateSQL, updateRows uint64
	for _, n := range []int{16, 512} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			store, _ := openTestStore(t, ModeMemory, nil, 0)
			s := nativeHandle(t, store, "root")
			nativePut(t, s, "format", "swarm", `{"version":2}`)
			_, err := s.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
				for i := 0; i < n; i++ {
					id := fmt.Sprintf("m%04d", i)
					if err := tx.Put(RecordKey{"member", id}, json.RawMessage(fmt.Sprintf(`{"id":%q,"name":"old"}`, id))); err != nil {
						return err
					}
				}
				return nil
			})
			nativeMust(t, err)
			recorder := runtimemetrics.New()
			ctx := runtimemetrics.WithRecorder(t.Context(), recorder)
			scopes := notificationScopes()
			nativeMust(t, store.ReadRecordsView(ctx, s.ViewID(), func(r CoordinationReader) error {
				_, err := r.Revisions(scopes)
				return err
			}))
			read := recorder.Snapshot()
			if read.Count(runtimemetrics.SQLStatements) != 3 || read.Count(runtimemetrics.RowsRead) != uint64(2+len(scopes)) || read.Count(runtimemetrics.PayloadBytesRead) != 0 || read.Count(runtimemetrics.RecordsDecoded) != 0 {
				t.Fatalf("revision read scanned payloads or family: %+v", read)
			}
			recorder = runtimemetrics.New()
			ctx = runtimemetrics.WithRecorder(t.Context(), recorder)
			commit, err := s.UpdateRecords(ctx, func(tx CoordinationTx) error {
				return tx.Put(RecordKey{"member", "m0000"}, json.RawMessage(`{"id":"m0000","name":"new"}`))
			})
			nativeMust(t, err)
			var changed []RevisionScope
			for _, scope := range commit.Changed {
				if scope.Index == CoordinationIndexSemanticNews || scope.Index == CoordinationIndexDisplay || scope.Index == CoordinationIndexDisplayFact || scope.Index == CoordinationIndexSemanticMail {
					changed = append(changed, scope)
				}
			}
			want := []RevisionScope{{CoordinationIndexDisplay, ""}, notificationFact("member", "m0000")}
			if !slices.Equal(changed, want) {
				t.Fatalf("label invalidation fanout=%+v", changed)
			}
			cost := recorder.Snapshot()
			if n == 16 {
				updateSQL, updateRows = cost.Count(runtimemetrics.SQLStatements), cost.Count(runtimemetrics.RowsRead)
			} else if cost.Count(runtimemetrics.SQLStatements) != updateSQL || cost.Count(runtimemetrics.RowsRead) != updateRows {
				t.Fatalf("unrelated family growth changed update cost: %+v; first SQL=%d rows=%d", cost, updateSQL, updateRows)
			}
			t.Logf("members=%d revision SQL=%d rows=%d payload=0; label update SQL=%d rows=%d changed notification scopes=%d", n, read.Count(runtimemetrics.SQLStatements), read.Count(runtimemetrics.RowsRead), cost.Count(runtimemetrics.SQLStatements), cost.Count(runtimemetrics.RowsRead), len(changed))
		})
	}
}
