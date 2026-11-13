package sessions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
)

// pendingChild acquires the member "child" of the driven family and has it
// change a record and append the message "one". The update is in the log and
// not in the database.
func pendingChild(t *testing.T, store *SQLiteStore) Session {
	t.Helper()
	child, err := store.Acquire(context.Background(), "child", AcquireOptions{Parent: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	updateFrom(t, child.(CoordinationSession), changeRecord("task", "a", `{"n":2}`, "one"))
	if err := store.writer.wait(context.Background(), accepted(store)); err != nil {
		t.Fatal(err)
	}
	return child
}

// startCheckpoint starts a checkpoint of everything pending, which then waits
// for the store's only connection. free lets it have the connection, and
// done waits for it to end.
func startCheckpoint(t *testing.T, store *SQLiteStore) (free, done func()) {
	t.Helper()
	conn, err := store.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// A test that fails before free must not leave the store without it.
	t.Cleanup(func() { _ = conn.Close() })
	synced := make(chan error, 1)
	go func() { synced <- store.Sync(context.Background()) }()
	w := store.writer
	eventually(t, "the checkpoint never started", func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.applying != nil
	})
	free = func() {
		t.Helper()
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
	}
	done = func() {
		t.Helper()
		if err := <-synced; err != nil {
			t.Fatal(err)
		}
	}
	return free, done
}

// takeWindow makes what is pending the window of a checkpoint that has yet
// to write any of it, as the writer does when it starts one. finish runs
// that checkpoint.
func takeWindow(t *testing.T, store *SQLiteStore) (finish func()) {
	t.Helper()
	w := store.writer
	w.mu.Lock()
	window, log := w.window, w.log
	w.applying, w.window = window, newPendingWindow()
	w.mu.Unlock()
	return func() {
		t.Helper()
		if err := w.apply(window, log); err != nil {
			t.Fatal(err)
		}
		w.mu.Lock()
		w.applied, w.applying = max(w.applied, window.last), nil
		announce(&w.appliedMoved)
		w.mu.Unlock()
	}
}

// afterCrash drops store as a killed process would and opens the database
// again, which recovers the log the store left.
func afterCrash(t *testing.T, store *SQLiteStore, path string) *SQLiteStore {
	t.Helper()
	crashStore(t, store)
	next, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { next.Close() })
	return next
}

func userMessage(text string) messages.ChatMessage {
	return messages.ChatMessage{Role: messages.MessageRoleUser, Content: text}
}

// pendingIn reports how many messages each of the writer's windows holds for
// the session id, or -1 for a window that does not hold the session.
func pendingIn(store *SQLiteStore, id []byte) (applying, window int) {
	w := store.writer
	w.mu.Lock()
	defer w.mu.Unlock()
	count := func(window *pendingWindow) int {
		if window == nil || window.sessions[string(id)] == nil {
			return -1
		}
		return len(window.sessions[string(id)].tail)
	}
	return count(w.applying), count(w.window)
}

// A session's write that gives up while a checkpoint is under way takes
// nothing with it: the checkpoint writes the session's pending messages with
// the records of the same updates, so a crash right after it keeps both.
func TestWriteGivenUpBehindACheckpointLosesNothingInACrash(t *testing.T) {
	store, path, _ := drivenFamily(t)
	child := pendingChild(t, store)
	free, done := startCheckpoint(t, store)
	brief, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := child.AddMessage(brief, userMessage("two")); err == nil {
		t.Fatal("the session wrote without the store's connection")
	}
	free()
	done()

	next := afterCrash(t, store, path)
	if record, stored := storedRecord(t, next, "task", "a"), storedMessages(t, next); record != `{"n":2}` || stored != 1 {
		t.Fatalf("after the crash the record is %q and the transcript holds %d messages, want the update whole", record, stored)
	}
}

