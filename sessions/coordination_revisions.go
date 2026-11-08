package sessions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// validateBeforeV10 validates the genuine v9 shape and relational/transcript
// integrity before backfill. Quick-check is additionally mandatory before any
// migration.
func validateBeforeV10(ctx context.Context, conn *sql.Conn) error {
	var newColumns, newTables int
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('sessions') WHERE name IN ('transcript_epoch','coordination_stamp','coordination_format')`).Scan(&newColumns); err != nil {
		return err
	}
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('swarm_relations','swarm_revisions','swarm_semantic_heads','swarm_semantic_history','swarm_status_facts','swarm_status_totals','swarm_status_items','swarm_display_sources','swarm_display_facts','swarm_display_heads','swarm_display_decisions','swarm_display_marks','transcript_display')`).Scan(&newTables); err != nil {
		return err
	}
	if newColumns != 0 || newTables != 0 {
		return errors.New("native schema 10 shape present before migration")
	}
	for table, spec := range schemaTables {
		switch table {
		case "swarm_relations", "swarm_revisions", "swarm_semantic_heads", "swarm_semantic_history", "swarm_status_facts", "swarm_status_totals", "swarm_status_items", "transcript_display":
			continue
		}
		if table == "sessions" {
			spec.columns = spec.columns[:11]
			spec.requiredSQL = spec.requiredSQL[:6]
		}
		if err := validateSchemaTable(ctx, conn, table, spec); err != nil {
			return fmt.Errorf("pre-migration table %s: %w", table, err)
		}
	}
	for index, columns := range map[string][]string{"sessions_parent_idx": {"parent_id"}, "sessions_updated_idx": {"updated_ns", "name"}, "sessions_expiry_idx": {"ttl_ns", "updated_ns"}, "session_artifacts_digest_idx": {"digest"}, "swarm_artifacts_digest_idx": {"digest"}} {
		if err := requireIndexColumns(ctx, conn, index, columns); err != nil {
			return err
		}
	}
	var violation [4]any
	err := conn.QueryRowContext(ctx, `PRAGMA foreign_key_check`).Scan(&violation[0], &violation[1], &violation[2], &violation[3])
	if err == nil {
		return errors.New("pre-migration database contains foreign-key violations")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var bad []byte
	err = conn.QueryRowContext(ctx, `SELECT s.id FROM sessions s LEFT JOIN messages m ON m.session_id=s.id GROUP BY s.id HAVING count(m.sequence)<>s.next_sequence OR (count(m.sequence)>0 AND (min(m.sequence)<>0 OR max(m.sequence)<>s.next_sequence-1)) LIMIT 1`).Scan(&bad)
	if err == nil {
		return errors.New("pre-migration session message sequence is corrupt")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return nil
}

// validateNativeIndexes checks the composite relation foreign key that
// index_info cannot establish. Index definitions are checked by
// validateIndexDefinitions.
func validateNativeIndexes(ctx context.Context, conn *sql.Conn) error {
	var composite bool
	if err := conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pragma_foreign_key_list('swarm_relations') GROUP BY id HAVING count(*)=3 AND group_concat("from",',')='parent_id,kind,id')`).Scan(&composite); err != nil {
		return err
	}
	if !composite {
		return schemaCorruption("session database swarm_relations lacks composite record foreign key")
	}
	return nil
}
