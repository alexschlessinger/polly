package sessions

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
)

// statementDB opens a database on one connection that keeps its statements.
func statementDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "statements.db")
	db := sql.OpenDB(&statementConnector{dsn: sqliteDataSource(ModeDisk, path)})
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec("CREATE TABLE t (n INTEGER PRIMARY KEY, text TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	return db, path
}

// kept reports how many statements db's connection has prepared to keep, and
// how many it keeps.
func kept(t *testing.T, db *sql.DB) (prepared, held int) {
	t.Helper()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.Raw(func(driver any) error {
		c := driver.(*statementConn)
		prepared, held = c.prepared, len(c.statements)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return prepared, held
}

func numbers(t *testing.T, db *sql.DB, query string, args ...any) []int {
	t.Helper()
	rows, err := db.Query(query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []int
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		got = append(got, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return got
}

// A statement is prepared the first time its text is run on a connection and
// run again from then on, with the arguments of each run and no others.
func TestConnectionKeepsTheStatementsItPrepared(t *testing.T) {
	db, _ := statementDB(t)
	before, _ := kept(t, db)
	for n, text := range []string{"one", "two", "three"} {
		if _, err := db.Exec("INSERT INTO t (n, text) VALUES (?, ?)", n+1, text); err != nil {
			t.Fatal(err)
		}
	}
	for n, want := range []string{"one", "two", "three"} {
		var text string
		if err := db.QueryRow("SELECT text FROM t WHERE n = ?", n+1).Scan(&text); err != nil || text != want {
			t.Fatalf("row %d = %q, %v", n+1, text, err)
		}
	}
	if after, _ := kept(t, db); after-before != 2 {
		t.Fatalf("six runs of two statements prepared %d", after-before)
	}
	// A run that fails leaves the statement fit to run again.
	if _, err := db.Exec("INSERT INTO t (n, text) VALUES (?, ?)", 1, "again"); err == nil {
		t.Fatal("a second row with the same key was stored")
	}
	if _, err := db.Exec("INSERT INTO t (n, text) VALUES (?, ?)", 4, "four"); err != nil {
		t.Fatal(err)
	}
	if got := numbers(t, db, "SELECT n FROM t ORDER BY n"); fmt.Sprint(got) != "[1 2 3 4]" {
		t.Fatalf("rows = %v", got)
	}
	if _, err := db.Exec("INSERT INTO nowhere VALUES (1)"); err == nil {
		t.Fatal("a statement that cannot be prepared ran")
	}
}

// Rows being read have their statement to themselves: the same text run
// meanwhile, on the same connection, neither disturbs them nor waits.
func TestStatementBeingReadIsNotRunAgainUnderItsRows(t *testing.T) {
	ctx := context.Background()
	db, _ := statementDB(t)
	for n := 1; n <= 3; n++ {
		if _, err := db.Exec("INSERT INTO t (n, text) VALUES (?, ?)", n, "row"); err != nil {
			t.Fatal(err)
		}
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	const query = "SELECT n FROM t WHERE n >= ? ORDER BY n"
	outer, err := conn.QueryContext(ctx, query, 1)
	if err != nil {
		t.Fatal(err)
	}
	// Before the connection, which waits for its rows.
	defer outer.Close()
	var seen, inner []int
	for outer.Next() {
		var n int
		if err := outer.Scan(&n); err != nil {
			t.Fatal(err)
		}
		if seen = append(seen, n); len(seen) > 3 {
			t.Fatalf("the rows being read started over: %v", seen)
		}
		rows, err := conn.QueryContext(ctx, query, 3)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var m int
			if err := rows.Scan(&m); err != nil {
				t.Fatal(err)
			}
			inner = append(inner, m)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := outer.Close(); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(seen) != "[1 2 3]" || fmt.Sprint(inner) != "[3 3 3]" {
		t.Fatalf("outer rows %v, inner rows %v", seen, inner)
	}
	// Closed, the rows give their statement back.
	var n int
	if err := conn.QueryRowContext(ctx, query, 2).Scan(&n); err != nil || n != 2 {
		t.Fatalf("after its rows closed the statement gave %d, %v", n, err)
	}
}

// A connection keeps a bounded number of statements, and closes them all
// with itself: SQLite then closes the database, and takes its log away.
func TestConnectionBoundsAndClosesItsStatements(t *testing.T) {
	db, path := statementDB(t)
	if _, err := db.Exec("PRAGMA journal_mode = WAL"); err != nil {
		t.Fatal(err)
	}
	for n := range maxCachedStatements + 20 {
		if _, err := db.Exec(fmt.Sprintf("INSERT INTO t (n, text) VALUES (%d, 'row')", n)); err != nil {
			t.Fatal(err)
		}
	}
	if _, held := kept(t, db); held > maxCachedStatements {
		t.Fatalf("the connection keeps %d statements", held)
	}
	if got := numbers(t, db, "SELECT count(*) FROM t"); got[0] != maxCachedStatements+20 {
		t.Fatalf("%d rows stored", got[0])
	}
	if _, err := os.Stat(path + "-wal"); err != nil {
		t.Fatalf("no write-ahead log while the database is open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + "-wal"); !os.IsNotExist(err) {
		t.Fatalf("the write-ahead log outlived the connection: %v", err)
	}
}

// The store opens its database on such a connection.
func TestStoreKeepsItsStatements(t *testing.T) {
	ctx := context.Background()
	store, _ := openTestStore(t, ModeDisk, nil, 0)
	session := acquireNamed(t, store, "kept")
	if err := session.AddMessage(ctx, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "one"}); err != nil {
		t.Fatal(err)
	}
	before, _ := kept(t, store.db)
	if before == 0 {
		t.Fatal("the store's connection has kept no statement")
	}
	for range 5 {
		if err := session.AddMessage(ctx, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "more"}); err != nil {
			t.Fatal(err)
		}
	}
	if after, _ := kept(t, store.db); after != before {
		t.Fatalf("five more of the same write prepared %d statements", after-before)
	}
}
