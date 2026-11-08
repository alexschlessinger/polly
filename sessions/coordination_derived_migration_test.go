package sessions

import (
	"bytes"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
)

func TestDerivedMigrationAndAtomicRollback(t *testing.T) {
	path, root := nativeSchema9Fixture(t, false)
	raw, err := sql.Open("sqlite", sqliteDataSource(ModeDisk, path))
	nativeMust(t, err)
	id, err := hex.DecodeString(root)
	nativeMust(t, err)
	records := map[RecordKey]string{
		{"member", "reader"}:           `{"execution":"e","publications":{"posted":"2026-01-01T00:00:00Z","id":"past"}}`,
		{"execution", "e"}:             `{"run":"r","status":"running"}`,
		{"publication", "z-old"}:       `{"run":"r","kind":"host","author":"writer","key":"k","value":"v","posted":"2025-01-01T00:00:00Z"}`,
		{"publication", "a-duplicate"}: `{"run":"r","kind":"host","author":"writer","key":"k","value":"v","duplicateOf":"z-old","posted":"2025-01-01T00:00:01Z"}`,
		{"publication", "b-withdraw"}:  `{"run":"r","kind":"host","author":"writer","key":"k","value":"other","supersedes":"z-old","posted":"2025-01-01T00:00:02Z"}`,
	}
	for k, p := range records {
		_, err = raw.Exec(`INSERT INTO swarm_records VALUES(?,?,?,?)`, id, k.Kind, k.ID, []byte(p))
		nativeMust(t, err)
	}
	nativeMust(t, raw.Close())
	store, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path})
	nativeMust(t, err)
	defer store.Close()
	nativeMust(t, store.ReadRecordsView(t.Context(), root, func(r CoordinationReader) error {
		got := derivedIDs(t, r, CoordinationIndexPublicationPending, "reader")
		if !slices.Equal(got, []string{"a-duplicate"}) {
			t.Fatal(got)
		}
		for k, p := range records {
			v, ok, err := r.Get(k)
			nativeMust(t, err)
			if !ok || !bytes.Equal(v.Payload, []byte(p)) {
				t.Fatal("migration changed domain bytes", k)
			}
		}
		return nil
	}))
	s := nativeHandle(t, store, "root")
	nativePut(t, s, "publication", "third", `{"run":"r","kind":"host","author":"writer","key":"k","value":"v","duplicateOf":"z-old","posted":"2025-01-01T00:00:03Z"}`)
	_, err = store.db.Exec(`CREATE TEMP TRIGGER fail_pending BEFORE INSERT ON swarm_relations WHEN NEW.index_name='derived.publication.pending' BEGIN SELECT RAISE(ABORT,'injected derived failure'); END`)
	nativeMust(t, err)
	_, err = s.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
		// Swallowing a failed promotion cannot publish any half-derived state.
		_ = tx.Put(RecordKey{"publication", "withdraw-duplicate"}, json.RawMessage(`{"kind":"host","supersedes":"a-duplicate"}`))
		return nil
	})
	if err == nil {
		t.Fatal("derived failure committed")
	}
	_, err = store.db.Exec(`DROP TRIGGER fail_pending`)
	nativeMust(t, err)
	checkDerivedParity(t, s, CoordinationIndexPublicationPending, "reader", "a-duplicate")
	nativeMust(t, s.ReadRecords(t.Context(), func(r CoordinationReader) error {
		_, ok, err := r.Get(RecordKey{"publication", "withdraw-duplicate"})
		if ok {
			t.Fatal("partial publication")
		}
		return err
	}))
	veto := errors.New("veto")
	calls := 0
	_, err = s.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
		calls++
		if err := tx.Put(RecordKey{"publication", "withdraw-duplicate"}, json.RawMessage(`{"kind":"host","supersedes":"a-duplicate"}`)); err != nil {
			return err
		}
		return veto
	})
	if !errors.Is(err, veto) || calls != 1 {
		t.Fatalf("retry/veto calls=%d err=%v", calls, err)
	}
	checkDerivedParity(t, s, CoordinationIndexPublicationPending, "reader", "a-duplicate")
}

func TestDerivedPublicationAudienceFanoutIsExplicit(t *testing.T) {
	for _, size := range []int{16, 64} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			store, _ := openTestStore(t, ModeMemory, nil, 0)
			s := nativeHandle(t, store, "root")
			nativePut(t, s, "format", "swarm", `{"version":2}`)
			_, err := s.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
				for i := 0; i < size; i++ {
					if err := tx.Put(RecordKey{"member", fmt.Sprintf("reader-%03d", i)}, json.RawMessage(`{}`)); err != nil {
						return err
					}
				}
				return nil
			})
			nativeMust(t, err)
			recorder := runtimemetrics.New()
			_, err = s.UpdateRecords(runtimemetrics.WithRecorder(t.Context(), recorder), func(tx CoordinationTx) error {
				return tx.Put(RecordKey{"publication", "host"}, json.RawMessage(`{"kind":"host","author":"reader-000","posted":"2026-01-01T00:00:00Z"}`))
			})
			nativeMust(t, err)
			var links int
			nativeMust(t, store.db.QueryRow(`SELECT count(*) FROM swarm_relations WHERE parent_id=? AND index_name=?`, s.id, string(CoordinationIndexPublicationPending)).Scan(&links))
			// size-1 members (author excluded), plus the implicit parent.
			if links != size {
				t.Fatalf("audience links=%d want %d", links, size)
			}
			counts := recorder.Snapshot()
			t.Logf("eligible-readers=%d pending-links=%d publication-write statements=%d rows=%d bytes=%d (intentional audience fanout)", size, links, counts.Count(runtimemetrics.SQLStatements), counts.Count(runtimemetrics.RowsRead), counts.Count(runtimemetrics.PayloadBytesRead))
		})
	}
}
