package sessions

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
)

// holdCheckpoints keeps checkpoints from running on their own for the rest of
// the test, so what is pending stays pending until something settles.
func holdCheckpoints(t *testing.T) {
	t.Helper()
	old := checkpointDelay
	checkpointDelay = time.Hour
	t.Cleanup(func() { checkpointDelay = old })
}

// crashStore drops the store as a killed process would: nothing pending
// reaches the database, the log's files stay, and the lock on them goes. The
// leases its sessions held are left to run out.
func crashStore(t *testing.T, store *SQLiteStore) {
	t.Helper()
	w := store.writer
	if err := w.wait(context.Background(), accepted(store)); err != nil {
		t.Fatal(err)
	}
	w.mu.Lock()
	w.applyErr = errors.New("crashed")
	w.closed = true
	w.stopped.Store(true)
	started, log := w.started, w.log
	w.log = nil
	w.mu.Unlock()
	if started {
		wake(w.wakeLog)
		wake(w.wakeApply)
		<-w.logDone
		<-w.applyDone
	}
	store.closed.Store(true)
	store.cancel(ErrStoreClosed)
	store.wg.Wait()
	if log != nil {
		if err := log.close(false); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.db.Close(); err != nil {
		t.Fatal(err)
	}
}

// readOwnLog reads what the store has written to its part of the log.
func readOwnLog(t *testing.T, store *SQLiteStore, path string) []loggedUpdate {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(logDir(path), store.writer.owner()+"-*.log"))
	if err != nil {
		t.Fatal(err)
	}
	var updates []loggedUpdate
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		_, read, err := readLogFile(data)
		if err != nil {
			t.Fatal(err)
		}
		updates = append(updates, read...)
	}
	return updates
}

