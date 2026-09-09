package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestCoordinationViewIsLeaseFreeAndIdentityScoped(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	ctx := context.Background()
	parent := acquireNamed(t, store, "parent")
	id := parent.(ViewIdentity).ViewID()
	if err := parent.(CoordinationSession).UpdateCoordination(ctx, func(s *CoordinationState) error {
		s.Records["run"] = map[string]json.RawMessage{"r": json.RawMessage(`{"status":"completed"}`)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, _ := parent.GetLastUsed(ctx)
	view, err := store.ReadCoordinationView(ctx, id)
	if err != nil || view.ParentID != id || string(view.Records["run"]["r"]) != `{"status":"completed"}` {
		t.Fatalf("leased parent view: %+v, %v", view, err)
	}
	after, _ := parent.GetLastUsed(ctx)
	if !after.Equal(before) {
		t.Fatal("inspection touched last-used time")
	}
	child, err := store.Acquire(ctx, "child", AcquireOptions{Parent: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadCoordinationView(ctx, child.(ViewIdentity).ViewID()); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("child identity exposed parent records: %v", err)
	}
	_ = child.Close()
	other := acquireNamed(t, store, "other")
	otherView, err := store.ReadCoordinationView(ctx, other.(ViewIdentity).ViewID())
	if err != nil || len(otherView.Records) != 0 {
		t.Fatalf("cross-family records: %+v, %v", otherView, err)
	}
	_ = other.Close()
	if err := parent.Rename(ctx, "renamed"); err != nil {
		t.Fatal(err)
	}
	_ = parent.Close()
	if _, err := store.ReadCoordinationView(ctx, id); err != nil {
		t.Fatal(err)
	}
	var leases int
	if err := store.db.QueryRowContext(ctx, "SELECT count(*) FROM session_leases").Scan(&leases); err != nil || leases != 0 {
		t.Fatalf("inspection acquired a lease: %d, %v", leases, err)
	}
	if err := store.Delete(ctx, "renamed"); err != nil {
		t.Fatal(err)
	}
	replacement := acquireNamed(t, store, "renamed")
	_ = replacement.Close()
	if _, err := store.ReadCoordinationView(ctx, id); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("reused name revived old identity: %v", err)
	}
}

func TestCoordinationViewExpiryHonorsLeaseAndFamilyPins(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, &Metadata{TTL: time.Second}, 0)
	ctx := context.Background()
	for _, pinned := range []bool{false, true} {
		name := "plain"
		if pinned {
			name = "pinned"
		}
		session := acquireNamed(t, store, name)
		id := session.(ViewIdentity).ViewID()
		if pinned {
			if err := session.(CoordinationSession).UpdateCoordination(ctx, func(s *CoordinationState) error {
				s.Records["run"] = map[string]json.RawMessage{"r": json.RawMessage(`{"status":"completed"}`)}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		backdate := func() {
			t.Helper()
			if _, err := store.db.ExecContext(ctx, "UPDATE sessions SET updated_ns=? WHERE id=?", time.Now().Add(-time.Hour).UnixNano(), session.(*sqliteSession).id); err != nil {
				t.Fatal(err)
			}
		}
		backdate()
		if _, err := store.ReadCoordinationView(ctx, id); err != nil {
			t.Fatalf("live lease hidden: %v", err)
		}
		_ = session.Close()
		backdate()
		_, err := store.ReadCoordinationView(ctx, id)
		if pinned && err != nil || !pinned && !errors.Is(err, ErrSessionNotFound) {
			t.Fatalf("pinned=%t: %v", pinned, err)
		}
	}
}
