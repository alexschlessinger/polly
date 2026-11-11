package sessions

import (
	"context"
	"encoding/hex"
	"testing"
	"time"
)

// storedOutcome reads the outcome and last update the database holds for the
// session id.
func storedOutcome(t *testing.T, store *SQLiteStore, id []byte) (outcome ReportStatus, updatedNS int64) {
	t.Helper()
	var stored *string
	err := store.db.QueryRow("SELECT json_extract(CAST(settings_json AS TEXT), '$.spawnOutcome'), updated_ns FROM sessions WHERE id = ?", id).Scan(&stored, &updatedNS)
	if err != nil {
		t.Fatal(err)
	}
	if stored != nil {
		outcome = ReportStatus(*stored)
	}
	return outcome, updatedNS
}

func recordOutcome(t *testing.T, session Session, outcome ReportStatus) {
	t.Helper()
	if err := session.(SpawnOutcomeSession).RecordSpawnOutcome(context.Background(), outcome); err != nil {
		t.Fatal(err)
	}
}

// A member of a family this store drives notes how its run ended without
// asking the database: the outcome is the member's own to read at once, and
// reaches the database with the next checkpoint. The first outcome stays, and
// a later one leaves the session as it was.
func TestMemberOfADrivenFamilyRecordsItsOutcomeBehindItself(t *testing.T) {
	ctx := context.Background()
	store, _, _ := drivenFamily(t)
	child, err := store.Acquire(ctx, "child", AcquireOptions{Parent: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	updateFrom(t, child.(CoordinationSession), changeRecord("task", "a", `{"n":2}`, "one"))
	id := child.(*sqliteSession).id

	// With the store's only connection taken, the database cannot be asked.
	conn, err := store.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	prompt, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	err = child.(SpawnOutcomeSession).RecordSpawnOutcome(prompt, ReportPaused)
	if closeErr := conn.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil {
		t.Fatalf("recording the outcome: %v", err)
	}
	if stored, _ := storedOutcome(t, store, id); stored != "" {
		t.Fatalf("outcome %q reached the database before a checkpoint", stored)
	}
	recordOutcome(t, child, ReportFailed)
	if metadata, err := child.GetMetadata(ctx); err != nil || metadata.SpawnOutcome != ReportPaused {
		t.Fatalf("the member reads outcome %+v, %v", metadata, err)
	}
	if err := store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	stored, written := storedOutcome(t, store, id)
	if stored != ReportPaused {
		t.Fatalf("outcome after a checkpoint = %q", stored)
	}

	// The next slice, on the lease the store kept, ends another run.
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := store.Acquire(ctx, "child", AcquireOptions{ExistingOnly: true, ExpectedID: hex.EncodeToString(id)})
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	recordOutcome(t, again, ReportFinished)
	if err := store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if stored, updated := storedOutcome(t, store, id); stored != ReportPaused || updated != written {
		t.Fatalf("a later outcome left %q, updated %d, want the first and %d", stored, updated, written)
	}
	if err := again.(SpawnOutcomeSession).RecordSpawnOutcome(ctx, "done"); err == nil {
		t.Fatal("an unknown outcome was recorded")
	}
}

// An outcome the store that noted it never wrote is in its log, and the next
// store to open the database writes it.
func TestRecoveryWritesALoggedOutcome(t *testing.T) {
	ctx := context.Background()
	store, path, _ := drivenFamily(t)
	child, err := store.Acquire(ctx, "child", AcquireOptions{Parent: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	updateFrom(t, child.(CoordinationSession), changeRecord("task", "a", `{"n":2}`, "one"))
	recordOutcome(t, child, ReportCanceled)
	id := child.(*sqliteSession).id
	if stored, _ := storedOutcome(t, store, id); stored != "" {
		t.Fatalf("outcome %q reached the database before a checkpoint", stored)
	}
	crashStore(t, store)

	next, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if stored, _ := storedOutcome(t, next, id); stored != ReportCanceled {
		t.Fatalf("outcome after recovery = %q", stored)
	}
}

// A session whose family this store does not drive has its outcome written
// before the call returns, once.
func TestOutcomeOutsideADrivenFamilyIsWrittenAtOnce(t *testing.T) {
	ctx := context.Background()
	store, _ := openTestStore(t, ModeDisk, nil, 0)
	parent, err := store.Acquire(ctx, "parent", AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}
	child, err := store.Acquire(ctx, "child", AcquireOptions{Parent: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	id := child.(*sqliteSession).id
	recordOutcome(t, child, ReportFinished)
	stored, written := storedOutcome(t, store, id)
	if stored != ReportFinished {
		t.Fatalf("outcome after the call = %q", stored)
	}
	recordOutcome(t, child, ReportFailed)
	if stored, updated := storedOutcome(t, store, id); stored != ReportFinished || updated != written {
		t.Fatalf("a later outcome left %q, updated %d, want the first and %d", stored, updated, written)
	}
	if metadata, err := store.GetMetadata(ctx, "child"); err != nil || metadata.SpawnOutcome != ReportFinished {
		t.Fatalf("stored metadata = %+v, %v", metadata, err)
	}
}

// What is pending for a session in two windows comes to the first outcome.
func TestJoinedPendingKeepsTheFirstOutcome(t *testing.T) {
	token := []byte("lease")
	older := &pendingSession{token: token, outcome: ReportPaused, outcomeNS: 1}
	newer := &pendingSession{token: token, outcome: ReportFinished, outcomeNS: 2}
	if joined := joinPending(older, newer); joined.outcome != ReportPaused || joined.outcomeNS != 1 {
		t.Fatalf("joined outcome = %q at %d", joined.outcome, joined.outcomeNS)
	}
	if joined := joinPending(&pendingSession{token: token}, newer); joined.outcome != ReportFinished || joined.outcomeNS != 2 {
		t.Fatalf("joined outcome = %q at %d", joined.outcome, joined.outcomeNS)
	}
}
