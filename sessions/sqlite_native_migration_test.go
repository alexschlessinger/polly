package sessions

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
)

func nativeSchema9Fixture(t *testing.T, badRecord bool) (string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "schema9.db")
	nativeMust(t, prepareDiskStore(path))
	db, err := sql.Open("sqlite", sqliteDataSource(ModeDisk, path))
	nativeMust(t, err)
	defer db.Close()
	conn, err := db.Conn(t.Context())
	nativeMust(t, err)
	defer conn.Close()
	_, err = conn.ExecContext(t.Context(), `PRAGMA auto_vacuum=INCREMENTAL`)
	nativeMust(t, err)
	nativeMust(t, configureJournal(t.Context(), conn, ModeDisk))
	root := bytes.Repeat([]byte{1}, 16)
	nativeMust(t, runTransaction(t.Context(), conn, "BEGIN IMMEDIATE", func() error {
		for _, migration := range schemaMigrations[:9] {
			if err := migration(t.Context(), conn); err != nil {
				return err
			}
		}
		if _, err := conn.ExecContext(t.Context(), `INSERT INTO sessions(id,name,retention,created_ns,updated_ns,settings_json,next_sequence) VALUES(?,'root','named',10,20,?,1)`, root, []byte(`{"description":"unchanged metadata"}`)); err != nil {
			return err
		}
		if _, err := conn.ExecContext(t.Context(), `INSERT INTO messages VALUES(?,0,?)`, root, []byte(`{"role":"user","content":"history"}`)); err != nil {
			return err
		}
		if _, err := conn.ExecContext(t.Context(), `INSERT INTO swarm_records VALUES(?,'format','swarm',?)`, root, []byte(`{"version":2}`)); err != nil {
			return err
		}
		for i := 0; i < 300; i++ {
			kind := "mail"
			id := fmt.Sprintf("m%04d", i)
			payload := []byte(fmt.Sprintf(`{"to":"recipient-%d","posted":"2026-01-01T00:00:00Z"}`, i%3))
			if badRecord && i == 299 {
				payload = []byte(`{"to":42}`)
			}
			if _, err := conn.ExecContext(t.Context(), `INSERT INTO swarm_records VALUES(?,?,?,?)`, root, kind, id, payload); err != nil {
				return err
			}
		}
		if _, err := conn.ExecContext(t.Context(), `INSERT INTO swarm_records VALUES(?,'unknown','opaque',?)`, root, []byte(" [ true, 42 ] \n")); err != nil {
			return err
		}
		_, err := conn.ExecContext(t.Context(), `PRAGMA user_version=9`)
		return err
	}))
	return path, hex.EncodeToString(root)
}

