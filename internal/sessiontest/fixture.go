package sessiontest

import (
	"context"
	"database/sql"
	"encoding/hex"
	"path/filepath"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/sessions"
)

// Fixture opens fresh store handles on the same backing bytes. Hooks model
// external lease takeover and a failed commit, not failures inside the callback.
type Fixture struct {
	Open       func() sessions.SessionStore
	Revoke     func(string)
	FailCommit func() func()
	HasBlob    func(string) bool
	ExpireNow  func(string)
}

type Factory struct {
	Name string
	New  func(*testing.T) *Fixture
}

func Factories() []Factory { return []Factory{{"sqlite", SQLite}, {"file", File}} }

func SQLite(t *testing.T) *Fixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sessions.db")
	open := func() sessions.SessionStore {
		t.Helper()
		s, err := sessions.OpenStore(sessions.StoreConfig{Mode: sessions.ModeDisk, Path: path})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	}
	// Initialize the schema before opening the fault-injection connection.
	s := open()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(context.Background(), query, args...); err != nil {
			t.Fatal(err)
		}
	}
	return &Fixture{Open: open, Revoke: func(id string) {
		exec(`UPDATE session_leases SET owner_token=randomblob(16), expires_ns=0 WHERE hex(session_id)=upper(?)`, id)
	}, FailCommit: func() func() {
		// A deferred foreign key lets all checkpoint statements succeed, then makes
		// SQLite itself reject COMMIT. The transaction must still be rolled back.
		exec(`CREATE TABLE contract_parent (id INTEGER PRIMARY KEY)`)
		exec(`CREATE TABLE contract_deferred (id INTEGER REFERENCES contract_parent(id) DEFERRABLE INITIALLY DEFERRED)`)
		exec(`CREATE TRIGGER contract_commit_failure AFTER INSERT ON messages BEGIN INSERT INTO contract_deferred VALUES (1); END`)
		return func() {
			exec(`DROP TRIGGER contract_commit_failure`)
			exec(`DROP TABLE contract_deferred`)
			exec(`DROP TABLE contract_parent`)
		}
	}, ExpireNow: func(id string) {
		exec(`UPDATE sessions SET ttl_ns=1, updated_ns=? WHERE hex(id)=upper(?)`, time.Now().Add(-time.Hour).UnixNano(), id)
	}, HasBlob: func(id string) bool {
		digest, err := hex.DecodeString(id[len("sha256:"):])
		if err != nil {
			t.Fatal(err)
		}
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM artifact_blobs WHERE digest=?`, digest).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count > 0
	}}
}

func File(t *testing.T) *Fixture {
	t.Helper()
	db := &fileDatabase{path: filepath.Join(t.TempDir(), "sessions.json"), leases: map[string]*fileSession{}}
	return &Fixture{Open: func() sessions.SessionStore {
		s := &fileStore{db: db}
		t.Cleanup(func() { s.Close() })
		return s
	}, Revoke: func(id string) {
		db.mu.Lock()
		defer db.mu.Unlock()
		delete(db.leases, id)
	}, FailCommit: func() func() {
		db.mu.Lock()
		db.failCommit = true
		db.mu.Unlock()
		return func() { db.mu.Lock(); db.failCommit = false; db.mu.Unlock() }
	}, ExpireNow: func(id string) {
		db.mu.Lock()
		defer db.mu.Unlock()
		v, err := db.load()
		must(t, err)
		v.Sessions[id].Metadata.TTL = time.Nanosecond
		v.Sessions[id].Metadata.LastUsed = time.Now().Add(-time.Hour)
		must(t, db.save(v))
	}, HasBlob: func(id string) bool {
		db.mu.Lock()
		defer db.mu.Unlock()
		v, err := db.load()
		if err != nil {
			t.Fatal(err)
		}
		_, ok := v.Blobs[id]
		return ok
	}}
}
