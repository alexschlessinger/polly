package sessions

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
)

// changeRecord is an UpdateCoordinationFrom callback that sets one record and
// appends the given messages to the caller's transcript.
func changeRecord(kind, id, value string, appended ...string) func(*CoordinationState) error {
	return func(state *CoordinationState) error {
		state.Changes = map[string]map[string]json.RawMessage{kind: {id: json.RawMessage(value)}}
		for _, text := range appended {
			state.Append = append(state.Append, messages.ChatMessage{Role: messages.MessageRoleUser, Content: text})
		}
		return nil
	}
}

// updateFrom applies fn at the family's current revision.
func updateFrom(t *testing.T, coord CoordinationSession, fn func(*CoordinationState) error) {
	t.Helper()
	ctx := context.Background()
	revision, err := coord.(CoordinationRevisionSession).CoordinationRevision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := coord.(CoordinationDeltaSession).UpdateCoordinationFrom(ctx, revision, fn); err != nil {
		t.Fatal(err)
	}
}

func accepted(store *SQLiteStore) uint64 {
	store.writer.mu.Lock()
	defer store.writer.mu.Unlock()
	return store.writer.accepted
}

func historyText(t *testing.T, session Session) string {
	t.Helper()
	history, err := session.GetHistory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, message := range history {
		texts = append(texts, message.Content)
	}
	return strings.Join(texts, ",")
}

// An update answered from memory is queued for the writer, and everything
// else the store does waits for the queue, so it sees the update's rows in
// the order the updates were accepted.
func TestCoordinationUpdatesAreWrittenBehind(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "polly.db")
	store, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	sqlite := store
	parent, err := store.Acquire(ctx, "parent", AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	coord := parent.(CoordinationSession)
	// The first update goes to the database and leaves the family in memory.
	if err := coord.UpdateCoordination(ctx, setRecord("task", "a", `{"n":1}`)); err != nil {
		t.Fatal(err)
	}
	before := accepted(sqlite)
	updateFrom(t, coord, changeRecord("task", "a", `{"n":2}`, "one"))
	updateFrom(t, coord, changeRecord("task", "b", `{"n":3}`, "two", "three"))
	if got := accepted(sqlite) - before; got != 2 {
		t.Fatalf("%d of 2 updates were queued for the writer", got)
	}
	if got := readRecord(t, coord, "task", "a"); got != `{"n":2}` {
		t.Fatalf("a read after an accepted update = %q", got)
	}
	// A write that goes straight to the database lands after the queue.
	if err := parent.AddMessage(ctx, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "four"}); err != nil {
		t.Fatal(err)
	}
	updateFrom(t, coord, changeRecord("task", "c", `{"n":4}`, "five"))
	if got := historyText(t, parent); got != "one,two,three,four,five" {
		t.Fatalf("transcript = %q", got)
	}
	// Closing writes what is still queued.
	updateFrom(t, coord, changeRecord("task", "a", `{"n":5}`, "six"))
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	parent, err = reopened.Acquire(ctx, "parent", AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	coord = parent.(CoordinationSession)
	for id, want := range map[string]string{"a": `{"n":5}`, "b": `{"n":3}`, "c": `{"n":4}`} {
		if got := readRecord(t, coord, "task", id); got != want {
			t.Fatalf("task %s after reopening = %q, want %q", id, got, want)
		}
	}
	if got := historyText(t, parent); got != "one,two,three,four,five,six" {
		t.Fatalf("transcript after reopening = %q", got)
	}
}

// Sync covers updates that are still queued.
func TestSyncWritesQueuedUpdates(t *testing.T) {
	ctx := context.Background()
	store, _ := openTestStore(t, ModeDisk, nil, 0)
	parent, err := store.Acquire(ctx, "parent", AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	coord := parent.(CoordinationSession)
	if err := coord.UpdateCoordination(ctx, setRecord("task", "a", `{"n":1}`)); err != nil {
		t.Fatal(err)
	}
	for n := 2; n <= 50; n++ {
		updateFrom(t, coord, changeRecord("task", "a", fmt.Sprintf(`{"n":%d}`, n)))
	}
	if err := store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	var stored []byte
	if err := store.withTransactionNow(ctx, "BEGIN", func(conn *sql.Conn) error {
		return conn.QueryRowContext(ctx, `SELECT payload_json FROM swarm_records WHERE kind='task' AND id='a'`).Scan(&stored)
	}); err != nil {
		t.Fatal(err)
	}
	if string(stored) != `{"n":50}` {
		t.Fatalf("stored after Sync = %q", stored)
	}
}

// When accepted updates cannot be written the store stops taking more: the
// callers were told they succeeded, and the database does not have them.
func TestCoordinationStopsWhenQueuedUpdatesCannotBeWritten(t *testing.T) {
	ctx := context.Background()
	store, _ := openTestStore(t, ModeDisk, nil, 0)
	parent, err := store.Acquire(ctx, "parent", AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	coord := parent.(CoordinationSession)
	if err := coord.UpdateCoordination(ctx, setRecord("task", "a", `{"n":1}`)); err != nil {
		t.Fatal(err)
	}
	revision, err := coord.(CoordinationRevisionSession).CoordinationRevision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.withWrite(ctx, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `ALTER TABLE swarm_records RENAME TO swarm_records_gone`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	delta := coord.(CoordinationDeltaSession)
	if err := delta.UpdateCoordinationFrom(ctx, revision, changeRecord("task", "a", `{"n":2}`)); err != nil {
		t.Fatalf("an update answered from memory: %v", err)
	}
	if err := store.Sync(ctx); err == nil || !strings.Contains(err.Error(), "were not written") {
		t.Fatalf("Sync after a failed write = %v", err)
	}
	revision++
	if err := delta.UpdateCoordinationFrom(ctx, revision, changeRecord("task", "a", `{"n":3}`)); err == nil {
		t.Fatal("the store took another update after losing one")
	}
}
