package sessions

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
)

// whileWriting runs fn while a transaction on the store's writing connection
// is open and has added a message to the session that it has not committed.
// It fails the test when fn waits for the writer.
func whileWriting(t *testing.T, store *SQLiteStore, session Session, fn func(context.Context) error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id := session.(*sqliteSession).id
	writing, release, written := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		written <- store.withTransactionNow(ctx, "BEGIN IMMEDIATE", func(conn *sql.Conn) error {
			if _, err := conn.ExecContext(ctx, "INSERT INTO messages(session_id,sequence,payload_json) VALUES(?,?,?)", id, 1000, []byte(`{}`)); err != nil {
				return err
			}
			close(writing)
			<-release
			// Rolled back: the message was only there to be unseen.
			return context.Canceled
		})
	}()
	select {
	case <-writing:
	case err := <-written:
		t.Fatalf("the write did not begin: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- fn(ctx) }()
	var err error
	select {
	case err = <-done:
	case <-time.After(3 * time.Second):
		close(release)
		<-written
		t.Fatal("a read waited for the store's writing connection")
	}
	close(release)
	<-written
	if err != nil {
		t.Fatal(err)
	}
}

// historyWhileWriting reads the session's transcript during a write, and
// returns what the read saw.
func historyWhileWriting(t *testing.T, store *SQLiteStore, session Session) (history []messages.ChatMessage) {
	t.Helper()
	whileWriting(t, store, session, func(ctx context.Context) (err error) {
		history, err = session.GetHistory(ctx)
		return err
	})
	return history
}

// A disk store reads a transcript through a connection of its own: the read
// neither waits for a write in progress nor sees what it has not committed.
func TestTranscriptReadDoesNotWaitForAWrite(t *testing.T) {
	ctx := context.Background()
	store, _ := openTestStore(t, ModeDisk, nil, 0)
	session, err := store.Acquire(ctx, "reader", AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err := session.AddMessage(ctx, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "saved"}); err != nil {
		t.Fatal(err)
	}
	history := historyWhileWriting(t, store, session)
	if len(history) != 1 || history[0].Content != "saved" {
		t.Fatalf("history read during a write = %+v", history)
	}
}

// A store promoted to disk gets the readers a disk store opens with.
func TestPromotedStoreReadsThroughReaders(t *testing.T) {
	ctx := context.Background()
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	if store.readers != nil {
		t.Fatal("a memory store has connections that only read")
	}
	session, err := store.Acquire(ctx, "promoted", AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err := session.AddMessage(ctx, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "before"}); err != nil {
		t.Fatal(err)
	}
	if history, err := session.GetHistory(ctx); err != nil || len(history) != 1 {
		t.Fatalf("history in memory = %+v, %v", history, err)
	}
	if err := store.Promote(ctx, filepath.Join(t.TempDir(), "polly.db")); err != nil {
		t.Fatal(err)
	}
	if store.readers == nil {
		t.Fatal("a promoted store has no connections that only read")
	}
	if err := session.AddMessage(ctx, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "after"}); err != nil {
		t.Fatal(err)
	}
	history := historyWhileWriting(t, store, session)
	if len(history) != 2 || history[0].Content != "before" || history[1].Content != "after" {
		t.Fatalf("history read during a write = %+v", history)
	}
}

// The readers are configured to read only, and are few.
func TestReadersCannotWrite(t *testing.T) {
	ctx := context.Background()
	store, _ := openTestStore(t, ModeDisk, nil, 0)
	for pragma, want := range map[string]int64{
		"query_only":     1,
		"cache_size":     -readCacheKiB,
		"foreign_keys":   1,
		"busy_timeout":   10_000,
		"trusted_schema": 0,
		"temp_store":     2,
	} {
		var got int64
		if err := store.readers.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&got); err != nil || got != want {
			t.Fatalf("a reader's PRAGMA %s = %d, %v; want %d", pragma, got, err, want)
		}
	}
	var journal string
	if err := store.readers.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal); err != nil || journal != "wal" {
		t.Fatalf("a reader's journal mode = %q, %v", journal, err)
	}
	if _, err := store.readers.ExecContext(ctx, "DELETE FROM messages"); err == nil {
		t.Fatal("a reader wrote to the database")
	}
	if stats := store.readers.Stats(); stats.MaxOpenConnections != readConnections {
		t.Fatalf("the readers may open %d connections, want %d", stats.MaxOpenConnections, readConnections)
	}
}

