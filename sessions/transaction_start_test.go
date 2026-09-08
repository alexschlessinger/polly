package sessions

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
)

var transactionDriverIDs atomic.Int64

type uncertainBeginDriver struct {
	base driver.Driver
	fail atomic.Bool
}

func (d *uncertainBeginDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.base.Open(name)
	if err != nil {
		return nil, err
	}
	return &uncertainBeginConn{Conn: conn, owner: d}, nil
}

type uncertainBeginConn struct {
	driver.Conn
	owner *uncertainBeginDriver
}

func (c *uncertainBeginConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	execer, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	result, err := execer.ExecContext(ctx, query, args)
	if err == nil && strings.HasPrefix(query, "BEGIN") && c.owner.fail.Swap(false) {
		// Model the driver's cancellation window after SQLite executes BEGIN
		// but before database/sql receives its successful result.
		return nil, context.Canceled
	}
	return result, err
}

func TestUncertainTransactionStartRollsBackBeforeConnectionReuse(t *testing.T) {
	real, _ := openTestStore(t, ModeMemory, nil, 0)
	for _, write := range []bool{false, true} {
		t.Run(fmt.Sprint("write=", write), func(t *testing.T) {
			injected := &uncertainBeginDriver{base: real.db.Driver()}
			name := fmt.Sprintf("uncertain-begin-%d", transactionDriverIDs.Add(1))
			sql.Register(name, injected)
			db, err := sql.Open(name, ":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			store := &SQLiteStore{db: db}
			transaction := store.withRead
			if write {
				transaction = store.withWrite
			}
			injected.fail.Store(true)
			called := false
			err = transaction(context.Background(), func(*sql.Conn) error { called = true; return nil })
			if !errors.Is(err, context.Canceled) || called {
				t.Fatalf("uncertain begin: %v callback=%v", err, called)
			}
			if err := transaction(context.Background(), func(conn *sql.Conn) error { _, err := conn.ExecContext(context.Background(), "SELECT 1"); return err }); err != nil {
				t.Fatalf("connection retained the canceled transaction: %v", err)
			}
		})
	}
}
