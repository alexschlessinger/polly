package sessions_test

import (
	"context"
	"errors"
	"testing"

	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
	"github.com/alexschlessinger/pollytool/internal/sessiontest"
	"github.com/alexschlessinger/pollytool/sessions"
)

func TestRuntimeStorageScaleNativeObservations(t *testing.T) {
	t.Parallel()
	store := sessiontest.SQLite(t).Open()
	fixture := sessiontest.SeedFamily(t, store, 2, 32)
	coord := fixture.Parent.(sessions.CoordinationSession)
	raw, err := coord.ReadCoordination(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	records, bytes := sessiontest.RecordSize(raw)
	r := runtimemetrics.New()
	ctx := runtimemetrics.WithRecorder(t.Context(), r)
	if _, err := coord.ReadCoordination(ctx); err != nil {
		t.Fatal(err)
	}
	veto := errors.New("veto")
	if err := coord.UpdateCoordination(ctx, func(*sessions.CoordinationState) error { return veto }); !errors.Is(err, veto) {
		t.Fatalf("veto: %v", err)
	}
	if err := coord.UpdateCoordination(ctx, func(*sessions.CoordinationState) error { return nil }); err != nil {
		t.Fatal(err)
	}
	s := r.Snapshot()
	if s.Count(runtimemetrics.ReadTransactions) != 1 || s.Count(runtimemetrics.WriteTransactions) != 2 || s.Timing(runtimemetrics.ReadTransaction).Count != 1 || s.Timing(runtimemetrics.WriteTransaction).Count != 2 {
		t.Fatalf("transaction attempts/timers: %+v", s)
	}
	if s.Count(runtimemetrics.SQLStatements) != 3 || s.Count(runtimemetrics.RowsRead) != 3*records || s.Count(runtimemetrics.PayloadBytesRead) != 3*bytes {
		t.Fatalf("covered legacy SELECT observations: %+v", s)
	}
	if s.Count(runtimemetrics.CoordinationRecordsRead) != 0 {
		t.Fatal("native SQL seam filled adapter materialization counter")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := coord.ReadCoordination(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read: %v", err)
	}
	if err := coord.UpdateCoordination(canceled, func(*sessions.CoordinationState) error { t.Fatal("canceled callback invoked"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled write: %v", err)
	}
	// Pre-canceled leased operations never reach a transaction or SELECT.
	if r.Snapshot() != s {
		t.Fatal("pre-canceled operation claimed transaction/SQL work")
	}
	disabled := runtimemetrics.WithRecorder(ctx, nil)
	if _, err := coord.ReadCoordination(disabled); err != nil {
		t.Fatal(err)
	}
	if err := coord.UpdateCoordination(disabled, func(*sessions.CoordinationState) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if r.Snapshot() != s {
		t.Fatal("disabled native operations reached recorder")
	}
	t.Logf("covered native legacy SELECTs=%d rows=%d payload_bytes=%d; transaction attempts include veto/rollback, not total SQL or successful commits", s.Count(runtimemetrics.SQLStatements), s.Count(runtimemetrics.RowsRead), s.Count(runtimemetrics.PayloadBytesRead))
}
