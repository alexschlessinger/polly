package sessions

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
)

func TestHeartbeatBatchTransactionBounds(t *testing.T) {
	overrideLeaseTiming(t, time.Hour, 2*time.Hour)
	for _, n := range []int{1, 16, 128, 257} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			store, path := openTestStore(t, ModeDisk, nil, 0)
			owners := make([]*sqliteSession, n)
			for i := range owners {
				owners[i] = acquireNamed(t, store, fmt.Sprint("owner-", i)).(*sqliteSession)
			}
			d := maintenanceInjectDriver(t, store, path)
			// Measure the previous one-autocommit-per-session implementation on
			// the same SQLite/WAL/FULL connection and owners, not a mock.
			start := time.Now()
			for _, owner := range owners {
				now, expiresNS := leaseWindow()
				if owned, err := extendLease(t.Context(), store.db, owner.id, owner.ownerToken, now.UnixNano(), expiresNS); err != nil || !owned {
					t.Fatalf("baseline renewal: %t %v", owned, err)
				}
			}
			baseline := time.Since(start)
			if d.autocommitRenewals.Load() != int64(n) || d.renewals.Load() != int64(n) {
				t.Fatal("baseline did not execute one autocommit per owner")
			}
			recorder := runtimemetrics.New()
			ctx := runtimemetrics.WithRecorder(t.Context(), recorder)
			start = time.Now()
			for offset := 0; offset < n; offset += leaseHeartbeatBatchSize {
				if err := store.renewLeaseBatch(ctx, owners[offset:min(n, offset+leaseHeartbeatBatchSize)]); err != nil {
					t.Fatal(err)
				}
			}
			batched := time.Since(start)
			want := int64((n + leaseHeartbeatBatchSize - 1) / leaseHeartbeatBatchSize)
			if d.commits.Load() != want || d.renewals.Load() != 2*int64(n) || d.autocommitRenewals.Load() != int64(n) || recorder.Snapshot().Count(runtimemetrics.WriteTransactions) != uint64(want) {
				t.Fatalf("transactions/updates: commits=%d updates=%d autocommits=%d metrics=%+v", d.commits.Load(), d.renewals.Load(), d.autocommitRenewals.Load(), recorder.Snapshot())
			}
			var synchronous int
			if err := store.db.QueryRow("PRAGMA synchronous").Scan(&synchronous); err != nil || synchronous != 2 {
				t.Fatalf("FULL durability changed: %d %v", synchronous, err)
			}
			t.Logf("%d owners: old=%d autocommit transactions (%s), batch=%d FULL commits (%s), %d exact-token UPDATEs in either case", n, n, baseline, want, batched, n)
		})
	}
}

func TestHeartbeatBatchRollbackAndOwnerMismatch(t *testing.T) {
	overrideLeaseTiming(t, time.Hour, 2*time.Hour)
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	first := acquireNamed(t, store, "first").(*sqliteSession)
	second := acquireNamed(t, store, "second").(*sqliteSession)
	before := leaseExpiryNS(t, first)
	if _, err := store.db.Exec(fmt.Sprintf(`CREATE TEMP TRIGGER veto_renew BEFORE UPDATE ON session_leases WHEN old.session_id=x'%x' BEGIN SELECT RAISE(ABORT,'veto renewal'); END`, second.id)); err != nil {
		t.Fatal(err)
	}
	if err := store.renewLeaseBatch(t.Context(), []*sqliteSession{first, second}); err == nil {
		t.Fatal("batch succeeded after second UPDATE veto")
	}
	if got := leaseExpiryNS(t, first); got != before {
		t.Fatalf("partial batch escaped rollback: before=%d after=%d", before, got)
	}
	// An observed mismatch remains conclusive if a later peer UPDATE fails.
	if _, err := store.db.Exec(`UPDATE session_leases SET owner_token=zeroblob(16) WHERE session_id=?`, first.id); err != nil {
		t.Fatal(err)
	}
	if err := store.renewLeaseBatch(t.Context(), []*sqliteSession{first, second}); err == nil || !errors.Is(context.Cause(first.ctx), ErrSessionLeaseLost) || second.ctx.Err() != nil {
		t.Fatalf("failed batch lost mismatch or canceled peer: %v first=%v second=%v", err, context.Cause(first.ctx), second.ctx.Err())
	}
	if _, err := store.db.Exec(`DROP TRIGGER veto_renew`); err != nil {
		t.Fatal(err)
	}
	if err := store.renewLeaseBatch(t.Context(), []*sqliteSession{first, second}); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(context.Cause(first.ctx), ErrSessionLeaseLost) || second.ctx.Err() != nil {
		t.Fatalf("owner mismatch not isolated: first=%v second=%v", context.Cause(first.ctx), second.ctx.Err())
	}
	if got := leaseExpiryNS(t, first); got != before {
		t.Fatal("foreign owner lease was renewed")
	}
}

func TestHeartbeatBatchUncertainCommitAndCancellation(t *testing.T) {
	overrideLeaseTiming(t, time.Hour, 2*time.Hour)
	store, path := openTestStore(t, ModeDisk, nil, 0)
	owner := acquireNamed(t, store, "uncertain").(*sqliteSession)
	before := leaseExpiryNS(t, owner)
	d := maintenanceInjectDriver(t, store, path)
	d.failCommit.Store(true)
	if err := store.renewLeaseBatch(t.Context(), []*sqliteSession{owner}); !errors.Is(err, errMaintenanceInjected) {
		t.Fatalf("uncertain commit=%v", err)
	}
	if d.commits.Load() != 1 || d.renewals.Load() != 1 || owner.ctx.Err() != nil {
		t.Fatalf("uncertain batch replayed/canceled ownership: commits=%d updates=%d cause=%v", d.commits.Load(), d.renewals.Load(), context.Cause(owner.ctx))
	}
	if leaseExpiryNS(t, owner) <= before {
		t.Fatal("injected uncertain COMMIT did not commit")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := store.renewLeaseBatch(ctx, []*sqliteSession{owner}); !errors.Is(err, context.Canceled) || owner.ctx.Err() != nil {
		t.Fatalf("canceled renewal=%v cause=%v", err, context.Cause(owner.ctx))
	}
	if d.renewals.Load() != 1 {
		t.Fatal("canceled renewal executed an UPDATE")
	}
}

func TestHeartbeatOldSnapshotCannotRenewClosedLease(t *testing.T) {
	overrideLeaseTiming(t, time.Hour, 2*time.Hour)
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	owner := acquireNamed(t, store, "closing").(*sqliteSession)
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	// A batch copied before Close must neither resurrect the removed lease
	// nor steal a subsequent owner's replacement lease.
	if err := store.renewLeaseBatch(t.Context(), []*sqliteSession{owner}); err != nil {
		t.Fatal(err)
	}
	if maintenanceCount(t, store, "session_leases") != 0 {
		t.Fatal("closed lease resurrected")
	}
	replacement := acquireNamed(t, store, "closing").(*sqliteSession)
	before := leaseExpiryNS(t, replacement)
	if err := store.renewLeaseBatch(t.Context(), []*sqliteSession{owner}); err != nil {
		t.Fatal(err)
	}
	if leaseExpiryNS(t, replacement) != before {
		t.Fatal("old snapshot renewed replacement owner")
	}
}
