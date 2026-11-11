package sessions

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"testing"
	"time"
)

// storedLease reads the lease the database holds on the session id: its
// owner, nil when there is none, and when it runs out.
func storedLease(t *testing.T, store *SQLiteStore, id []byte) (owner []byte, expires int64) {
	t.Helper()
	err := store.db.QueryRow("SELECT owner_token, expires_ns FROM session_leases WHERE session_id = ?", id).Scan(&owner, &expires)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	return owner, expires
}

// drivenFamily opens a store with the family's root acquired as "parent".
func drivenFamily(t *testing.T) (*SQLiteStore, string, Session) {
	t.Helper()
	holdCheckpoints(t)
	store, path := openTestStore(t, ModeDisk, nil, 0)
	parent := acquireNamed(t, store, "parent")
	if err := parent.(CoordinationSession).UpdateCoordination(context.Background(), setRecord("task", "a", `{"n":1}`)); err != nil {
		t.Fatal(err)
	}
	return store, path, parent
}

// memberSlice acquires the member "child" as the runtime does for a slice,
// has it change a record and append a message, and closes it. It returns
// the session's id and the owner of the lease the slice ran under.
func memberSlice(t *testing.T, store *SQLiteStore, expected []byte, message string) (id, owner []byte) {
	t.Helper()
	options := AcquireOptions{Parent: "parent"}
	if expected != nil {
		options = AcquireOptions{ExistingOnly: true, ExpectedID: hex.EncodeToString(expected)}
	}
	child, err := store.Acquire(context.Background(), "child", options)
	if err != nil {
		t.Fatal(err)
	}
	updateFrom(t, child.(CoordinationSession), changeRecord("task", "a", `{"n":2}`, message))
	concrete := child.(*sqliteSession)
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	return concrete.id, concrete.ownerToken
}

// The store that drives a family starts each slice of its members. When a
// member's handle closes, the store keeps the lease and hands it to the next
// handle on the session without asking the database. Other stores see the
// session leased meanwhile; the store's own readers see it free.
func TestStoreKeepsAMembersLeaseForItsNextHandle(t *testing.T) {
	ctx := context.Background()
	store, path, _ := drivenFamily(t)
	id, owner := memberSlice(t, store, nil, "one")
	if stored, _ := storedLease(t, store, id); !bytes.Equal(stored, owner) {
		t.Fatalf("lease after the member closed = %x, want the one it held", stored)
	}
	other, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	for _, reader := range []*SQLiteStore{store, other} {
		view, err := reader.ReadView(ctx, ViewTarget{ID: hex.EncodeToString(id)}, "")
		if err != nil {
			t.Fatal(err)
		}
		summaries, err := reader.ListSummaries(ctx)
		if err != nil {
			t.Fatal(err)
		}
		listed, _ := summaryByName(summaries, "child")
		if elsewhere := reader == other; view.InUse != elsewhere || listed.InUse != elsewhere {
			t.Fatalf("in use to another store = %v: view %v, summary %v", elsewhere, view.InUse, listed.InUse)
		}
	}
	// The session by another name is a different session.
	if _, err := store.Acquire(ctx, "renamed", AcquireOptions{ExistingOnly: true, ExpectedID: hex.EncodeToString(id)}); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("acquiring the member by another name = %v", err)
	}

	// With the store's only connection taken, the database cannot be asked.
	conn, err := store.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	prompt, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	again, err := store.Acquire(prompt, "child", AcquireOptions{ExistingOnly: true, ExpectedID: hex.EncodeToString(id)})
	if closeErr := conn.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil {
		t.Fatalf("taking the kept lease back: %v", err)
	}
	defer again.Close()
	if held := again.(*sqliteSession).ownerToken; !bytes.Equal(held, owner) {
		t.Fatalf("the next handle holds lease %x, want %x", held, owner)
	}
	if got := historyText(t, again); got != "one" {
		t.Fatalf("transcript on the next handle = %q", got)
	}
	updateFrom(t, again.(CoordinationSession), changeRecord("task", "a", `{"n":3}`, "two"))
	if got := historyText(t, again); got != "one,two" {
		t.Fatalf("transcript after the next handle appended = %q", got)
	}
}

func summaryByName(summaries []SessionSummary, name string) (SessionSummary, bool) {
	for _, summary := range summaries {
		if summary.Metadata.Name == name {
			return summary, true
		}
	}
	return SessionSummary{}, false
}

