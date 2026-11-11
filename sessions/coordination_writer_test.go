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
// else the store does in a transaction waits for the queue, so it sees the
// update's rows in the order the updates were accepted.
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

// An update answered from memory hands its caller the wait for it to be safe
// from a crash. On a disk store that is the write to the update log: once the
// wait returns the log holds the update, whether or not a checkpoint has put
// it in the table. A memory store has nothing to wait for.
func TestAcceptedUpdateHandsBackTheWaitForTheLog(t *testing.T) {
	ctx := context.Background()
	holdCheckpoints(t)
	store, path := openTestStore(t, ModeDisk, nil, 0)
	parent := acquireNamed(t, store, "parent")
	coord := parent.(CoordinationSession)
	if err := coord.UpdateCoordination(ctx, setRecord("task", "a", `{"n":1}`)); err != nil {
		t.Fatal(err)
	}
	var state *CoordinationState
	keeping := func(fn func(*CoordinationState) error) func(*CoordinationState) error {
		return func(s *CoordinationState) error {
			state = s
			return fn(s)
		}
	}
	updateFrom(t, coord, keeping(changeRecord("task", "a", `{"n":2}`, "one")))
	if state.Written == nil {
		t.Fatal("an accepted update handed back no wait")
	}
	if err := state.Written(ctx); err != nil {
		t.Fatal(err)
	}
	logged := readOwnLog(t, store, path)
	if len(logged) != 1 || string(logged[0].changes["task"]["a"]) != `{"n":2}` || len(logged[0].appended) != 1 {
		t.Fatalf("the log once the wait returned: %+v", logged)
	}
	if stored, appended := storedRecord(t, store, "task", "a"), storedMessages(t, store); stored != `{"n":1}` || appended != 0 {
		t.Fatalf("the table before a checkpoint = %q with %d messages", stored, appended)
	}
	if err := store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if stored, appended := storedRecord(t, store, "task", "a"), storedMessages(t, store); stored != `{"n":2}` || appended != 1 {
		t.Fatalf("the table after Sync = %q with %d messages", stored, appended)
	}
	// An update with nothing to keep is done when it returns.
	updateFrom(t, coord, keeping(func(*CoordinationState) error { return nil }))
	if state.Written != nil {
		t.Fatal("an update that kept nothing handed back a wait")
	}

	memory, _ := openTestStore(t, ModeMemory, nil, 0)
	parent = acquireNamed(t, memory, "parent")
	coord = parent.(CoordinationSession)
	if err := coord.UpdateCoordination(ctx, setRecord("task", "a", `{"n":1}`)); err != nil {
		t.Fatal(err)
	}
	updateFrom(t, coord, keeping(changeRecord("task", "a", `{"n":2}`, "one")))
	if state.Written != nil {
		t.Fatal("a memory store handed back a wait")
	}
	if got := historyText(t, parent); got != "one" {
		t.Fatalf("memory store transcript = %q", got)
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

// When a checkpoint cannot write accepted updates the store stops taking
// more: the callers were told they were accepted, and the database does not
// have them. A caller that waits for the database hears why.
func TestCoordinationStopsWhenQueuedUpdatesCannotBeWritten(t *testing.T) {
	ctx := context.Background()
	store, path := openTestStore(t, ModeDisk, nil, 0)
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
	var state *CoordinationState
	if err := delta.UpdateCoordinationFrom(ctx, revision, func(s *CoordinationState) error {
		state = s
		return changeRecord("task", "a", `{"n":2}`)(s)
	}); err != nil {
		t.Fatalf("an update answered from memory: %v", err)
	}
	// The log has the update, so a crash would not lose it.
	if err := state.Written(ctx); err != nil {
		t.Fatalf("the wait for the log = %v", err)
	}
	if err := store.Sync(ctx); err == nil || !strings.Contains(err.Error(), "were not written") {
		t.Fatalf("Sync after a failed checkpoint = %v", err)
	}
	revision++
	if err := delta.UpdateCoordinationFrom(ctx, revision, changeRecord("task", "a", `{"n":3}`)); err == nil {
		t.Fatal("the store took another update after a checkpoint failed")
	}
	// The store keeps its log when it closes, and the next one to open the
	// database writes what the log holds.
	if err := store.withTransactionNow(ctx, "BEGIN IMMEDIATE", func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `ALTER TABLE swarm_records_gone RENAME TO swarm_records`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	reopened, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if stored := storedRecord(t, reopened, "task", "a"); stored != `{"n":2}` {
		t.Fatalf("the record after the next store recovered the log = %q", stored)
	}
}

// A volatile change says what its caller is doing at the moment. The store
// keeps it in memory, where its own readers see it, and writes it neither to
// the log nor to the table: the record is written with its next change that
// is not volatile. A commit through another connection does not make the
// store forget it.
func TestVolatileChangeStaysInMemory(t *testing.T) {
	ctx := context.Background()
	holdCheckpoints(t)
	path := filepath.Join(t.TempDir(), "polly.db")
	open := func() *SQLiteStore {
		store, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		return store
	}
	here, there := open(), open()
	parent := acquireNamed(t, here, "parent")
	coord := parent.(CoordinationSession)
	if err := coord.UpdateCoordination(ctx, setRecord("task", "a", `{"n":1}`)); err != nil {
		t.Fatal(err)
	}
	var state *CoordinationState
	volatile := func(kind, id, value string, kept ...string) func(*CoordinationState) error {
		return func(s *CoordinationState) error {
			state = s
			s.Changes = map[string]map[string]json.RawMessage{kind: {id: json.RawMessage(value)}}
			s.Volatile = map[string]map[string]bool{kind: {id: true}}
			for _, other := range kept {
				s.Changes[kind][other] = json.RawMessage(value)
			}
			return nil
		}
	}
	updateFrom(t, coord, volatile("task", "a", `{"n":2}`))
	if state.Written != nil {
		t.Fatal("a change that is only volatile handed back a wait")
	}
	if got := readRecord(t, coord, "task", "a"); got != `{"n":2}` {
		t.Fatalf("the store's own read after a volatile change = %q", got)
	}
	if logged := readOwnLog(t, here, path); len(logged) != 0 {
		t.Fatalf("a volatile change was logged: %+v", logged)
	}
	if err := here.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := storedRecord(t, here, "task", "a"); got != `{"n":1}` {
		t.Fatalf("a volatile change reached the table: %q", got)
	}
	// Another connection commits, and this store reads through the database.
	child, err := there.Acquire(ctx, "child", AcquireOptions{Parent: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	if got := readRecord(t, child.(CoordinationSession), "task", "a"); got != `{"n":1}` {
		t.Fatalf("another handle read a volatile change: %q", got)
	}
	if err := coord.UpdateCoordination(ctx, func(*CoordinationState) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if got := readRecord(t, coord, "task", "a"); got != `{"n":2}` {
		t.Fatalf("the volatile change after another connection's commit = %q", got)
	}
	// In one update, what is not volatile is kept and what is volatile is not.
	updateFrom(t, coord, volatile("task", "a", `{"n":3}`, "b"))
	if err := state.Written(ctx); err != nil {
		t.Fatal(err)
	}
	logged := readOwnLog(t, here, path)
	if len(logged) != 1 || len(logged[0].changes["task"]) != 1 || string(logged[0].changes["task"]["b"]) != `{"n":3}` {
		t.Fatalf("the log after an update that is partly volatile: %+v", logged)
	}
	// The record's next change that is not volatile writes it.
	updateFrom(t, coord, changeRecord("task", "a", `{"n":4}`))
	if err := here.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if a, b := storedRecord(t, here, "task", "a"), storedRecord(t, here, "task", "b"); a != `{"n":4}` || b != `{"n":3}` {
		t.Fatalf("the table after a change that is not volatile: a=%q b=%q", a, b)
	}
}