func TestNativeSchema9MigrationBackfillAndRollback(t *testing.T) {
	t.Run("backfill-cascade", func(t *testing.T) {
		path, id := nativeSchema9Fixture(t, false)
		store, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path, Integrity: IntegrityMetadata})
		nativeMust(t, err)
		defer store.Close()
		nativeMust(t, store.ReadRecordsView(t.Context(), id, func(r CoordinationReader) error {
			i, err := r.Identity()
			nativeMust(t, err)
			if i.Sequence != 1 {
				t.Fatal(i)
			}
			p, err := r.Query(RecordQuery{Index: CoordinationIndexMailRecipient, Key: "recipient-1", Limit: 1, IncludeTotal: true})
			nativeMust(t, err)
			if p.Total != 100 || len(p.Records) != 1 {
				t.Fatal(p)
			}
			v, found, err := r.Get(RecordKey{"unknown", "opaque"})
			nativeMust(t, err)
			if !found || string(v.Payload) != " [ true, 42 ] \n" {
				t.Fatalf("changed bytes %q", v.Payload)
			}
			return nil
		}))
		header, err := store.ReadViewHeader(t.Context(), ViewTarget{ID: id}, "")
		nativeMust(t, err)
		if header.Metadata.Description != "unchanged metadata" || header.Metadata.LastUsed.UnixNano() != 20 {
			t.Fatal(header)
		}
		s := nativeHandle(t, store, "root")
		scope := RevisionScope{CoordinationIndexRecord, CoordinationIndexKey("mail", "m0001")}
		_, err = s.UpdateRecords(t.Context(), func(tx CoordinationTx) error { return tx.Delete(RecordKey{"mail", "m0001"}) })
		nativeMust(t, err)
		deleted := nativeRevisions(t, s, scope)[0]
		nativeMust(t, s.Clear(t.Context()))
		epoch, err := store.ReadTranscriptPage(t.Context(), ViewTarget{ID: id}, TranscriptQuery{Limit: 1})
		nativeMust(t, err)
		nativeMust(t, s.Close())
		nativeMust(t, store.ReadRecordsView(t.Context(), id, func(r CoordinationReader) error {
			rs, err := r.Revisions([]RevisionScope{scope})
			nativeMust(t, err)
			if rs[0] != deleted {
				t.Fatal("tombstone lost", rs, deleted)
			}
			return nil
		}))
		after, err := store.ReadTranscriptPage(t.Context(), ViewTarget{ID: id}, TranscriptQuery{Epoch: epoch.Epoch, Limit: 1})
		nativeMust(t, err)
		if after.Epoch != epoch.Epoch {
			t.Fatal("epoch changed")
		}
		nativeMust(t, store.Delete(t.Context(), "root"))
		for _, table := range []string{"swarm_records", "swarm_relations", "swarm_revisions"} {
			var n int
			nativeMust(t, store.db.QueryRow(`SELECT count(*) FROM `+table).Scan(&n))
			if n != 0 {
				t.Fatalf("cascade leaked %s", table)
			}
		}
	})
	t.Run("failed-backfill-is-transactional", func(t *testing.T) {
		path, _ := nativeSchema9Fixture(t, true)
		store, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path})
		if store != nil {
			store.Close()
		}
		if !errors.Is(err, ErrCoordinationFormat) {
			t.Fatal(err)
		}
		raw, err := sql.Open("sqlite", sqliteDataSource(ModeDisk, path))
		nativeMust(t, err)
		defer raw.Close()
		var version, columns, tables, count int
		nativeMust(t, raw.QueryRow(`PRAGMA user_version`).Scan(&version))
		nativeMust(t, raw.QueryRow(`SELECT count(*) FROM pragma_table_info('sessions') WHERE name='transcript_epoch'`).Scan(&columns))
		nativeMust(t, raw.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='swarm_relations'`).Scan(&tables))
		nativeMust(t, raw.QueryRow(`SELECT count(*) FROM swarm_records`).Scan(&count))
		if version != 9 || columns != 0 || tables != 0 || count != 302 {
			t.Fatalf("partial migration v=%d columns=%d tables=%d rows=%d", version, columns, tables, count)
		}
	})
	t.Run("pre-migration-transcript-gap", func(t *testing.T) {
		path, _ := nativeSchema9Fixture(t, false)
		raw, err := sql.Open("sqlite", sqliteDataSource(ModeDisk, path))
		nativeMust(t, err)
		_, err = raw.Exec(`DELETE FROM messages`)
		nativeMust(t, err)
		nativeMust(t, raw.Close())
		store, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path, Integrity: IntegrityMetadata})
		if store != nil {
			store.Close()
		}
		if err == nil || !strings.Contains(err.Error(), "sequence is corrupt") {
			t.Fatal(err)
		}
		raw, err = sql.Open("sqlite", sqliteDataSource(ModeDisk, path))
		nativeMust(t, err)
		defer raw.Close()
		var v int
		nativeMust(t, raw.QueryRow(`PRAGMA user_version`).Scan(&v))
		if v != 9 {
			t.Fatal(v)
		}
	})
}

func TestNativeSchema10ValidationAndFutureRefusal(t *testing.T) {
	for _, mutation := range []string{
		// Schema 10 owns these index definitions; altered shapes are refused.
		`DROP INDEX sessions_summary_idx; CREATE INDEX sessions_summary_idx ON sessions(updated_ns ASC,name,id)`,
		`DROP INDEX swarm_relations_kind_idx; CREATE INDEX swarm_relations_kind_idx ON swarm_relations(parent_id,index_name,index_key,kind,id)`,
		`ALTER TABLE swarm_revisions RENAME TO invalid_revisions`,
		`DROP TABLE swarm_revisions; PRAGMA user_version=9`,
		`PRAGMA user_version=999`,
	} {
		t.Run(mutation, func(t *testing.T) {
			store, path := openTestStore(t, ModeDisk, nil, 0)
			nativeMust(t, store.Close())
			raw, err := sql.Open("sqlite", sqliteDataSource(ModeDisk, path))
			nativeMust(t, err)
			_, err = raw.Exec(mutation)
			nativeMust(t, err)
			nativeMust(t, raw.Close())
			opened, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path})
			if opened != nil {
				opened.Close()
			}
			if err == nil {
				t.Fatal("accepted malformed/future schema")
			}
		})
	}
}

func TestNativePromotionPreservesEpochRevisionsOwnersAndHandles(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	s := nativeHandle(t, store, "root")
	owner := bytes.Clone(s.ownerToken)
	id := s.ViewID()
	nativePut(t, s, "format", "swarm", `{"version":2}`)
	nativePut(t, s, "mail", "m", `{"to":"recipient"}`)
	scope := RevisionScope{CoordinationIndexMailRecipient, "recipient"}
	revision := nativeRevisions(t, s, scope)[0]
	nativeMust(t, s.Clear(t.Context()))
	nativeMust(t, s.AddMessages(t.Context(), messages.User("history")))
	ref, err := s.ArtifactStore().Put(t.Context(), artifacts.Blob{Kind: artifacts.KindText, Data: []byte("open reader")})
	nativeMust(t, err)
	header, err := store.ReadViewHeader(t.Context(), ViewTarget{ID: id}, "")
	nativeMust(t, err)
	reader, err := header.Artifacts.Open(t.Context(), ref.ID)
	nativeMust(t, err)
	defer reader.Close()
	path := filepath.Join(t.TempDir(), "destination.db")
	destination, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path})
	nativeMust(t, err)
	other := nativeHandle(t, destination, "root")
	nativeMust(t, other.AddMessages(t.Context(), messages.User("unrelated")))
	nativeMust(t, other.Close())
	nativeMust(t, destination.Close())
	nativeMust(t, store.Promote(t.Context(), path))
	if s.ViewID() != id || !bytes.Equal(s.ownerToken, owner) || nativeRevisions(t, s, scope)[0] != revision {
		t.Fatal("promotion changed identity/owner/revision")
	}
	var storedOwner []byte
	nativeMust(t, store.db.QueryRow(`SELECT owner_token FROM session_leases WHERE session_id=?`, s.id).Scan(&storedOwner))
	if !bytes.Equal(storedOwner, owner) {
		t.Fatal("owner changed")
	}
	after, err := store.ReadTranscriptPage(t.Context(), ViewTarget{ID: id}, TranscriptQuery{Epoch: header.Epoch, Limit: 1})
	nativeMust(t, err)
	if after.Messages[0].Content != "history" {
		t.Fatal(after)
	}
	buf := make([]byte, len("open reader"))
	_, err = reader.Read(buf)
	nativeMust(t, err)
	if string(buf) != "open reader" {
		t.Fatal(string(buf))
	}
	nativePut(t, s, "mail", "m", `{"to":"new-recipient"}`)
	if nativeRevisions(t, s, scope)[0] == revision {
		t.Fatal("live handle cannot update promoted scopes")
	}
	mode, location := store.Location()
	if mode != ModeDisk || location != path {
		t.Fatal(mode, location)
	}
}

type nativeUncertainDriver struct {
	base driver.Driver
	fail atomic.Int32
}
type nativeUncertainConn struct {
	driver.Conn
	owner *nativeUncertainDriver
}

func (d *nativeUncertainDriver) Open(name string) (driver.Conn, error) {
	c, err := d.base.Open(name)
	if err != nil {
		return nil, err
	}
	return &nativeUncertainConn{c, d}, nil
}
func (c *nativeUncertainConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	execer, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	result, err := execer.ExecContext(ctx, q, args)
	if err == nil && ((strings.HasPrefix(q, "BEGIN") && c.owner.fail.CompareAndSwap(1, 0)) || (q == "COMMIT" && c.owner.fail.CompareAndSwap(2, 0))) {
		return nil, errors.New("injected uncertain transaction result")
	}
	return result, err
}
func TestNativeUncertainBeginCommitNoCallbackReplay(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	s := nativeHandle(t, store, "root")
	injected := &nativeUncertainDriver{base: store.db.Driver()}
	driverName := fmt.Sprintf("native-uncertain-%d", transactionDriverIDs.Add(1))
	sql.Register(driverName, injected)
	db, err := sql.Open(driverName, sqliteDataSource(ModeMemory, store.path))
	nativeMust(t, err)
	db.SetMaxOpenConns(1)
	nativeMust(t, db.Ping())
	store.dbMu.Lock()
	old := store.db
	store.db = db
	nativeMust(t, old.Close())
	store.dbMu.Unlock()
	calls := 0
	apply := func(tx CoordinationTx) error {
		calls++
		nativeMust(t, tx.Put(RecordKey{"receipt", "effect"}, json.RawMessage(`{"done":true}`)))
		return tx.Effects(CoordinationEffects{Append: messages.User("committed once")})
	}
	injected.fail.Store(1)
	c, err := s.UpdateRecords(t.Context(), apply)
	if err == nil || calls != 0 || c.Sequence != 0 || len(c.Changed) != 0 {
		t.Fatalf("uncertain begin %+v %v calls=%d", c, err, calls)
	}
	injected.fail.Store(2)
	c, err = s.UpdateRecords(t.Context(), apply)
	if err == nil || calls != 1 || c.Sequence != 0 || len(c.Changed) != 0 {
		t.Fatalf("uncertain commit %+v %v calls=%d", c, err, calls)
	}
	// The caller inspects receipts, never blindly reruns the callback.
	nativeMust(t, s.ReadRecords(t.Context(), func(r CoordinationReader) error {
		v, found, err := r.Get(RecordKey{"receipt", "effect"})
		nativeMust(t, err)
		if !found || string(v.Payload) != `{"done":true}` {
			t.Fatal(v)
		}
		i, err := r.Identity()
		if i.Sequence != 1 {
			t.Fatal(i)
		}
		return err
	}))
	c, err = s.UpdateRecords(t.Context(), func(CoordinationTx) error { return nil })
	nativeMust(t, err)
	if c.Sequence != 1 || calls != 1 {
		t.Fatal("callback replayed")
	}
}

func TestNativeSQLiteConfiguration(t *testing.T) {
	store, _ := openTestStore(t, ModeDisk, nil, 0)
	var version, journal string
	var synchronous, foreignKeys int
	nativeMust(t, store.db.QueryRow(`SELECT sqlite_version()`).Scan(&version))
	nativeMust(t, store.db.QueryRow(`PRAGMA journal_mode`).Scan(&journal))
	nativeMust(t, store.db.QueryRow(`PRAGMA synchronous`).Scan(&synchronous))
	nativeMust(t, store.db.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys))
	if journal != "wal" || synchronous != 2 || foreignKeys != 1 || store.db.Stats().MaxOpenConnections != 1 {
		t.Fatalf("changed SQLite configuration: %s %d %d %+v", journal, synchronous, foreignKeys, store.db.Stats())
	}
	var options []string
	nativeMust(t, queryEach(t.Context(), store.db, `PRAGMA compile_options`, nil, func(rows *sql.Rows) error {
		var option string
		if err := rows.Scan(&option); err != nil {
			return err
		}
		options = append(options, option)
		return nil
	}))
	t.Logf("sqlite=%s journal=%s synchronous=%d foreign_keys=%d max_connections=1 compile_options=%s", version, journal, synchronous, foreignKeys, strings.Join(options, ","))
}

func TestNativeRevisionsAcrossProcesses(t *testing.T) {
	skipIfWindows(t)
	const helperPath = "POLLYTOOL_TEST_LOCK_DATABASE"
	if path := os.Getenv(helperPath); path != "" {
		store, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path})
		nativeMust(t, err)
		defer store.Close()
		session, err := store.Acquire(t.Context(), "remote-child", AcquireOptions{Parent: "root"})
		nativeMust(t, err)
		defer session.Close()
		s := session.(*sqliteSession)
		nativePut(t, s, "receipt", "remote", `{"persisted":true}`)
		rs := nativeRevisions(t, s, RevisionScope{CoordinationIndexRecord, CoordinationIndexKey("receipt", "remote")})
		fmt.Println("NATIVE_REVISION=" + rs[0].Token)
		return
	}
	store, path := openTestStore(t, ModeDisk, nil, 0)
	s := nativeHandle(t, store, "root")
	scope := RevisionScope{CoordinationIndexRecord, CoordinationIndexKey("receipt", "remote")}
	before := nativeRevisions(t, s, scope)[0]
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestNativeRevisionsAcrossProcesses$")
	child.Env = append(os.Environ(), helperPath+"="+path)
	output, err := child.CombinedOutput()
	if err != nil {
		t.Fatalf("child: %v\n%s", err, output)
	}
	after := nativeRevisions(t, s, scope)[0]
	if after == before || !strings.Contains(string(output), "NATIVE_REVISION="+after.Token) {
		t.Fatalf("cross-process tokens before=%+v after=%+v output=%s", before, after, output)
	}
	nativeMust(t, s.Close())
	nativeMust(t, store.Close())
	reopened, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path})
	nativeMust(t, err)
	defer reopened.Close()
	nativeMust(t, reopened.ReadRecordsView(t.Context(), s.ViewID(), func(r CoordinationReader) error {
		rs, err := r.Revisions([]RevisionScope{scope})
		if rs[0] != after {
			t.Fatal("reopen token changed")
		}
		return err
	}))
	nativeMust(t, reopened.Delete(t.Context(), "root"))
	replacement := nativeHandle(t, reopened, "root")
	if nativeRevisions(t, replacement, scope)[0].Token == after.Token {
		t.Fatal("family delete/recreate ABA")
	}
}
