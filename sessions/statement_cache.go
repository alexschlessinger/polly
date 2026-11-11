package sessions

import (
	"context"
	"database/sql/driver"
	"errors"

	"modernc.org/sqlite"
)

// database/sql hands a statement's text to the driver every time it is run
// on a connection, and the driver prepares it, runs it and throws the
// prepared statement away. The store runs the same few dozen statements over
// and over on its one connection, so its connection keeps each statement it
// has prepared and runs that the next time the same text comes.

// maxCachedStatements bounds the statements a connection keeps. The store's
// statements are few; the bound is for texts built from their arguments.
const maxCachedStatements = 256

// sqliteConn is what the store needs of the driver's connection.
type sqliteConn interface {
	driver.Conn
	driver.ConnPrepareContext
	driver.ConnBeginTx
	driver.ExecerContext
	driver.QueryerContext
	driver.Pinger
	driver.SessionResetter
	driver.Validator
}

// statementConnector opens connections that keep their statements.
type statementConnector struct {
	dsn    string
	driver sqlite.Driver
}

func (c *statementConnector) Connect(context.Context) (driver.Conn, error) { return c.Open(c.dsn) }

func (c *statementConnector) Driver() driver.Driver { return c }

func (c *statementConnector) Open(dsn string) (driver.Conn, error) {
	conn, err := c.driver.Open(dsn)
	if err != nil {
		return nil, err
	}
	inner, ok := conn.(sqliteConn)
	if !ok {
		return nil, errors.Join(errors.New("sqlite connection lacks a context interface"), conn.Close())
	}
	return &statementConn{sqliteConn: inner, statements: map[string]*keptStatement{}}, nil
}

// statementConn is a driver connection that keeps the statements it has
// prepared. database/sql calls a driver connection, and the rows it handed
// out, from one goroutine at a time, which is what guards the fields here.
type statementConn struct {
	sqliteConn
	statements map[string]*keptStatement
	// prepared counts the statements the connection has prepared to keep.
	prepared int
}

type keptStatement struct {
	stmt  driver.Stmt
	exec  driver.StmtExecContext
	query driver.StmtQueryContext
	// open is set while rows of the statement are being read. Running the
	// statement again would start it over under them, so until they are
	// closed the same text is prepared afresh for whoever else runs it.
	open bool
}

// statement returns the kept statement for query, preparing it at its first
// use, or nil when the text is to be run without one: its statement is being
// read, the connection keeps as many as it may and all are being read, or
// the driver's statement cannot run with a context.
func (c *statementConn) statement(ctx context.Context, query string) (*keptStatement, error) {
	if kept := c.statements[query]; kept != nil {
		if kept.open {
			return nil, nil
		}
		return kept, nil
	}
	if len(c.statements) >= maxCachedStatements && !c.dropOne() {
		return nil, nil
	}
	stmt, err := c.sqliteConn.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	exec, canExec := stmt.(driver.StmtExecContext)
	rows, canQuery := stmt.(driver.StmtQueryContext)
	if !canExec || !canQuery {
		return nil, stmt.Close()
	}
	c.prepared++
	kept := &keptStatement{stmt: stmt, exec: exec, query: rows}
	c.statements[query] = kept
	return kept, nil
}

// dropOne closes one kept statement that is not being read, and reports
// whether there was one.
func (c *statementConn) dropOne() bool {
	for query, kept := range c.statements {
		if !kept.open {
			delete(c.statements, query)
			_ = kept.stmt.Close()
			return true
		}
	}
	return false
}

func (c *statementConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	kept, err := c.statement(ctx, query)
	if err != nil {
		return nil, err
	}
	if kept == nil {
		return c.sqliteConn.ExecContext(ctx, query, args)
	}
	return kept.exec.ExecContext(ctx, args)
}

func (c *statementConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	kept, err := c.statement(ctx, query)
	if err != nil {
		return nil, err
	}
	if kept == nil {
		return c.sqliteConn.QueryContext(ctx, query, args)
	}
	rows, err := kept.query.QueryContext(ctx, args)
	if err != nil {
		return nil, err
	}
	kept.open = true
	return &keptRows{Rows: rows, statement: kept}, nil
}

// Close closes the kept statements before the connection: SQLite keeps a
// connection with statements left on it open.
func (c *statementConn) Close() error {
	var err error
	for query, kept := range c.statements {
		delete(c.statements, query)
		err = errors.Join(err, kept.stmt.Close())
	}
	return errors.Join(err, c.sqliteConn.Close())
}

// keptRows are the rows of a kept statement. Closing them resets the
// statement, which is then free to run again.
type keptRows struct {
	driver.Rows
	statement *keptStatement
}

func (r *keptRows) Close() error {
	if r.statement == nil {
		return nil
	}
	err := r.Rows.Close()
	r.statement.open = false
	r.statement = nil
	return err
}
