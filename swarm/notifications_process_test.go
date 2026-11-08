package swarm

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/alexschlessinger/pollytool/sessions"
)

// The child uses only public storage interfaces and never a notification hook.
// This exercises a real separate process's commit-before-notify/no-notify case.
func TestNotificationsCrossProcessWriter(t *testing.T) {
	t.Parallel()
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "notification-writer" {
		return
	}
	path := os.Args[len(os.Args)-1]
	store, err := sessions.OpenStore(sessions.StoreConfig{Mode: sessions.ModeDisk, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	session, err := store.Acquire(t.Context(), "notification-root", sessions.AcquireOptions{ExistingOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	access, err := sessions.OpenCoordinationAccess(session)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := access.UpdateRecords(t.Context(), func(tx sessions.CoordinationTx) error {
		return tx.Put(sessions.RecordKey{Kind: "notification-fixture", ID: "a"}, json.RawMessage(`{"value":"remote"}`))
	}); err != nil {
		t.Fatal(err)
	}
}

func TestNotificationsCrossProcessViewRevisions(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "notifications.db")
	store, err := sessions.OpenStore(sessions.StoreConfig{Mode: sessions.ModeDisk, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	session, err := store.Acquire(t.Context(), "notification-root", sessions.AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	root := session.(sessions.ViewIdentity).ViewID()
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	// Use the exported trusted-view constructor to select the supplied store;
	// only the scheduling clock is replaced for deterministic unchanged checks.
	view, err := NewViewNotifications(t.Context(), store, root)
	if err != nil {
		t.Fatal(err)
	}
	view.Close()
	n, clock, start := notificationTestProvider(t, view.source)
	a := sessions.RevisionScope{Index: sessions.CoordinationIndexRecord, Key: sessions.CoordinationIndexKey("notification-fixture", "a")}
	b := sessions.RevisionScope{Index: sessions.CoordinationIndexRecord, Key: sessions.CoordinationIndexKey("notification-fixture", "b")}
	sa, sb := notificationTestSubscribe(t, n, a), notificationTestSubscribe(t, n, b)
	start()
	timer := changeTestReceive(t, clock.timers)
	changeTestReceive(t, sa.Changes())
	changeTestReceive(t, sb.Changes())
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// This is the test binary, not a product child-process launch path.
	cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestNotificationsCrossProcessWriter$", "notification-writer", path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("separate-process commit: %v\n%s", err, output)
	}
	timer.fire()
	timer = changeTestReceive(t, clock.timers)
	changeTestReceive(t, sa.Changes())
	changeTestQuiet(t, sb.sub)
	// Repeating a poll of the independent commit is not another dirty hint.
	timer.fire()
	timer = changeTestReceive(t, clock.timers)
	changeTestQuiet(t, sa.sub)
	changeTestQuiet(t, sb.sub)
	// The root's disappearance is a stable revision transition for every scope.
	if err := store.Delete(context.Background(), "notification-root"); err != nil {
		t.Fatal(err)
	}
	timer.fire()
	timer = changeTestReceive(t, clock.timers)
	changeTestReceive(t, sa.Changes())
	changeTestReceive(t, sb.Changes())
	timer.fire()
	changeTestReceive(t, clock.timers)
	changeTestQuiet(t, sa.sub)
	changeTestQuiet(t, sb.sub)
}
