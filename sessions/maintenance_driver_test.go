package sessions

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var errMaintenanceInjected = errors.New("injected transient maintenance failure")

// A real SQLite driver with deterministic fault windows. No statements are
// mocked: the uncertain-commit case actually commits before reporting failure.
type maintenanceFaultDriver struct {
	base               driver.Driver
	failCommit         atomic.Bool
	failCheck          atomic.Bool
	failSchema         atomic.Bool
	commits            atomic.Int64
	renewals           atomic.Int64
	autocommitRenewals atomic.Int64
	checkEntered       chan struct{}
	checkRelease       chan struct{}
	pauseCheck         atomic.Bool
}

func (d *maintenanceFaultDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.base.Open(name)
	if err != nil {
		return nil, err
	}
	return &maintenanceFaultConn{Conn: conn, owner: d}, nil
}

type maintenanceFaultConn struct {
	driver.Conn
	owner         *maintenanceFaultDriver
	inTransaction bool
}

func (c *maintenanceFaultConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	result, err := c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
	if err == nil {
		if strings.HasPrefix(query, "BEGIN") {
			c.inTransaction = true
		} else if query == "COMMIT" || query == "ROLLBACK" {
			c.inTransaction = false
		}
	}
	if strings.TrimSpace(query) == "COMMIT" {
		c.owner.commits.Add(1)
		if err == nil && c.owner.failCommit.Swap(false) {
			return nil, errMaintenanceInjected
		}
	}
	if strings.HasPrefix(strings.TrimSpace(query), "UPDATE session_leases") {
		c.owner.renewals.Add(1)
		if !c.inTransaction {
			c.owner.autocommitRenewals.Add(1)
		}
	}
	return result, err
}

func (c *maintenanceFaultConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if strings.HasPrefix(query, "PRAGMA table_info") && c.owner.failSchema.Swap(false) {
		return nil, errMaintenanceInjected
	}
	if query == "PRAGMA quick_check" {
		if c.owner.pauseCheck.Swap(false) {
			close(c.owner.checkEntered)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-c.owner.checkRelease:
			}
		}
		if c.owner.failCheck.Swap(false) {
			return nil, errMaintenanceInjected
		}
	}
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func maintenanceInjectDriver(t *testing.T, store *SQLiteStore, path string) *maintenanceFaultDriver {
	t.Helper()
	store.dbMu.Lock()
	defer store.dbMu.Unlock()
	d := &maintenanceFaultDriver{base: store.db.Driver(), checkEntered: make(chan struct{}), checkRelease: make(chan struct{})}
	name := fmt.Sprintf("maintenance-fault-%d", transactionDriverIDs.Add(1))
	sql.Register(name, d)
	db, err := sql.Open(name, sqliteDataSource(ModeDisk, path))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if err := store.db.Close(); err != nil {
		t.Fatal(err)
	}
	store.db = db
	return d
}

func maintenanceWait(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for observed maintenance state")
		}
		runtime.Gosched()
	}
}

func TestMaintenanceUncertainCommitDoesNotReplay(t *testing.T) {
	store, path := openTestStore(t, ModeDisk, nil, 0)
	maintenanceSeedSessions(t, store, 4, true)
	d := maintenanceInjectDriver(t, store, path)
	d.failCommit.Store(true)
	result, err := store.Maintain(t.Context(), MaintenanceOptions{Limit: 2})
	if !errors.Is(err, errMaintenanceInjected) || result != (MaintenanceResult{}) {
		t.Fatalf("uncertain result=%+v %v", result, err)
	}
	if got := d.commits.Load(); got != 1 {
		t.Fatalf("uncertain COMMIT replayed: commits=%d", got)
	}
	if got := maintenanceCount(t, store, "sessions"); got != 3 {
		t.Fatalf("durable state not committed once: sessions=%d", got)
	}
	status, _ := store.MaintenanceStatus(t.Context())
	if !errors.Is(status.LastMaintenanceError, errMaintenanceInjected) || status.Corrupt {
		t.Fatalf("status=%+v", status)
	}
	if store.maintenancePosition.expiry.id != nil {
		t.Fatal("uncertain commit advanced cursor")
	}
}
