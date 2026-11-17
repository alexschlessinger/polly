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
			store := &SQLiteStore{db: db, turn: make(chan struct{}, 1)}
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

// endedCommitDriver models a caller's context that ends while SQLite executes
// COMMIT: the statement completes, and the driver reports the context's
// error for it all the same.
type endedCommitDriver struct {
	base driver.Driver
	end  atomic.Pointer[context.CancelFunc]
}

func (d *endedCommitDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.base.Open(name)
	if err != nil {
		return nil, err
	}
	return &endedCommitConn{Conn: conn, owner: d}, nil
}

type endedCommitConn struct {
	driver.Conn
	owner *endedCommitDriver
}

func (c *endedCommitConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	execer, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	if query != "COMMIT" {
		return execer.ExecContext(ctx, query, args)
	}
	result, err := execer.ExecContext(context.WithoutCancel(ctx), query, args)
	if end := c.owner.end.Swap(nil); end != nil {
		(*end)()
	}
	if err == nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return result, err
}

// A transaction that returns an error did not commit, and one that committed
// returns none, whenever its caller's context ends: before the commit the
// transaction is rolled back, and a commit under way runs to its end.
func TestTransactionReportsWhetherItCommitted(t *testing.T) {
	real, _ := openTestStore(t, ModeMemory, nil, 0)
	injected := &endedCommitDriver{base: real.db.Driver()}
	name := fmt.Sprintf("ended-commit-%d", transactionDriverIDs.Add(1))
	sql.Register(name, injected)
	db, err := sql.Open(name, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("CREATE TABLE written(n INTEGER)"); err != nil {
		t.Fatal(err)
	}
	store := &SQLiteStore{db: db, turn: make(chan struct{}, 1)}
	stored := func() (count int) {
		t.Helper()
		if err := db.QueryRow("SELECT count(*) FROM written").Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	for _, during := range []bool{true, false} {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		before := stored()
		if during {
			injected.end.Store(&cancel)
		}
		err := store.withWrite(ctx, func(conn *sql.Conn) error {
			_, err := conn.ExecContext(ctx, "INSERT INTO written VALUES(1)")
			if !during {
				cancel()
			}
			return err
		})
		if ctx.Err() == nil {
			t.Fatalf("during commit = %v: the caller's context did not end", during)
		}
		if wrote := stored() - before; (err == nil) != (wrote == 1) || during != (wrote == 1) {
			t.Fatalf("during commit = %v: the transaction returned %v and stored %d rows", during, err, wrote)
		}
	}
}
