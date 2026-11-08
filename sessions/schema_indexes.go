package sessions

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// schemaIndex is one index in the current schema. Its definition follows
// "ON ..." and is the single source for both creation and validation.
type schemaIndex struct {
	name       string
	definition string
}

// schemaIndexes is the complete current-schema index set, including indexes
// introduced by earlier migrations. Schema 10 re-asserts all of them, so a
// current-schema store cannot exist without one and validation never consults a
// second list.
var schemaIndexes = []schemaIndex{
	{"sessions_updated_idx", "ON sessions(updated_ns DESC, name)"},
	{"sessions_expiry_idx", "ON sessions(ttl_ns, updated_ns)"},
	{"sessions_parent_idx", "ON sessions(parent_id)"},
	{"session_artifacts_digest_idx", "ON session_artifacts(digest)"},
	{"swarm_artifacts_digest_idx", "ON swarm_artifacts(digest)"},
	{"sessions_deadline_idx", "ON sessions(" + expiryKeySQL + ",id) WHERE ttl_ns > 0"},
	{"sessions_summary_idx", "ON sessions(updated_ns DESC,name,id)"},
	{"sessions_parent_summary_idx", "ON sessions(parent_id,updated_ns DESC,name,id)"},
	{"sessions_spawn_idx", "ON sessions(parent_id,json_extract(settings_json,'$.spawnCallID'))"},
	{"swarm_status_items_source_idx", "ON swarm_status_items(parent_id,kind,id)"},
	{"swarm_semantic_heads_changed_idx", "ON swarm_semantic_heads(parent_id,stamp,kind,id)"},
	// swarm_members' primary key leads with the parent, so pinning checks and
	// the cascade from a deleted session otherwise scan the table.
	{"swarm_members_member_idx", "ON swarm_members(member_id)"},
	{"swarm_relations_record_idx", "ON swarm_relations(parent_id,kind,id)"},
	{"swarm_relations_kind_idx", "ON swarm_relations(parent_id,index_name,index_key,kind,sort_key)"},
	{"swarm_display_sources_subject_idx", "ON swarm_display_sources(parent_id,kind,subject,sort_key)"},
	{"swarm_display_facts_changed_idx", "ON swarm_display_facts(parent_id,stamp,kind,id)"},
	{"swarm_display_facts_present_idx", "ON swarm_display_facts(parent_id,kind,id) WHERE value IS NOT NULL"},
	{"swarm_display_decisions_member_idx", "ON swarm_display_decisions(parent_id,member,sort_key)"},
	{"swarm_display_decisions_workflow_idx", "ON swarm_display_decisions(parent_id,workflow)"},
	{"transcript_display_users_idx", "ON transcript_display(session_id,real_user,sequence)"},
	{"transcript_display_facts_idx", "ON transcript_display(session_id,relevant,sequence)"},
}

// allSchemaIndexes adds the per-selector candidate indexes, whose names and
// definitions come from candidateSourceIndexes.
func allSchemaIndexes() []schemaIndex {
	out := append([]schemaIndex(nil), schemaIndexes...)
	for _, index := range candidateSourceIndexes {
		out = append(out, schemaIndex{index.indexName(), index.definition()})
	}
	return out
}

// createSchemaV10Indexes creates every index schema 10 owns. They are part of
// the version bump: a current-schema store missing one is corrupt and refused,
// not silently repaired on open.
func createSchemaV10Indexes(ctx context.Context, conn *sql.Conn) error {
	for _, index := range allSchemaIndexes() {
		if _, err := conn.ExecContext(ctx, "CREATE INDEX IF NOT EXISTS "+index.name+" "+index.definition); err != nil {
			return err
		}
	}
	return nil
}

// validateIndexDefinitions checks the exact definition of every index,
// including expressions, collations, direction and partial predicates.
func validateIndexDefinitions(ctx context.Context, conn *sql.Conn) error {
	for _, index := range allSchemaIndexes() {
		var statement string
		if err := conn.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='index' AND name=?`, index.name).Scan(&statement); err != nil {
			return fmt.Errorf("session database index %s: %w", index.name, schemaObjectError(err))
		}
		compact := compactSchemaSQL(statement)
		if !strings.HasPrefix(compact, "createindex") || !strings.HasSuffix(compact, compactSchemaSQL(index.definition)) {
			return schemaCorruption("session database index %s definition differs", index.name)
		}
	}
	return nil
}