// A lease is kept only for a member of a family the store drives: a session
// on its own, a family's root, and a member whose root has closed give
// theirs up when they close.
func TestOnlyAMemberOfADrivenFamilyLeavesItsLease(t *testing.T) {
	ctx := context.Background()
	store, _, parent := drivenFamily(t)
	alone, err := store.Acquire(ctx, "alone", AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.Acquire(ctx, "child", AcquireOptions{Parent: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	updateFrom(t, child.(CoordinationSession), changeRecord("task", "a", `{"n":2}`, "one"))
	for _, session := range []Session{alone, parent, child} {
		id := session.(*sqliteSession).id
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
		if owner, _ := storedLease(t, store, id); owner != nil {
			t.Fatalf("a lease is left on session %x", id)
		}
	}
	if got := storedMessages(t, store); got != 1 {
		t.Fatalf("%d messages in the table after the member closed", got)
	}
}

// When a family's root closes, nobody is left to start its members' slices:
// the leases kept on them go, and what was pending for them is written.
func TestRootClosingGivesUpItsMembersLeases(t *testing.T) {
	store, _, parent := drivenFamily(t)
	id, _ := memberSlice(t, store, nil, "one")
	if got := storedMessages(t, store); got != 0 {
		t.Fatalf("%d messages in the table while the lease is kept", got)
	}
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}
	if owner, _ := storedLease(t, store, id); owner != nil {
		t.Fatal("the member's lease outlived its family's root")
	}
	if got := storedMessages(t, store); got != 1 {
		t.Fatalf("%d messages in the table after the root closed", got)
	}
}

// A kept lease gives way to a caller of its own store that wants the session
// some other way: by name alone, or gone.
func TestKeptLeaseGivesWayInItsOwnStore(t *testing.T) {
	ctx := context.Background()
	store, _, _ := drivenFamily(t)
	id, owner := memberSlice(t, store, nil, "one")
	prompt, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	named, err := store.Acquire(prompt, "child", AcquireOptions{ExistingOnly: true})
	if err != nil {
		t.Fatalf("acquiring a kept session by name: %v", err)
	}
	if held := named.(*sqliteSession).ownerToken; bytes.Equal(held, owner) {
		t.Fatal("a session acquired by name alone got the kept lease")
	}
	if got := historyText(t, named); got != "one" {
		t.Fatalf("transcript after the kept lease gave way = %q", got)
	}
	if err := named.Close(); err != nil {
		t.Fatal(err)
	}
	memberSlice(t, store, id, "two")
	if !store.keeps(id) {
		t.Fatal("the member's lease was not kept")
	}
	if err := store.Delete(ctx, "child"); err != nil {
		t.Fatalf("deleting a kept session: %v", err)
	}
	if exists, err := store.Exists(ctx, "child"); err != nil || exists {
		t.Fatalf("the deleted session exists = %v, %v", exists, err)
	}
}

// The store renews the leases it keeps along with those its handles hold,
// and gives one up once it has kept it for leaseKeep.
func TestStoreRenewsThenGivesUpAKeptLease(t *testing.T) {
	overrideLeaseTiming(t, 10*time.Millisecond, 200*time.Millisecond)
	old := leaseKeep
	leaseKeep = 400 * time.Millisecond
	t.Cleanup(func() { leaseKeep = old })
	store, _, _ := drivenFamily(t)
	id, _ := memberSlice(t, store, nil, "one")
	_, first := storedLease(t, store, id)
	// Past the time the lease as first taken would have run out.
	time.Sleep(250 * time.Millisecond)
	owner, renewed := storedLease(t, store, id)
	if owner == nil || renewed <= first || renewed <= time.Now().UnixNano() {
		t.Fatalf("kept lease: owner %x, expiry moved by %v", owner, time.Duration(renewed-first))
	}
	if !store.keeps(id) || storedMessages(t, store) != 0 {
		t.Fatal("the lease was given up before its time")
	}
	eventually(t, "the kept lease was never given up", func() bool {
		owner, _ := storedLease(t, store, id)
		return owner == nil
	})
	if got := storedMessages(t, store); got != 1 {
		t.Fatalf("%d messages in the table after the lease was given up", got)
	}
}

// A kept lease that somebody else took is not handed out: the store learns
// of it at its next heartbeat, and the session is then theirs.
func TestKeptLeaseTakenElsewhereIsNotHandedOut(t *testing.T) {
	overrideLeaseTiming(t, 10*time.Millisecond, 5*time.Second)
	overrideLeaseAcquire(t, 50*time.Millisecond, 10*time.Millisecond)
	ctx := context.Background()
	store, _, _ := drivenFamily(t)
	id, _ := memberSlice(t, store, nil, "one")
	if _, err := store.db.ExecContext(ctx, "UPDATE session_leases SET owner_token = ? WHERE session_id = ?", []byte("replacementowner"), id); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the store still keeps a lease it lost", func() bool { return !store.keeps(id) })
	_, err := store.Acquire(ctx, "child", AcquireOptions{ExistingOnly: true, ExpectedID: hex.EncodeToString(id)})
	if !errors.Is(err, ErrSessionInUse) {
		t.Fatalf("acquiring a session leased elsewhere = %v", err)
	}
}
