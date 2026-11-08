package sessions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
)

func seedIntegrityViolation(t *testing.T, store *SQLiteStore, foreignKey bool) {
	t.Helper()
	var statements []string
	if foreignKey {
		statements = []string{"PRAGMA foreign_keys=OFF", `INSERT INTO messages VALUES(zeroblob(16),0,x'7b7d')`, "PRAGMA foreign_keys=ON"}
	} else {
		statements = []string{"PRAGMA ignore_check_constraints=ON", `INSERT INTO artifact_blobs VALUES(zeroblob(32),-1,0,0)`, "PRAGMA ignore_check_constraints=OFF"}
	}
	for _, statement := range statements {
		if _, err := store.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDeferredIntegrityStartupDefaults(t *testing.T) {
	for _, foreignKey := range []bool{false, true} {
		for _, deferred := range []bool{false, true} {
			t.Run(fmt.Sprintf("fk=%t/deferred=%t", foreignKey, deferred), func(t *testing.T) {
				store, path := openTestStore(t, ModeDisk, nil, 0)
				seedIntegrityViolation(t, store, foreignKey)
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
				config := StoreConfig{Mode: ModeDisk, Path: path}
				if deferred {
					config.Integrity = IntegrityDeferred
					config.DeferredIntegrityInterval = time.Hour
				}
				store, err := OpenStore(config)
				if deferred {
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = store.Close() })
					status, err := store.MaintenanceStatus(t.Context())
					if err != nil || status.IntegrityInterval != time.Hour || !status.LastIntegrityCheck.IsZero() || status.Corrupt {
						t.Fatalf("deferred status=%+v %v", status, err)
					}
					if err := store.CheckIntegrity(t.Context()); !errors.Is(err, ErrStoreCorrupt) {
						t.Fatalf("explicit check=%v", err)
					}
				} else if err == nil || !errors.Is(err, ErrStoreCorrupt) {
					if store != nil {
						_ = store.Close()
					}
					t.Fatalf("unsafe startup accepted: %v", err)
				}
				raw, err := sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				defer raw.Close()
				var actual int
				if err := raw.QueryRow("PRAGMA user_version").Scan(&actual); err != nil || actual != schemaVersion {
					t.Fatalf("failed validation changed version: %d %v", actual, err)
				}
			})
		}
	}
}

func TestDeferredIntegrityRetainsSchemaValidation(t *testing.T) {
	store, path := openTestStore(t, ModeDisk, nil, 0)
	if _, err := store.db.Exec(`DROP INDEX sessions_spawn_idx; ALTER TABLE sessions DROP COLUMN settings_json`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if reopened, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path, Integrity: IntegrityDeferred, DeferredIntegrityInterval: time.Hour}); err == nil || !strings.Contains(err.Error(), "table sessions") {
		if reopened != nil {
			_ = reopened.Close()
		}
		t.Fatalf("deferred open skipped schema validation: %v", err)
	}
	if store, err := OpenStore(StoreConfig{Mode: ModeMemory, Integrity: IntegrityDeferred, DeferredIntegrityInterval: -time.Second}); store != nil || err == nil {
		t.Fatalf("negative interval accepted: %v", err)
	}
}

func TestIntegrityConfirmedCorruptionFencesEveryWriter(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	session := acquireNamed(t, store, "owned").(*sqliteSession)
	seedIntegrityViolation(t, store, false)
	if err := store.CheckIntegrity(t.Context()); !errors.Is(err, ErrStoreCorrupt) {
		t.Fatal(err)
	}
	writes := map[string]func() error{
		"acquire": func() error { _, err := store.Acquire(t.Context(), "other", AcquireOptions{}); return err },
		"message": func() error {
			return session.AddMessage(t.Context(), messages.ChatMessage{Role: messages.MessageRoleUser, Content: "no"})
		},
		"cache":    func() error { return store.PutModelCache(t.Context(), "key", []byte("no")) },
		"clear":    func() error { return session.Clear(t.Context()) },
		"remove":   func() error { return session.ArtifactStore().RemoveAll(t.Context()) },
		"renew":    func() error { return store.renewLeaseBatch(t.Context(), []*sqliteSession{session}) },
		"release":  func() error { return store.releaseLease(t.Context(), session.id, session.ownerToken) },
		"expire":   func() error { return store.Expire(t.Context()) },
		"maintain": func() error { _, err := store.Maintain(t.Context(), MaintenanceOptions{Limit: 1}); return err },
		"promote":  func() error { return store.Promote(t.Context(), filepath.Join(t.TempDir(), "blocked.db")) },
	}
	for name, write := range writes {
		if err := write(); !errors.Is(err, ErrStoreCorrupt) {
			t.Fatalf("%s was not fenced: %v", name, err)
		}
	}
	status, _ := store.MaintenanceStatus(t.Context())
	if !status.Corrupt || !errors.Is(status.LastIntegrityError, ErrStoreCorrupt) || status.LastIntegrityCheck.IsZero() {
		t.Fatalf("status=%+v", status)
	}
	// Even an external repair and a successful check cannot silently reopen
	// this handle's write authority. Its previous verdict remains latched.
	if _, err := store.db.Exec(`DELETE FROM artifact_blobs`); err != nil {
		t.Fatal(err)
	}
	if err := store.CheckIntegrity(t.Context()); err != nil {
		t.Fatal(err)
	}
	status, _ = store.MaintenanceStatus(t.Context())
	if !status.Corrupt || status.LastIntegrityError != nil {
		t.Fatalf("successful recheck cleared latch or retained stale error: %+v", status)
	}
	if err := store.Close(); !errors.Is(err, ErrStoreCorrupt) {
		t.Fatalf("close=%v", err)
	}
}

