package sessions

import (
	"context"
	"time"
)

// MaxMaintenanceLimit bounds the candidates inspected by one Maintain call.
const MaxMaintenanceLimit = 1000

// MaintenanceOptions bounds expiry and unreferenced-artifact collection, not
// integrity validation. Limit is required and must be in [1, MaxMaintenanceLimit];
// invalid limits return ErrInvalidPageRequest without performing maintenance.
// A candidate's atomic deletion/cascade may itself require substantial work.
// Retained sessions and referenced blobs count as examined candidates too.
// SQLite advances separate expiry/blob keysets between calls and alternates
// their budgets to avoid starvation; new or changed rows behind a cursor are
// reconsidered after it wraps. Expire and default startup remain expiry-only.
type MaintenanceOptions struct{ Limit int }

// MaintenanceResult describes committed work only on nil error. An error may
// represent an uncertain commit; callers must inspect durable state rather than
// assuming nothing was deleted. More is a hint that another bounded call may
// find candidates, not a promise that a concurrent writer cannot create more.
type MaintenanceResult struct {
	Examined, SessionsDeleted, ArtifactsDeleted int
	More                                        bool
}

// MaintenanceStatus is a snapshot of maintenance observations, not lease or
// write authority. Zero timestamps mean no completed attempt in this store's
// observation lifetime. Error fields retain the latest attempt's error (nil on
// success); implementations preserve errors.Is identity where applicable.
// Corrupt means confirmed corruption and requires subsequent writes to fail
// closed. Cancellation or a transient I/O error alone must not mark corruption.
type MaintenanceStatus struct {
	LastMaintenance, LastIntegrityCheck      time.Time
	LastMaintenanceError, LastIntegrityError error
	// IntegrityInterval is positive when the host explicitly opts into
	// deferred current-schema integrity checks. Zero means no such opt-in.
	IntegrityInterval time.Duration
	Corrupt           bool
}

// MaintenanceStore is an optional capability, not a SessionStore requirement.
// There is deliberately no compatibility adapter: Expire cannot establish a
// bounded scan or integrity validation, and a success-returning stub is unsafe.
// Implementations advertise this interface only when all methods are functional.
//
// Maintain preserves live-lease and swarm-pin retention and inspects at most
// Limit expiry/blob candidates. CheckIntegrity explicitly performs potentially
// whole-store validation; its failures and scheduled-check failures must be
// observable through MaintenanceStatus. All operations honor cancellation.
//
// Deferred integrity is opt-in and requires a positive scheduled interval,
// explicit CheckIntegrity and fail-closed writes after confirmed corruption.
// Default CLI/zero-value StoreConfig startup checks and mandatory pre-migration
// validation remain unchanged. Merely defining or selecting this capability
// never opts a host into deferred checks or starts background work.
type MaintenanceStore interface {
	Maintain(context.Context, MaintenanceOptions) (MaintenanceResult, error)
	CheckIntegrity(context.Context) error
	MaintenanceStatus(context.Context) (MaintenanceStatus, error)
}
