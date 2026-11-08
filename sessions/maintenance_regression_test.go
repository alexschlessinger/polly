package sessions

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestMaintenanceExpiryBatchBoundsAndFutureRows(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	maintenanceSeedSessions(t, store, 2*MaxMaintenanceLimit+3, false)
	var position expiryPosition
	result, err := store.expireBatch(t.Context(), &position, time.Now().UnixNano())
	if err != nil || result.Examined != 0 || result.More {
		t.Fatalf("future rows consumed candidate budget: %+v %v", result, err)
	}
	// More than one batch of equal deadline keys must not repeat retained
	// prefixes or skip the later IDs. This also exercises the cleanup cursor.
	if _, err := store.db.Exec(`UPDATE sessions SET updated_ns=1; INSERT INTO swarm_members SELECT id,id FROM sessions WHERE name!='candidate-2002'`); err != nil {
		t.Fatal(err)
	}
	for batch, want := range []int{MaxMaintenanceLimit, MaxMaintenanceLimit, 3} {
		result, err = store.expireBatch(t.Context(), &position, time.Now().UnixNano())
		if err != nil || result.Examined != want || result.More != (batch < 2) {
			t.Fatalf("batch %d: %+v %v", batch, result, err)
		}
		if batch == 2 && result.SessionsDeleted != 1 {
			t.Fatalf("retained prefix starved later candidate: %+v", result)
		}
	}
	if position.id != nil {
		t.Fatal("exhausted expiry cursor did not wrap")
	}
	if _, err := store.db.Exec(`DELETE FROM swarm_members`); err != nil {
		t.Fatal(err)
	}
	if err := store.Expire(t.Context()); err != nil {
		t.Fatal(err)
	}
	if maintenanceCount(t, store, "sessions") != 0 {
		t.Fatal("explicit Expire did not exhaust all batches")
	}
}

func TestMaintenanceBlobRollbackAndCancellationWaiting(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	digests := maintenanceSeedBlobs(t, store, 4, nil)
	store.maintenancePosition.blobFirst = true
	if _, err := store.db.Exec(fmt.Sprintf(`CREATE TEMP TRIGGER veto_blob BEFORE DELETE ON artifact_blobs WHEN old.digest=x'%x' BEGIN SELECT RAISE(ABORT,'veto blob'); END`, digests[1])); err != nil {
		t.Fatal(err)
	}
	before := store.maintenancePosition
	result, err := store.Maintain(t.Context(), MaintenanceOptions{Limit: 4})
	if err == nil || !strings.Contains(err.Error(), "veto blob") || result != (MaintenanceResult{}) || maintenanceCount(t, store, "artifact_blobs") != 4 || !reflect.DeepEqual(before, store.maintenancePosition) {
		t.Fatalf("blob rollback=%+v %v", result, err)
	}
	if _, err := store.db.Exec(`DROP TRIGGER veto_blob`); err != nil {
		t.Fatal(err)
	}
	conn, err := store.db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	waitCount := store.db.Stats().WaitCount
	ctx, cancel := context.WithCancel(t.Context())
	finished := make(chan error, 1)
	go func() { _, err := store.Maintain(ctx, MaintenanceOptions{Limit: 4}); finished <- err }()
	maintenanceWait(t, func() bool { return store.db.Stats().WaitCount > waitCount })
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiting cancellation=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("maintenance did not cancel while waiting for connection")
	}
	// A second caller queued on the maintenance gate must also cancel
	// independently instead of waiting for the first caller's database lock.
	store.maintenanceGate <- struct{}{}
	ctx, cancel = context.WithCancel(t.Context())
	go func() { _, err := store.Maintain(ctx, MaintenanceOptions{Limit: 1}); finished <- err }()
	cancel()
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("gate cancellation=%v", err)
	}
	<-store.maintenanceGate
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if maintenanceCount(t, store, "artifact_blobs") != 4 {
		t.Fatal("cancellation performed deletion")
	}
}

func TestMaintenanceExpiryIndexValidation(t *testing.T) {
	for _, wrong := range []bool{false, true} {
		t.Run(fmt.Sprint("wrong=", wrong), func(t *testing.T) {
			store, path := openTestStore(t, ModeDisk, nil, 0)
			maintenanceSeedSessions(t, store, 3, false)
			if _, err := store.db.Exec(`DROP INDEX sessions_deadline_idx`); err != nil {
				t.Fatal(err)
			}
			if wrong {
				if _, err := store.db.Exec(`CREATE INDEX sessions_deadline_idx ON sessions(ttl_ns,id)`); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path, Integrity: IntegrityDeferred, DeferredIntegrityInterval: time.Hour})
			if reopened != nil {
				defer reopened.Close()
			}
			if err == nil {
				t.Fatal("schema-10 store opened with a missing or altered expiry index")
			}
			if wrong && !strings.Contains(err.Error(), "sessions_deadline_idx definition differs") {
				t.Fatalf("altered expiry index error = %v", err)
			}
		})
	}
}