// A session's cache identity confirms the lease without waiting for a write:
// a handle whose lease is fresh asks nobody, and one that cannot be sure
// reads the stored lease through the readers.
func TestCacheSessionIDDoesNotWaitForAWrite(t *testing.T) {
	ctx := context.Background()
	store, _ := openTestStore(t, ModeDisk, nil, 0)
	session, err := store.Acquire(ctx, "cached", AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	handle := session.(*sqliteSession)
	want, err := session.CacheSessionID(ctx)
	if err != nil || want == "" {
		t.Fatalf("cache identity = %q, %v", want, err)
	}
	for _, fresh := range []bool{true, false} {
		if !fresh {
			handle.leaseExpiry.Store(0)
		}
		if handle.leaseFresh() != fresh {
			t.Fatalf("the handle's lease is fresh = %v, want %v", handle.leaseFresh(), fresh)
		}
		whileWriting(t, store, session, func(ctx context.Context) error {
			got, err := session.CacheSessionID(ctx)
			if err == nil && got != want {
				t.Errorf("cache identity changed to %q", got)
			}
			return err
		})
	}
	// A handle that cannot be sure finds out that its lease is gone.
	if _, err := store.db.ExecContext(ctx, "DELETE FROM session_leases WHERE session_id = ?", handle.id); err != nil {
		t.Fatal(err)
	}
	if _, err := session.CacheSessionID(ctx); !errors.Is(err, ErrSessionLeaseLost) {
		t.Fatalf("cache identity without a lease: %v", err)
	}
}

// The store's reads that need no lease, a handle's read of its own row and a
// read of an artifact go through the readers: none waits for a write.
func TestStoreReadsDoNotWaitForAWrite(t *testing.T) {
	ctx := context.Background()
	store, _ := openTestStore(t, ModeDisk, nil, 0)
	session, err := store.Acquire(ctx, "reader", AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err := session.AddMessage(ctx, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "saved"}); err != nil {
		t.Fatal(err)
	}
	// Larger than one chunk, so the read asks for several.
	stored := bytes.Repeat([]byte("artifact bytes "), 100_000)
	ref, err := session.ArtifactStore().Put(ctx, artifacts.Blob{Kind: artifacts.KindText, Data: stored})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutModelCache(ctx, "models", []byte("cached")); err != nil {
		t.Fatal(err)
	}
	for name, read := range map[string]func(context.Context) error{
		"List": func(ctx context.Context) error {
			names, err := store.List(ctx)
			if err == nil && !slices.Equal(names, []string{"reader"}) {
				err = fmt.Errorf("names = %q", names)
			}
			return err
		},
		"Exists": func(ctx context.Context) error {
			exists, err := store.Exists(ctx, "reader")
			if err == nil && !exists {
				err = errors.New("the session does not exist")
			}
			return err
		},
		"GetMetadata by name": func(ctx context.Context) error {
			metadata, err := store.GetMetadata(ctx, "reader")
			if err == nil && metadata.Name != "reader" {
				err = fmt.Errorf("metadata of %q", metadata.Name)
			}
			return err
		},
		"ListSummaries": func(ctx context.Context) error {
			summaries, err := store.ListSummaries(ctx)
			if err == nil && (len(summaries) != 1 || summaries[0].MessageCount != 1 || !summaries[0].InUse) {
				err = fmt.Errorf("summaries = %+v", summaries)
			}
			return err
		},
		"GetLast": func(ctx context.Context) error {
			last, err := store.GetLast(ctx)
			if err == nil && last != "reader" {
				err = fmt.Errorf("last = %q", last)
			}
			return err
		},
		"GetModelCache": func(ctx context.Context) error {
			data, err := store.GetModelCache(ctx, "models")
			if err == nil && string(data) != "cached" {
				err = fmt.Errorf("cache = %q", data)
			}
			return err
		},
		"a handle's GetMetadata": func(ctx context.Context) error {
			metadata, err := session.GetMetadata(ctx)
			if err == nil && metadata.Name != "reader" {
				err = fmt.Errorf("metadata of %q", metadata.Name)
			}
			return err
		},
		"an artifact": func(ctx context.Context) error {
			reader, err := session.ArtifactStore().Open(ctx, ref.ID)
			if err != nil {
				return err
			}
			defer reader.Close()
			data, err := io.ReadAll(reader)
			if err == nil && !bytes.Equal(data, stored) {
				err = fmt.Errorf("read %d bytes of an artifact of %d", len(data), len(stored))
			}
			return err
		},
	} {
		t.Run(name, func(t *testing.T) { whileWriting(t, store, session, read) })
	}
}

// Whatever does use the writing connection waits for its turn at it: the
// writes that are one statement, and every read of a memory store, which has
// no other connection.
func TestWritingConnectionIsUsedInTurn(t *testing.T) {
	ctx := context.Background()
	for kind, mode := range map[string]StoreMode{"disk": ModeDisk, "memory": ModeMemory} {
		store, _ := openTestStore(t, mode, nil, 0)
		session, err := store.Acquire(ctx, "turns", AcquireOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer session.Close()
		uses := map[string]func(context.Context) error{
			"PutModelCache": func(ctx context.Context) error { return store.PutModelCache(ctx, "models", []byte("cached")) },
			"Sync":          store.Sync,
		}
		if mode == ModeMemory {
			// Sync has no disk to wait for.
			delete(uses, "Sync")
			uses["Exists"] = func(ctx context.Context) error { _, err := store.Exists(ctx, "turns"); return err }
			uses["List"] = func(ctx context.Context) error { _, err := store.List(ctx); return err }
			uses["a handle's GetMetadata"] = func(ctx context.Context) error { _, err := session.GetMetadata(ctx); return err }
		}
		for name, use := range uses {
			t.Run(kind+"/"+name, func(t *testing.T) {
				if err := store.enter(ctx); err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() { done <- use(ctx) }()
				select {
				case err := <-done:
					store.leave()
					t.Fatalf("it did not wait for its turn: %v", err)
				case <-time.After(30 * time.Millisecond):
				}
				store.leave()
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("it did not run once it had its turn")
				}
			})
		}
	}
}
