package sessions

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
)

// historyWhileWriting reads the session's transcript while a transaction on
// the store's writing connection is open and has added a message it has not
// committed. It returns what the read saw, or fails the test when the read
// waits for the writer.
func historyWhileWriting(t *testing.T, store *SQLiteStore, session Session) []messages.ChatMessage {
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
	type read struct {
		history []messages.ChatMessage
		err     error
	}
	done := make(chan read, 1)
	go func() {
		history, err := session.GetHistory(ctx)
		done <- read{history, err}
	}()
	var got read
	select {
	case got = <-done:
	case <-time.After(3 * time.Second):
		close(release)
		<-written
		t.Fatal("a transcript read waited for the store's writing connection")
	}
	close(release)
	<-written
	if got.err != nil {
		t.Fatal(got.err)
	}
	return got.history
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