// The same holds for a lease given up while a checkpoint is under way: a
// release that fails leaves the session's pending messages to the checkpoint.
func TestReleaseGivenUpBehindACheckpointLosesNothingInACrash(t *testing.T) {
	store, path, _ := drivenFamily(t)
	child := pendingChild(t, store)
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	kept := len(store.kept)
	store.mu.Unlock()
	if kept != 1 {
		t.Fatalf("the store kept %d leases after the member closed, want its one", kept)
	}
	free, done := startCheckpoint(t, store)
	brief, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := store.releaseKept(brief, nil); err == nil {
		t.Fatal("the lease was released without the store's connection")
	}
	free()
	done()

	next := afterCrash(t, store, path)
	if record, stored := storedRecord(t, next, "task", "a"), storedMessages(t, next); record != `{"n":2}` || stored != 1 {
		t.Fatalf("after the crash the record is %q and the transcript holds %d messages, want the update whole", record, stored)
	}
}

// A write that fails in its transaction, after it wrote what was pending for
// the session, leaves that pending: the transaction's rows are gone with it.
func TestFailedWriteLeavesWhatWasPending(t *testing.T) {
	ctx := context.Background()
	store, _, _ := drivenFamily(t)
	child := pendingChild(t, store)
	defer child.Close()
	handle := child.(*sqliteSession)
	failed := errors.New("the write failed")
	err := handle.leased(ctx, true, func(ctx context.Context, conn *sql.Conn) error {
		var stored int
		if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM messages`).Scan(&stored); err != nil {
			return err
		}
		if stored != 1 {
			t.Errorf("the write found %d messages stored, want the pending one written ahead of it", stored)
		}
		return failed
	})
	if !errors.Is(err, failed) {
		t.Fatalf("the write returned %v", err)
	}
	if _, window := pendingIn(store, handle.id); window != 1 {
		t.Fatalf("%d messages are pending for the session after its write failed, want 1", window)
	}
	if stored := storedMessages(t, store); stored != 0 {
		t.Fatalf("%d messages stored after the write failed", stored)
	}
	if err := store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if stored := storedMessages(t, store); stored != 1 {
		t.Fatalf("%d messages stored after a checkpoint, want 1", stored)
	}
	if got := historyText(t, child); got != "one" {
		t.Fatalf("transcript = %q", got)
	}
}

// What follows a transaction's commit runs before the transaction's turn at
// the connection ends, and not at all for a transaction that failed.
func TestTransactionIsFollowedInItsTurn(t *testing.T) {
	ctx := context.Background()
	store, _ := openTestStore(t, ModeDisk, nil, 0)
	followed := false
	err := store.withTransactionThen(ctx, "BEGIN IMMEDIATE", func(*sql.Conn) error { return nil }, func() {
		followed = true
		if len(store.turn) != 1 {
			t.Error("the turn had ended")
		}
	})
	if err != nil || !followed {
		t.Fatalf("followed = %v, err = %v", followed, err)
	}
	failed := errors.New("the transaction failed")
	err = store.withTransactionThen(ctx, "BEGIN IMMEDIATE", func(*sql.Conn) error { return failed }, func() {
		t.Error("a transaction that failed was followed")
	})
	if !errors.Is(err, failed) {
		t.Fatalf("the transaction returned %v", err)
	}
}

// A session that writes while a checkpoint of many transactions is under way
// waits for its turn at the connection, not for the checkpoint: it writes
// what the checkpoint has not come to itself.
func TestSessionWriteDoesNotWaitForTheCheckpoint(t *testing.T) {
	ctx := context.Background()
	store, _, _ := drivenFamily(t)
	old := checkpointRows
	checkpointRows = 1
	t.Cleanup(func() { checkpointRows = old })
	const members = 200
	children := make([]Session, members)
	for n := range children {
		child, err := store.Acquire(ctx, fmt.Sprintf("child-%d", n), AcquireOptions{Parent: "parent"})
		if err != nil {
			t.Fatal(err)
		}
		defer child.Close()
		updateFrom(t, child.(CoordinationSession), changeRecord("task", "a", fmt.Sprintf(`{"n":%d}`, n+2), "one"))
		children[n] = child
	}
	free, done := startCheckpoint(t, store)
	writer := children[members/2].(*sqliteSession)
	// left is how many sessions the checkpoint had yet to write when the
	// session's write had its turn.
	left, wrote := 0, make(chan error, 1)
	go func() {
		wrote <- writer.leased(ctx, true, func(ctx context.Context, conn *sql.Conn) error {
			w := store.writer
			w.mu.Lock()
			if w.applying != nil {
				left = len(w.applying.sessions)
			}
			w.mu.Unlock()
			return nil
		})
	}()
	// The checkpoint gets the connection once the write is under way.
	cache := store.coordination
	eventually(t, "the write never started", func() bool {
		cache.held <- struct{}{}
		defer func() { <-cache.held }()
		return writer.coord.writers == 1
	})
	free()
	if err := <-wrote; err != nil {
		t.Fatal(err)
	}
	if left == 0 {
		t.Fatal("the checkpoint had written every session when the write had its turn: the write waited for it")
	}
	if applying, window := pendingIn(store, writer.id); applying != -1 || window != -1 {
		t.Fatalf("after the session's write %d and %d of its messages are pending", applying, window)
	}
	done()
	if stored := storedMessages(t, store); stored != members {
		t.Fatalf("%d messages stored, want %d", stored, members)
	}
}

// What is pending for a session in the checkpoint under way and what was
// accepted for it since are written oldest first, so the messages keep their
// order and the first outcome given is the one stored.
func TestSessionWritesWhatIsPendingInTwoWindowsOldestFirst(t *testing.T) {
	ctx := context.Background()
	store, _, _ := drivenFamily(t)
	child := pendingChild(t, store)
	defer child.Close()
	id := child.(*sqliteSession).id
	recordOutcome(t, child, ReportPaused)
	finish := takeWindow(t, store)
	updateFrom(t, child.(CoordinationSession), changeRecord("task", "a", `{"n":3}`, "two"))
	recordOutcome(t, child, ReportFinished)
	if applying, later := pendingIn(store, id); applying != 1 || later != 1 {
		t.Fatalf("%d and %d messages are pending in the two windows, want one in each", applying, later)
	}

	if err := child.AddMessage(ctx, userMessage("three")); err != nil {
		t.Fatal(err)
	}
	if applying, later := pendingIn(store, id); applying != -1 || later != -1 {
		t.Fatalf("after the session's write %d and %d of its messages are pending", applying, later)
	}
	if stored := storedMessages(t, store); stored != 3 {
		t.Fatalf("%d messages stored after the session's write, want 3", stored)
	}
	if outcome, _ := storedOutcome(t, store, id); outcome != ReportPaused {
		t.Fatalf("the stored outcome is %q, want the first one given", outcome)
	}
	// The checkpoint goes on, and finds the session written.
	finish()
	if err := store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := historyText(t, child); got != "one,two,three" {
		t.Fatalf("transcript = %q", got)
	}
	if record, stored := storedRecord(t, store, "task", "a"), storedMessages(t, store); record != `{"n":3}` || stored != 3 {
		t.Fatalf("after the checkpoint the record is %q and %d messages are stored", record, stored)
	}
}

// A lease's release writes what is pending for its session as the session's
// own write does: both windows' parts, oldest first, and then they are
// pending no longer.
func TestReleaseWritesWhatIsPendingInTwoWindowsOldestFirst(t *testing.T) {
	ctx := context.Background()
	store, _, _ := drivenFamily(t)
	child := pendingChild(t, store)
	id := child.(*sqliteSession).id
	recordOutcome(t, child, ReportPaused)
	finish := takeWindow(t, store)
	updateFrom(t, child.(CoordinationSession), changeRecord("task", "a", `{"n":3}`, "two"))
	recordOutcome(t, child, ReportFinished)
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	if applying, later := pendingIn(store, id); applying != 1 || later != 1 {
		t.Fatalf("%d and %d messages are pending in the two windows, want one in each", applying, later)
	}

	if err := store.releaseKept(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if applying, later := pendingIn(store, id); applying != -1 || later != -1 {
		t.Fatalf("after the release %d and %d of the session's messages are pending", applying, later)
	}
	if stored := storedMessages(t, store); stored != 2 {
		t.Fatalf("%d messages stored after the release, want 2", stored)
	}
	if outcome, _ := storedOutcome(t, store, id); outcome != ReportPaused {
		t.Fatalf("the stored outcome is %q, want the first one given", outcome)
	}
	if owner, _ := storedLease(t, store, id); owner != nil {
		t.Fatal("the lease is still stored")
	}
	finish()
	if err := store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if record, stored := storedRecord(t, store, "task", "a"), storedMessages(t, store); record != `{"n":3}` || stored != 2 {
		t.Fatalf("after the checkpoint the record is %q and %d messages are stored", record, stored)
	}
}

// A release of several leases writes every session's pending messages.
func TestReleaseOfSeveralLeasesWritesEachSessionsPending(t *testing.T) {
	ctx := context.Background()
	store, _, _ := drivenFamily(t)
	var ids [][]byte
	for _, name := range []string{"x", "y", "z"} {
		child, err := store.Acquire(ctx, name, AcquireOptions{Parent: "parent"})
		if err != nil {
			t.Fatal(err)
		}
		updateFrom(t, child.(CoordinationSession), changeRecord("task", name, `{"n":1}`, name))
		ids = append(ids, child.(*sqliteSession).id)
		if err := child.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.releaseKept(ctx, nil); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if applying, window := pendingIn(store, id); applying != -1 || window != -1 {
			t.Fatalf("after the release %d and %d of a session's messages are pending", applying, window)
		}
		if owner, _ := storedLease(t, store, id); owner != nil {
			t.Fatal("a lease is still stored")
		}
	}
	if stored := storedMessages(t, store); stored != len(ids) {
		t.Fatalf("%d messages stored after the release, want %d", stored, len(ids))
	}
}

// Once a checkpoint has failed for good, a session whose messages it holds
// can neither write nor give up its lease, and a release that comes to such
// a session changes nothing for the sessions before it either. The log still
// holds every update, for the store that opens it next.
func TestWriteAndReleaseOverAFailedCheckpointChangeNothing(t *testing.T) {
	ctx := context.Background()
	store, path, _ := drivenFamily(t)
	var held []lease
	var members []Session
	for _, name := range []string{"first", "second"} {
		member, err := store.Acquire(ctx, name, AcquireOptions{Parent: "parent"})
		if err != nil {
			t.Fatal(err)
		}
		members = append(members, member)
		held = append(held, lease{id: member.(*sqliteSession).id, token: member.(*sqliteSession).ownerToken})
	}
	first, second := members[0], members[1]
	// The checkpoint that fails holds the second session's message; the
	// first session's is accepted after it started.
	updateFrom(t, second.(CoordinationSession), changeRecord("task", "b", `{"n":1}`, "second"))
	takeWindow(t, store)
	updateFrom(t, first.(CoordinationSession), changeRecord("task", "c", `{"n":1}`, "first"))
	w := store.writer
	if err := w.wait(ctx, accepted(store)); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("the checkpoint failed")
	w.mu.Lock()
	w.applyErr = failure
	w.stopped.Store(true)
	w.mu.Unlock()

	if err := second.AddMessage(ctx, userMessage("more")); !errors.Is(err, failure) {
		t.Fatalf("a write of the session the failed checkpoint holds returned %v", err)
	}
	if err := store.release(ctx, held...); !errors.Is(err, failure) {
		t.Fatalf("the release returned %v", err)
	}
	if applying, window := pendingIn(store, held[0].id); applying != -1 || window != 1 {
		t.Fatalf("%d and %d of the first session's messages are pending, want its one as it was", applying, window)
	}
	if applying, window := pendingIn(store, held[1].id); applying != 1 || window != -1 {
		t.Fatalf("%d and %d of the second session's messages are pending, want its one as it was", applying, window)
	}
	if stored := storedMessages(t, store); stored != 0 {
		t.Fatalf("%d messages stored", stored)
	}
	for _, one := range held {
		if owner, _ := storedLease(t, store, one.id); owner == nil {
			t.Fatal("a release that failed gave up a lease")
		}
	}

	next := afterCrash(t, store, path)
	if stored := storedMessages(t, next); stored != 2 {
		t.Fatalf("%d messages stored once the log was recovered, want 2", stored)
	}
}

// Pending messages that do not follow the stored transcript are dropped by
// the session's own write as a checkpoint drops them, and are pending no
// longer once the write commits.
func TestSessionWriteDropsPendingThatDoesNotFollowTheTranscript(t *testing.T) {
	ctx := context.Background()
	store, _, _ := drivenFamily(t)
	child := pendingChild(t, store)
	defer child.Close()
	id := child.(*sqliteSession).id
	w := store.writer
	w.mu.Lock()
	w.window.sessions[string(id)].start = 7
	w.mu.Unlock()
	if err := child.AddMessage(ctx, userMessage("two")); err != nil {
		t.Fatal(err)
	}
	if applying, window := pendingIn(store, id); applying != -1 || window != -1 {
		t.Fatalf("after the write %d and %d of the session's messages are pending", applying, window)
	}
	if got := historyText(t, child); got != "two" {
		t.Fatalf("transcript = %q", got)
	}
}

// What a transaction wrote of a session leaves what is pending for it once
// the transaction has committed, and only that: what was accepted for the
// session meanwhile stays, and so does what another lease left.
func TestWrittenPartLeavesWhatWasAcceptedSince(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	w := store.writer
	id, token := []byte("0123456789abcdef"), []byte("fedcba9876543210")
	payload := func(text string) []byte { return []byte(`{"role":"user","content":"` + text + `"}`) }
	first, second := &TranscriptIdentity{Sequence: 1}, &TranscriptIdentity{Sequence: 2}
	pending := &pendingSession{id: id, token: token, start: 4, tail: [][]byte{payload("one")}, transcript: first}
	w.mu.Lock()
	w.window.sessions[string(id)] = pending
	w.mu.Unlock()
	t.Cleanup(func() {
		w.mu.Lock()
		w.applying, w.window = nil, newPendingWindow()
		w.mu.Unlock()
	})

	if parts, err := w.pendingFor(id, []byte("another lease's")); err != nil || len(parts) != 0 {
		t.Fatalf("pending under another lease = %+v, %v", parts, err)
	}
	parts, err := w.pendingFor(id, token)
	if err != nil || len(parts) != 1 || parts[0].session.start != 4 || len(parts[0].session.tail) != 1 {
		t.Fatalf("pending = %+v, %v", parts, err)
	}
	if applying, window := pendingIn(store, id); applying != -1 || window != 1 {
		t.Fatalf("looking at what is pending left %d and %d messages pending", applying, window)
	}
	// Updates are accepted for the session, and a checkpoint takes the
	// window, before the transaction commits.
	w.mu.Lock()
	pending.tail = append(pending.tail, payload("two"))
	pending.transcript = second
	pending.outcome, pending.outcomeNS = ReportFinished, 7
	w.applying, w.window = w.window, newPendingWindow()
	w.mu.Unlock()
	w.wrote(parts)
	w.mu.Lock()
	left := w.applying.sessions[string(id)]
	w.mu.Unlock()
	if left != pending || left.start != 5 || len(left.tail) != 1 || string(left.tail[0]) != string(payload("two")) || left.transcript != second || left.outcome != ReportFinished {
		t.Fatalf("after the commit %+v is pending, want what was accepted since", left)
	}

	parts, err = w.pendingFor(id, token)
	if err != nil || len(parts) != 1 || parts[0].session.start != 5 {
		t.Fatalf("pending = %+v, %v", parts, err)
	}
	w.wrote(parts)
	if applying, window := pendingIn(store, id); applying != -1 || window != -1 {
		t.Fatalf("after the second commit %d and %d messages are pending", applying, window)
	}
}