// storedRecord reads a record from the table, without settling first.
func storedRecord(t *testing.T, store *SQLiteStore, kind, id string) string {
	t.Helper()
	var stored []byte
	ctx := context.Background()
	err := store.withTransactionNow(ctx, "BEGIN", func(conn *sql.Conn) error {
		err := conn.QueryRowContext(ctx, `SELECT payload_json FROM swarm_records WHERE kind=? AND id=?`, kind, id).Scan(&stored)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(stored)
}

// storedMessages counts the messages in the table, without settling first.
func storedMessages(t *testing.T, store *SQLiteStore) int {
	t.Helper()
	var count int
	ctx := context.Background()
	if err := store.withTransactionNow(ctx, "BEGIN", func(conn *sql.Conn) error {
		return conn.QueryRowContext(ctx, `SELECT count(*) FROM messages`).Scan(&count)
	}); err != nil {
		t.Fatal(err)
	}
	return count
}

func logFiles(t *testing.T, path string) []string {
	t.Helper()
	entries, err := os.ReadDir(logDir(path))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func testLogUpdate(n byte) queuedUpdate {
	id := func(b byte) []byte { return append(make([]byte, 15), b) }
	update := queuedUpdate{
		parent: id(1), parentToken: id(2), session: id(3), sessionToken: id(4),
		sequence: int64(n), nowNS: 1700000000000000000 + int64(n),
		appended:   [][]byte{[]byte(`{"role":"user","content":"a"}`), []byte(`{}`)},
		transcript: &TranscriptIdentity{Execution: "e", Generation: 2, Sequence: int64(n) + 2},
		changes: map[string]map[string]json.RawMessage{
			"task": {"kept": json.RawMessage(`{"n":1}`), "gone": nil},
			"mail": {"m": json.RawMessage(`[]`)},
		},
	}
	// Every other update carries an outcome, and every third no transcript.
	if n%2 == 0 {
		update.outcome = ReportPaused
	}
	if n%3 == 0 {
		update.transcript = nil
	}
	return update
}

// A log file gives back the updates appended to it, with the place each was
// accepted at, and a file cut short or damaged gives back the updates of the
// frames before the damage.
func TestUpdateLogReadsWhatWasAppendedUpToDamage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "polly.db")
	log, err := openUpdateLog(path)
	if err != nil {
		t.Fatal(err)
	}
	var want []loggedUpdate
	var ends []int64
	place := uint64(1)
	for _, batch := range []int{1, 3, 2} {
		var encoded []byte
		first := place
		for range batch {
			update := testLogUpdate(byte(place))
			if encoded, err = appendLogUpdate(encoded, &update); err != nil {
				t.Fatal(err)
			}
			want = append(want, loggedUpdate{place, update})
			place++
		}
		if err := log.append(first, place-1, encoded); err != nil {
			t.Fatal(err)
		}
		ends = append(ends, log.size)
	}
	file := log.files[0].path
	if err := log.close(false); err != nil {
		t.Fatal(err)
	}
	whole, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	flipped := append([]byte(nil), whole...)
	flipped[ends[0]+20] ^= 1
	for name, tc := range map[string]struct {
		data []byte
		want int
	}{
		"whole":               {whole, 6},
		"cut inside a frame":  {whole[:ends[1]+9], 4},
		"cut inside a header": {whole[:ends[0]+3], 1},
		"zeros after":         {append(append([]byte(nil), whole...), make([]byte, 4096)...), 6},
		"garbage after":       {append(append([]byte(nil), whole...), []byte("not a frame at all")...), 6},
		"a bit flipped":       {flipped, 1},
		"header only":         {whole[:logHeaderBytes], 0},
	} {
		owner, got, err := readLogFile(tc.data)
		if err != nil || !reflect.DeepEqual(owner, log.owner) {
			t.Fatalf("%s: owner %x, %v", name, owner, err)
		}
		if len(got) != tc.want || tc.want > 0 && !reflect.DeepEqual(got, want[:tc.want]) {
			t.Fatalf("%s: read %d updates, want the first %d:\n%+v", name, len(got), tc.want, got)
		}
	}
	if _, _, err := readLogFile(whole[:10]); err == nil {
		t.Fatal("a file without a header was read as a log")
	}
}

// A store that dies leaves in its log the updates no checkpoint wrote. The
// next store to open the database writes them and removes the log.
func TestNextStoreRecoversTheLogOfOneThatDied(t *testing.T) {
	ctx := context.Background()
	holdCheckpoints(t)
	store, path := openTestStore(t, ModeDisk, nil, 0)
	parent := acquireNamed(t, store, "parent")
	coord := parent.(CoordinationSession)
	if err := coord.UpdateCoordination(ctx, setRecord("task", "a", `{"n":1}`)); err != nil {
		t.Fatal(err)
	}
	updateFrom(t, coord, changeRecord("task", "a", `{"n":2}`, "one"))
	updateFrom(t, coord, func(state *CoordinationState) error {
		state.Changes = map[string]map[string]json.RawMessage{"task": {"a": nil, "b": json.RawMessage(`{"n":3}`)}}
		state.Transcript = &TranscriptIdentity{Execution: "run", Generation: 1}
		return nil
	})
	updateFrom(t, coord, changeRecord("task", "c", `{"n":4}`, "two", "three"))
	if got := storedMessages(t, store); got != 0 {
		t.Fatalf("%d messages reached the table before a checkpoint", got)
	}
	crashStore(t, store)
	if files := logFiles(t, path); len(files) != 2 {
		t.Fatalf("a store that died left %v", files)
	}

	next, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	for id, want := range map[string]string{"a": "", "b": `{"n":3}`, "c": `{"n":4}`} {
		if got := storedRecord(t, next, "task", id); got != want {
			t.Fatalf("task %s after recovery = %q, want %q", id, got, want)
		}
	}
	view, err := next.ReadView(ctx, ViewTarget{Name: "parent"}, "")
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, message := range view.History {
		texts = append(texts, message.Content)
	}
	if got := strings.Join(texts, ","); got != "one,two,three" || view.Sequence != 3 {
		t.Fatalf("transcript after recovery = %q, next %d", got, view.Sequence)
	}
	if id := view.Metadata.Transcript; id == nil || id.Execution != "run" || id.Sequence != 1 {
		t.Fatalf("transcript identity after recovery = %+v", id)
	}
	// Only the new store's own lock is left.
	if files := logFiles(t, path); len(files) != 1 || files[0] != next.writer.owner()+".lock" {
		t.Fatalf("the log after recovery holds %v", files)
	}
}

// A lease taken since an update was accepted means someone has gone on from
// what the database held, so recovery leaves that family's records and that
// session's transcript as they are.
func TestRecoveryLeavesWhatWasLeasedAgain(t *testing.T) {
	ctx := context.Background()
	holdCheckpoints(t)
	store, path := openTestStore(t, ModeDisk, nil, 0)
	parent := acquireNamed(t, store, "parent")
	coord := parent.(CoordinationSession)
	if err := coord.UpdateCoordination(ctx, setRecord("task", "a", `{"n":1}`)); err != nil {
		t.Fatal(err)
	}
	child, err := store.Acquire(ctx, "child", AcquireOptions{Parent: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	updateFrom(t, child.(CoordinationSession), changeRecord("task", "a", `{"n":2}`, "from the child"))
	updateFrom(t, coord, changeRecord("task", "b", `{"n":3}`, "from the parent"))
	crashStore(t, store)
	// A store that knows nothing of the log takes the parent over and goes on.
	db, err := sql.Open("sqlite", sqliteDataSource(ModeDisk, path))
	if err != nil {
		t.Fatal(err)
	}
	other := make([]byte, 16)
	if _, err := db.ExecContext(ctx, `UPDATE session_leases SET owner_token = ? WHERE session_id = ?`, other, parent.(*sqliteSession).id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE swarm_records SET payload_json = ? WHERE kind = 'task' AND id = 'a'`, []byte(`{"n":9}`)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	next, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if a, b := storedRecord(t, next, "task", "a"), storedRecord(t, next, "task", "b"); a != `{"n":9}` || b != "" {
		t.Fatalf("records of a family leased again after recovery: a=%q b=%q", a, b)
	}
	// The child's lease is still the one its update was accepted under.
	for name, want := range map[string]int{"parent": 0, "child": 1} {
		view, err := next.ReadView(ctx, ViewTarget{Name: name}, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(view.History) != want {
			t.Fatalf("%s holds %d messages after recovery, want %d", name, len(view.History), want)
		}
	}
	if files := logFiles(t, path); len(files) != 1 {
		t.Fatalf("the log after recovery holds %v", files)
	}
}

// A store that is already open when another dies recovers the log before it
// takes over a lease the dead store let run out.
func TestTakingOverALeaseRecoversTheLogFirst(t *testing.T) {
	ctx := context.Background()
	holdCheckpoints(t)
	oldStale, oldRetry := leaseStaleAfter, leaseRetryInterval
	leaseStaleAfter, leaseRetryInterval = 400*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { leaseStaleAfter, leaseRetryInterval = oldStale, oldRetry })
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
	updateFrom(t, coord, changeRecord("task", "a", `{"n":2}`, "one", "two"))
	crashStore(t, here)

	taken, err := there.Acquire(ctx, "parent", AcquireOptions{ExistingOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	if got := historyText(t, taken); got != "one,two" {
		t.Fatalf("transcript after taking the lease over = %q", got)
	}
	if got := readRecord(t, taken.(CoordinationSession), "task", "a"); got != `{"n":2}` {
		t.Fatalf("record after taking the lease over = %q", got)
	}
}

// A session reads its transcript over what is pending for it. A member that
// closes leaves its pending messages with its lease, which the store keeps.
// Giving the lease up writes them, without a checkpoint of everything else.
func TestSessionReadsAndClosesOverItsPendingMessages(t *testing.T) {
	ctx := context.Background()
	holdCheckpoints(t)
	store, _ := openTestStore(t, ModeDisk, nil, 0)
	parent := acquireNamed(t, store, "parent")
	coord := parent.(CoordinationSession)
	if err := coord.UpdateCoordination(ctx, setRecord("task", "a", `{"n":1}`)); err != nil {
		t.Fatal(err)
	}
	child, err := store.Acquire(ctx, "child", AcquireOptions{Parent: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	if err := child.AddMessage(ctx, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "stored"}); err != nil {
		t.Fatal(err)
	}
	member := child.(CoordinationSession)
	updateFrom(t, member, changeRecord("task", "a", `{"n":2}`, "pending one"))
	updateFrom(t, member, changeRecord("task", "a", `{"n":3}`, "pending two"))
	if got := storedMessages(t, store); got != 1 {
		t.Fatalf("%d messages in the table before a checkpoint", got)
	}
	if got := historyText(t, child); got != "stored,pending one,pending two" {
		t.Fatalf("transcript read over pending messages = %q", got)
	}
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	if got := storedMessages(t, store); got != 1 {
		t.Fatalf("%d messages in the table after the member closed", got)
	}
	// Asked for by name alone, the session is taken in the database, so the
	// store gives up the lease it kept.
	again, err := store.Acquire(ctx, "child", AcquireOptions{ExistingOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := storedMessages(t, store); got != 3 {
		t.Fatalf("%d messages in the table after the kept lease was given up", got)
	}
	// The family's records wait for the checkpoint, or for the root to close.
	if got := storedRecord(t, store, "task", "a"); got != `{"n":1}` {
		t.Fatalf("giving up a member's lease wrote the family's record: %q", got)
	}
	if got := historyText(t, again); got != "stored,pending one,pending two" {
		t.Fatalf("transcript after acquiring again = %q", got)
	}
	updateFrom(t, again.(CoordinationSession), changeRecord("task", "a", `{"n":4}`, "pending three"))
	if err := again.Close(); err != nil {
		t.Fatal(err)
	}
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}
	if got := storedRecord(t, store, "task", "a"); got != `{"n":4}` {
		t.Fatalf("the record after the root closed = %q", got)
	}
	if got := storedMessages(t, store); got != 4 {
		t.Fatalf("%d messages in the table after the root closed", got)
	}
}

// The log keeps an update until the file holding it is full, long after a
// checkpoint wrote it. The store marks how far its checkpoints got, so that
// recovery does not write such an update over what came after it.
func TestRecoveryLeavesUpdatesTheCheckpointWrote(t *testing.T) {
	ctx := context.Background()
	holdCheckpoints(t)
	store, path := openTestStore(t, ModeDisk, nil, 0)
	parent := acquireNamed(t, store, "parent")
	coord := parent.(CoordinationSession)
	if err := coord.UpdateCoordination(ctx, setRecord("task", "a", `{"n":1}`)); err != nil {
		t.Fatal(err)
	}
	updateFrom(t, coord, changeRecord("task", "a", `{"n":2}`, "old one", "old two"))
	// The log holds the update before a checkpoint writes it.
	if err := store.writer.wait(ctx, accepted(store)); err != nil {
		t.Fatal(err)
	}
	if err := store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	// The transcript starts over and the record moves on, both in the
	// database, with the first update still in the log.
	if err := parent.Clear(ctx); err != nil {
		t.Fatal(err)
	}
	if err := coord.UpdateCoordination(ctx, setRecord("task", "a", `{"n":3}`)); err != nil {
		t.Fatal(err)
	}
	if logged := readOwnLog(t, store, path); len(logged) != 1 {
		t.Fatalf("the log holds %d updates", len(logged))
	}
	crashStore(t, store)

	next, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if record, appended := storedRecord(t, next, "task", "a"), storedMessages(t, next); record != `{"n":3}` || appended != 0 {
		t.Fatalf("after recovery the record is %q and the transcript holds %d messages", record, appended)
	}
}
