package sessions

import (
	"context"
	"database/sql"
	"fmt"
)

// SQLite cannot extend a CHECK constraint in place. Rebuild only the report
// table inside migrateSchema's transaction, preserving IDs used by pending
// delivery receipts and both session foreign keys.
func applySchemaV6(ctx context.Context, conn *sql.Conn) error {
	for _, statement := range []string{
		`CREATE TABLE session_reports_v6 (
			id INTEGER PRIMARY KEY,
			session_id BLOB NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
			child_id BLOB REFERENCES sessions(id) ON DELETE SET NULL,
			child TEXT NOT NULL,
			status TEXT NOT NULL CHECK(status IN ('finished','canceled','failed','paused')),
			body TEXT NOT NULL,
			error TEXT NOT NULL DEFAULT '',
			input_tokens INTEGER NOT NULL DEFAULT 0 CHECK(input_tokens >= 0),
			output_tokens INTEGER NOT NULL DEFAULT 0 CHECK(output_tokens >= 0),
			posted_ns INTEGER NOT NULL
		) STRICT`,
		`INSERT INTO session_reports_v6 (id, session_id, child_id, child, status, body, error, input_tokens, output_tokens, posted_ns)
		 SELECT id, session_id, child_id, child, status, body, error, input_tokens, output_tokens, posted_ns FROM session_reports`,
		`DROP TABLE session_reports`,
		`ALTER TABLE session_reports_v6 RENAME TO session_reports`,
		`CREATE INDEX session_reports_session_idx ON session_reports(session_id, id)`,
		`PRAGMA user_version = 6`,
	} {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("apply session schema v6: %w", err)
		}
	}
	return nil
}
