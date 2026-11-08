package sessions

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDeferredIntegrityCanceledLoopPreservesObservation(t *testing.T) {
	// A canceled check leaves both the stop signal and a pending ticker ready.
	// Repeat that scheduler state to exercise either select choice without
	// database work, sleeps, or depending on Close winning a timing window.
	for range 64 {
		ctx, cancel := context.WithCancelCause(t.Context())
		store := &SQLiteStore{ctx: ctx}
		store.closed.Store(true)
		cancel(ErrStoreClosed)
		before := MaintenanceStatus{
			LastIntegrityCheck: time.Unix(1, 0),
			LastIntegrityError: context.Canceled,
			IntegrityInterval:  time.Nanosecond,
		}
		store.maintenanceStatus = before
		store.wg.Add(1)
		store.integrityLoop(time.Nanosecond)
		store.wg.Wait()
		after, err := store.MaintenanceStatus(t.Context())
		if err != nil || after != before {
			t.Fatalf("canceled scheduler changed last observation: before=%+v after=%+v err=%v", before, after, err)
		}
		// An explicit call is different: it must still report and record the
		// closed handle rather than silently preserve the old observation.
		if err := store.CheckIntegrity(t.Context()); !errors.Is(err, ErrStoreClosed) {
			t.Fatalf("explicit check after Close=%v", err)
		}
		after, err = store.MaintenanceStatus(t.Context())
		if err != nil || !errors.Is(after.LastIntegrityError, ErrStoreClosed) || after.Corrupt {
			t.Fatalf("explicit closed check status=%+v err=%v", after, err)
		}
	}
}
