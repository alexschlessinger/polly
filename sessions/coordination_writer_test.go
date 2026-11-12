package sessions

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// eventually polls until ok or two seconds have passed.
func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); !ok(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
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

// A checkpoint with more rows than one transaction takes is written in
// several, and whole.
func TestCheckpointLargerThanATransactionIsWrittenWhole(t *testing.T) {
	holdCheckpoints(t)
	old := checkpointRows
	checkpointRows = 3
	t.Cleanup(func() { checkpointRows = old })
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "polly.db")
	store, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := store.Acquire(ctx, "parent", AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	coord := parent.(CoordinationSession)
	// The first update goes to the database and leaves the family in memory.
	if err := coord.UpdateCoordination(ctx, setRecord("task", "first", `{"n":0}`)); err != nil {
		t.Fatal(err)
	}
	const updates = 10
	var transcript []string
	for n := range updates {
		text := fmt.Sprintf("message %d", n)
		transcript = append(transcript, text)
		updateFrom(t, coord, changeRecord("task", fmt.Sprintf("task-%d", n), fmt.Sprintf(`{"n":%d}`, n), text))
	}
	watcher, err := store.readers.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()
	stored := func() (records int) {
		t.Helper()
		if err := watcher.QueryRowContext(ctx, "SELECT count(*) FROM swarm_records WHERE kind = 'task'").Scan(&records); err != nil {
			t.Fatal(err)
		}
		return records
	}
	// The test takes every other turn at the writing connection, so it looks
	// at the database between the checkpoint's transactions.
	if err := store.enter(ctx); err != nil {
		t.Fatal(err)
	}
	synced := make(chan error, 1)
	go func() { synced <- store.Sync(ctx) }()
	var seen []int
	for deadline := time.Now().Add(10 * time.Second); ; {
		records := stored()
		if len(seen) == 0 || seen[len(seen)-1] != records {
			seen = append(seen, records)
		}
		if records == updates+1 || time.Now().After(deadline) {
			break
		}
		// Long enough for the checkpoint to come and wait for its turn.
		time.Sleep(200 * time.Microsecond)
		store.leave()
		if err := store.enter(ctx); err != nil {
			t.Fatal(err)
		}
	}
	store.leave()
	if err := <-synced; err != nil {
		t.Fatal(err)
	}
	if seen[0] != 1 || seen[len(seen)-1] != updates+1 {
		t.Fatalf("the database held %v records, want 1 before the checkpoint and %d after", seen, updates+1)
	}
	if len(seen) < 3 {
		t.Fatalf("the database held %v records: the checkpoint was one transaction", seen)
	}
	for _, records := range seen[1 : len(seen)-1] {
		if (records-1)%checkpointRows != 0 {
			t.Fatalf("the database held %v records: a transaction wrote other than %d", seen, checkpointRows)
		}
	}
	if err := watcher.Close(); err != nil {
		t.Fatal(err)
	}
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
	if parent, err = reopened.Acquire(ctx, "parent", AcquireOptions{}); err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	for n := range updates {
		if got, want := readRecord(t, parent.(CoordinationSession), "task", fmt.Sprintf("task-%d", n)), fmt.Sprintf(`{"n":%d}`, n); got != want {
			t.Fatalf("task-%d after reopening = %q, want %q", n, got, want)
		}
	}
	if got, want := historyText(t, parent), strings.Join(transcript, ","); got != want {
		t.Fatalf("transcript after reopening = %q, want %q", got, want)
	}
}

// A checkpoint leaves a session's row as the updates it writes said: the
// transcript's next sequence, that the session had a turn, and the identity
// the transcript is bound to, whichever of them an update brought.
func TestCheckpointWritesASessionsTurnAndIdentity(t *testing.T) {
	holdCheckpoints(t)
	ctx := context.Background()
	store, _ := openTestStore(t, ModeDisk, nil, 0)
	parent, err := store.Acquire(ctx, "parent", AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	coord := parent.(CoordinationSession)
	// The first update goes to the database and leaves the family in memory.
	if err := coord.UpdateCoordination(ctx, setRecord("task", "a", `{"n":0}`)); err != nil {
		t.Fatal(err)
	}
	type row struct {
		next, updated int64
		turn          bool
		execution     sql.NullString
		generation    sql.NullInt64
		sequence      sql.NullInt64
	}
	read := func() (r row) {
		t.Helper()
		err := store.readers.QueryRowContext(ctx, `SELECT next_sequence, has_turn, updated_ns,
			json_extract(CAST(settings_json AS TEXT), '$.transcript.execution'),
			json_extract(CAST(settings_json AS TEXT), '$.transcript.generation'),
			json_extract(CAST(settings_json AS TEXT), '$.transcript.sequence')
			FROM sessions WHERE name = 'parent'`).Scan(&r.next, &r.turn, &r.updated, &r.execution, &r.generation, &r.sequence)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	before := read()
	if before.next != 0 || before.turn || before.execution.Valid {
		t.Fatalf("the session's row before any turn: %+v", before)
	}
	for _, step := range []struct {
		name       string
		appended   []string
		identity   *TranscriptIdentity
		next       int64
		generation int64
		sequence   int64
	}{
		{name: "a turn and its identity", appended: []string{"one", "two"}, identity: &TranscriptIdentity{Execution: "run", Generation: 1}, next: 2, generation: 1, sequence: 2},
		{name: "a turn alone", appended: []string{"three"}, next: 3, generation: 1, sequence: 2},
		{name: "an identity alone", identity: &TranscriptIdentity{Execution: "run", Generation: 2}, next: 3, generation: 2, sequence: 3},
	} {
		queued := accepted(store)
		updateFrom(t, coord, func(state *CoordinationState) error {
			state.Transcript = step.identity
			return changeRecord("task", "a", fmt.Sprintf(`{"n":%d}`, step.next+step.generation), step.appended...)(state)
		})
		if accepted(store) != queued+1 {
			t.Fatalf("%s: the update was not queued for the writer", step.name)
		}
		if err := store.Sync(ctx); err != nil {
			t.Fatal(err)
		}
		after := read()
		if after.next != step.next || !after.turn || after.execution.String != "run" || after.generation.Int64 != step.generation || after.sequence.Int64 != step.sequence {
			t.Fatalf("%s: the session's row is %+v, want next %d, generation %d bound at %d", step.name, after, step.next, step.generation, step.sequence)
		}
		if after.updated <= before.updated {
			t.Fatalf("%s: the session's row was not touched: %d after %d", step.name, after.updated, before.updated)
		}
		before = after
	}
	if got := historyText(t, parent); got != "one,two,three" {
		t.Fatalf("transcript = %q", got)
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

// A session that writes its own row first writes what is pending for it. A
// checkpoint under way may hold that, among thousands of other sessions'
// messages. The session takes its own out of the checkpoint and does not
// wait for the rest.
func TestSessionTakesItsPendingOutOfACheckpointUnderWay(t *testing.T) {
	ctx := context.Background()
	holdCheckpoints(t)
	store, _ := openTestStore(t, ModeDisk, nil, 0)
	parent := acquireNamed(t, store, "parent")
	if err := parent.(CoordinationSession).UpdateCoordination(ctx, setRecord("task", "a", `{"n":1}`)); err != nil {
		t.Fatal(err)
	}
	child, err := store.Acquire(ctx, "child", AcquireOptions{Parent: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	updateFrom(t, child.(CoordinationSession), changeRecord("task", "a", `{"n":2}`, "one"))

	// The checkpoint starts and then waits for the store's only connection.
	conn, err := store.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	synced := make(chan error, 1)
	go func() { synced <- store.Sync(ctx) }()
	w := store.writer
	eventually(t, "the checkpoint never started", func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.applying != nil
	})
	prompt, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	id := child.(*sqliteSession).id
	pending, err := w.takeSession(prompt, id)
	if err != nil || pending == nil || len(pending.tail) != 1 {
		t.Fatalf("taking the session's pending out of the checkpoint = %+v, %v", pending, err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-synced; err != nil {
		t.Fatal(err)
	}
	if got := storedRecord(t, store, "task", "a"); got != `{"n":2}` {
		t.Fatalf("the checkpoint wrote the record %q", got)
	}
	if got := storedMessages(t, store); got != 0 {
		t.Fatalf("the checkpoint wrote %d messages the session had taken", got)
	}
	// Given back, they are written ahead of the session's next write.
	w.restoreSession(pending)
	if err := child.AddMessage(ctx, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "two"}); err != nil {
		t.Fatal(err)
	}
	if got := storedMessages(t, store); got != 2 {
		t.Fatalf("%d messages in the table after the session wrote again", got)
	}
	if got := historyText(t, child); got != "one,two" {
		t.Fatalf("transcript after the session wrote again = %q", got)
	}
}

// A session that finds a checkpoint's transaction writing its messages waits
// for that transaction. What it then takes is what was accepted since: the
// messages the transaction wrote are in the database, and no longer pending.
// Given back, what it took goes ahead of what was accepted meanwhile.
func TestSessionWaitingOnACheckpointTakesOnlyWhatCameAfter(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	w := store.writer
	id, token := []byte("0123456789abcdef"), []byte("fedcba9876543210")
	payload := func(text string) []byte { return []byte(`{"role":"user","content":"` + text + `"}`) }
	written := &pendingSession{id: id, token: token, start: 0, tail: [][]byte{payload("one"), payload("two")}, writing: true}
	later := &pendingSession{id: id, token: token, start: 2, tail: [][]byte{payload("three")}}
	w.mu.Lock()
	w.applying = newPendingWindow()
	w.applying.sessions[string(id)] = written
	w.window.sessions[string(id)] = later
	w.mu.Unlock()
	t.Cleanup(func() {
		w.mu.Lock()
		w.applying, w.window = nil, newPendingWindow()
		w.mu.Unlock()
	})

	taken := make(chan *pendingSession, 1)
	go func() {
		pending, err := w.takeSession(context.Background(), id)
		if err != nil {
			t.Error(err)
		}
		taken <- pending
	}()
	select {
	case pending := <-taken:
		t.Fatalf("the session took %+v from under the transaction writing it", pending)
	case <-time.After(100 * time.Millisecond):
	}
	// The transaction commits, and with it the checkpoint ends.
	w.mu.Lock()
	written.writing = false
	delete(w.applying.sessions, string(id))
	w.applying = nil
	announce(&w.appliedMoved)
	w.mu.Unlock()
	var pending *pendingSession
	select {
	case pending = <-taken:
	case <-time.After(2 * time.Second):
		t.Fatal("the session never took its pending messages")
	}
	if pending == nil || pending.start != 2 || len(pending.tail) != 1 {
		t.Fatalf("the session took %+v, want the one message accepted after the checkpoint's", pending)
	}

	newest := &pendingSession{id: id, token: token, start: 3, tail: [][]byte{payload("four")}}
	w.mu.Lock()
	w.window.sessions[string(id)] = newest
	w.mu.Unlock()
	w.restoreSession(pending)
	w.mu.Lock()
	restored := w.window.sessions[string(id)]
	w.mu.Unlock()
	if restored == nil || restored.start != 2 || len(restored.tail) != 2 {
		t.Fatalf("given back, the pending messages are %+v, want both in order", restored)
	}
}
