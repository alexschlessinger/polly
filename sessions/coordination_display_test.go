package sessions

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func displayReadFor(t *testing.T, s *sqliteSession, mark string) DisplayChanges {
	t.Helper()
	var out DisplayChanges
	nativeMust(t, s.ReadRecords(t.Context(), func(raw CoordinationReader) error {
		var err error
		out, err = raw.(CoordinationDisplayReader).DisplayChanges(mark)
		return err
	}))
	return out
}
func TestDisplayChangesStagingRollbackNoopABA(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	s := nativeHandle(t, store, "root")
	nativePut(t, s, "format", "swarm", `{"version":2}`)
	nativePut(t, s, "member", "m", `{"id":"m","name":"A"}`)
	first := displayReadFor(t, s, "")
	if !first.Reset {
		t.Fatal("cold read not marked reset")
	}
	nativePut(t, s, "member", "m", `{"id":"m","name":"A"}`)
	noop := displayReadFor(t, s, first.Mark)
	if noop.Mark != first.Mark || len(noop.Records) != 0 {
		t.Fatal("no-op invalidated")
	}
	var staged DisplayChanges
	var escaped CoordinationDisplayReader
	veto := errors.New("rollback")
	_, err := s.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
		if err := tx.Put(RecordKey{"member", "m"}, json.RawMessage(`{"id":"m","name":"rolled back"}`)); err != nil {
			return err
		}
		escaped = tx.(CoordinationDisplayReader)
		var err error
		staged, err = escaped.DisplayChanges(first.Mark)
		if err != nil {
			return err
		}
		if staged.Mark == first.Mark || len(staged.Records) != 1 {
			t.Fatalf("staged delta=%+v", staged)
		}
		return veto
	})
	if !errors.Is(err, veto) {
		t.Fatal(err)
	}
	if _, err = escaped.DisplayChanges(""); !errors.Is(err, ErrCoordinationClosed) {
		t.Fatalf("escaped reader: %v", err)
	}
	if got := displayReadFor(t, s, first.Mark); got.Mark != first.Mark || len(got.Records) != 0 {
		t.Fatal("rollback changed display")
	}
	// Commit a different write with the same numeric stamp. The rolled-back
	// random mark must be rejected, including after another later commit.
	nativePut(t, s, "member", "m", `{"id":"m","name":"B"}`)
	if got := displayReadFor(t, s, staged.Mark); !got.Reset {
		t.Fatal("rolled-back mark aliased committed data")
	}
	nativePut(t, s, "member", "m", `{"id":"m","name":"A"}`)
	if got := displayReadFor(t, s, staged.Mark); !got.Reset {
		t.Fatal("older rolled-back mark skipped committed data")
	}
	aba := displayReadFor(t, s, first.Mark)
	if aba.Mark == first.Mark || len(aba.Records) != 1 {
		t.Fatal("ABA was not visible")
	}
	_, err = s.UpdateRecords(t.Context(), func(tx CoordinationTx) error { return tx.Delete(RecordKey{"member", "m"}) })
	nativeMust(t, err)
	removed := displayReadFor(t, s, aba.Mark)
	memberValue := func(v DisplayChanges) (json.RawMessage, bool) {
		for _, r := range v.Records {
			if r.Kind == "member" && r.ID == "m" {
				return r.Value, true
			}
		}
		return nil, false
	}
	if value, found := memberValue(removed); !found || value != nil {
		t.Fatal("missing delete tombstone")
	}
	nativePut(t, s, "member", "m", `{"id":"m","name":"A"}`)
	if value, found := memberValue(displayReadFor(t, s, removed.Mark)); !found || value == nil {
		t.Fatal("reinsert missing")
	}
}