func TestIntegrityTransientAndCancellationDoNotFenceWrites(t *testing.T) {
	store, path := openTestStore(t, ModeDisk, nil, 0)
	d := maintenanceInjectDriver(t, store, path)
	d.failCheck.Store(true)
	if err := store.CheckIntegrity(t.Context()); !errors.Is(err, errMaintenanceInjected) {
		t.Fatalf("transient error=%v", err)
	}
	status, _ := store.MaintenanceStatus(t.Context())
	if status.Corrupt || !errors.Is(status.LastIntegrityError, errMaintenanceInjected) {
		t.Fatalf("transient status=%+v", status)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := store.CheckIntegrity(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled check=%v", err)
	}
	status, _ = store.MaintenanceStatus(t.Context())
	if status.Corrupt || !errors.Is(status.LastIntegrityError, context.Canceled) {
		t.Fatalf("cancellation status=%+v", status)
	}
	if err := store.PutModelCache(t.Context(), "after", []byte("healthy")); err != nil {
		t.Fatalf("transient/cancel fenced writes: %v", err)
	}
	if err := store.CheckIntegrity(t.Context()); err != nil {
		t.Fatal(err)
	}
	status, _ = store.MaintenanceStatus(t.Context())
	if status.Corrupt || status.LastIntegrityError != nil {
		t.Fatalf("successful status=%+v", status)
	}
}

func TestIntegrityQueuedWriterObservesCorruptionFence(t *testing.T) {
	store, path := openTestStore(t, ModeDisk, nil, 0)
	seedIntegrityViolation(t, store, false)
	d := maintenanceInjectDriver(t, store, path)
	d.pauseCheck.Store(true)
	check := make(chan error, 1)
	go func() { check <- store.CheckIntegrity(t.Context()) }()
	<-d.checkEntered
	waitCount := store.db.Stats().WaitCount
	write := make(chan error, 1)
	go func() { write <- store.PutModelCache(t.Context(), "queued", []byte("must not write")) }()
	maintenanceWait(t, func() bool { return store.db.Stats().WaitCount > waitCount })
	close(d.checkRelease)
	if err := <-check; !errors.Is(err, ErrStoreCorrupt) {
		t.Fatalf("check=%v", err)
	}
	if err := <-write; !errors.Is(err, ErrStoreCorrupt) {
		t.Fatalf("queued writer bypassed fence: %v", err)
	}
	if maintenanceCount(t, store, "model_metadata_cache") != 0 {
		t.Fatal("queued write committed")
	}
}

func TestDeferredIntegritySchedulerAndCloseCancellation(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(fmt.Sprint("corrupt=", corrupt), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "scheduled.db")
			store, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path, Integrity: IntegrityDeferred, DeferredIntegrityInterval: 10 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			if corrupt {
				seedIntegrityViolation(t, store, false)
				maintenanceWait(t, func() bool {
					status, _ := store.MaintenanceStatus(t.Context())
					return status.Corrupt
				})
				status, _ := store.MaintenanceStatus(t.Context())
				if status.LastIntegrityCheck.IsZero() || !errors.Is(status.LastIntegrityError, ErrStoreCorrupt) || status.IntegrityInterval != 10*time.Millisecond {
					t.Fatalf("scheduled failure=%+v", status)
				}
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
				return
			}
			d := maintenanceInjectDriver(t, store, path)
			d.pauseCheck.Store(true)
			select {
			case <-d.checkEntered:
			case <-time.After(5 * time.Second):
				t.Fatal("scheduled check did not run")
			}
			closed := make(chan error, 1)
			go func() { closed <- store.Close() }()
			select {
			case err := <-closed:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Close did not cancel scheduled check")
			}
			status, _ := store.MaintenanceStatus(t.Context())
			if status.Corrupt || !errors.Is(status.LastIntegrityError, context.Canceled) {
				t.Fatalf("shutdown status=%+v", status)
			}
			if err := store.CheckIntegrity(t.Context()); !errors.Is(err, ErrStoreClosed) {
				t.Fatalf("closed explicit check=%v", err)
			}
		})
	}
}
