package sessions

import (
	"context"
	"database/sql"
	"fmt"
)

var displayTables = map[string]schemaTableSpec{
	"swarm_display_marks":     {columns: []schemaColumnSpec{{"parent_id", "BLOB", 1, 1, ""}, {"stamp", "INTEGER", 1, 2, ""}, {"nonce", "TEXT", 1, 0, ""}}, withoutRowID: 1, requiredSQL: []string{"check(stamp>=0)"}},
	"swarm_display_sources":   {columns: []schemaColumnSpec{{"parent_id", "BLOB", 1, 1, ""}, {"kind", "TEXT", 1, 2, ""}, {"id", "TEXT", 1, 3, ""}, {"subject", "TEXT", 1, 0, ""}, {"sort_key", "BLOB", 1, 0, ""}, {"value", "BLOB", 1, 0, ""}}, withoutRowID: 1},
	"swarm_display_facts":     {columns: []schemaColumnSpec{{"parent_id", "BLOB", 1, 1, ""}, {"kind", "TEXT", 1, 2, ""}, {"id", "TEXT", 1, 3, ""}, {"stamp", "INTEGER", 1, 0, ""}, {"value", "BLOB", 0, 0, ""}}, withoutRowID: 1, requiredSQL: []string{"check(stamp>=0)"}},
	"swarm_display_heads":     {columns: []schemaColumnSpec{{"parent_id", "BLOB", 1, 1, ""}, {"stamp", "INTEGER", 1, 0, ""}}, withoutRowID: 1, requiredSQL: []string{"check(stamp>=0)"}},
	"swarm_display_decisions": {columns: []schemaColumnSpec{{"parent_id", "BLOB", 1, 1, ""}, {"kind", "TEXT", 1, 2, ""}, {"id", "TEXT", 1, 3, ""}, {"member", "TEXT", 1, 0, ""}, {"workflow", "TEXT", 1, 0, ""}, {"sort_key", "BLOB", 1, 0, ""}, {"value", "BLOB", 1, 0, ""}}, withoutRowID: 1},
}

func validateDisplaySchema(ctx context.Context, conn *sql.Conn) error {
	for table, spec := range displayTables {
		if err := validateSchemaTable(ctx, conn, table, spec); err != nil {
			return fmt.Errorf("session database table %s: %w", table, err)
		}
		if err := validateForeignKeys(ctx, conn, table, map[string]bool{"parent_id>sessions.id:CASCADE": true}); err != nil {
			return err
		}
	}
	return nil
}