func TestDisplayProjectionPromotionMigrationAndShape(t *testing.T) {
	t.Run("promotion-reopen-delete", func(t *testing.T) {
		store, _ := openTestStore(t, ModeMemory, nil, 0)
		s := nativeHandle(t, store, "root")
		nativePut(t, s, "format", "swarm", `{"version":2}`)
		nativePut(t, s, "member", "m", `{"id":"m","execution":"e"}`)
		nativePut(t, s, "execution", "e", `{"id":"e","member":"m","status":"completed","request":{"callID":"call"},"intent":[{"role":"user","content":"secret intent"}],"result":{"value":"secret output"}}`)
		first := displayReadFor(t, s, "")
		path := filepath.Join(t.TempDir(), "display.db")
		nativeMust(t, store.Promote(t.Context(), path))
		if displayReadFor(t, s, first.Mark).Mark != first.Mark {
			t.Fatal("promotion changed mark")
		}
		nativeMust(t, s.Close())
		nativeMust(t, store.Close())
		reopened, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path})
		nativeMust(t, err)
		defer reopened.Close()
		s = nativeHandle(t, reopened, "root")
		if got := displayReadFor(t, s, first.Mark); got.Mark != first.Mark || len(got.Records) != 0 {
			t.Fatal("reopen changed display")
		}
		nativeMust(t, reopened.withRead(t.Context(), func(conn *sql.Conn) error {
			var source, fact []byte
			if err := conn.QueryRowContext(t.Context(), `SELECT value FROM swarm_display_sources WHERE parent_id=? AND kind='execution' AND id='e'`, s.id).Scan(&source); err != nil {
				return err
			}
			if err := conn.QueryRowContext(t.Context(), `SELECT value FROM swarm_display_facts WHERE parent_id=? AND kind='member' AND id='m'`, s.id).Scan(&fact); err != nil {
				return err
			}
			if strings.Contains(string(source)+string(fact), "secret") {
				t.Fatal("source blobs entered display storage")
			}
			return nil
		}))
		nativeMust(t, s.Close())
		nativeMust(t, reopened.withWrite(t.Context(), func(conn *sql.Conn) error {
			_, err := conn.ExecContext(t.Context(), `DELETE FROM sessions WHERE id=?`, s.id)
			return err
		}))
		nativeMust(t, reopened.withRead(t.Context(), func(conn *sql.Conn) error {
			for table := range displayTables {
				var n int
				if err := conn.QueryRowContext(t.Context(), `SELECT count(*) FROM `+table).Scan(&n); err != nil {
					return err
				}
				if n != 0 {
					t.Fatalf("%s retained %d rows", table, n)
				}
			}
			return nil
		}))
	})
	t.Run("migration", func(t *testing.T) {
		path, root := nativeSchema9Fixture(t, false)
		db, err := sql.Open("sqlite", sqliteDataSource(ModeDisk, path))
		nativeMust(t, err)
		id, err := hex.DecodeString(root)
		nativeMust(t, err)
		for _, r := range []struct{ kind, id, value string }{{"member", "m", `{"id":"m","name":"m","execution":"e"}`}, {"execution", "e", `{"id":"e","member":"m","status":"completed","request":{"callID":"old"}}`}} {
			_, err = db.Exec(`INSERT INTO swarm_records VALUES(?,?,?,?)`, id, r.kind, r.id, []byte(r.value))
			nativeMust(t, err)
		}
		nativeMust(t, db.Close())
		store, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path})
		nativeMust(t, err)
		defer store.Close()
		nativeMust(t, store.ReadRecordsView(t.Context(), root, func(raw CoordinationReader) error {
			v, err := raw.(CoordinationDisplayReader).DisplayChanges("")
			if err != nil {
				return err
			}
			var member, launch bool
			for _, r := range v.Records {
				member = member || r.Kind == "member" && r.ID == "m"
				launch = launch || r.Kind == "launch" && r.ID == "e"
			}
			if !member || !launch {
				t.Fatal("migration omitted descriptors")
			}
			return nil
		}))
	})
	for _, rewound := range []bool{false, true} {
		t.Run("incomplete-"+map[bool]string{false: "current", true: "rewound"}[rewound], func(t *testing.T) {
			store, path := openTestStore(t, ModeDisk, nil, 0)
			nativeMust(t, store.Close())
			db, err := sql.Open("sqlite", sqliteDataSource(ModeDisk, path))
			nativeMust(t, err)
			_, err = db.Exec(`DROP TABLE swarm_display_marks`)
			nativeMust(t, err)
			if rewound {
				_, err = db.Exec(`PRAGMA user_version=9`)
				nativeMust(t, err)
			}
			nativeMust(t, db.Close())
			reopened, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path})
			if err == nil {
				reopened.Close()
				t.Fatal("incomplete display shape silently repaired")
			}
		})
	}
}
