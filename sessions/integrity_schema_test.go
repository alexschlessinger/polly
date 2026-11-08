package sessions

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestIntegrityStructuralViolationsFenceWrites(t *testing.T) {
	for name, query := range map[string]string{
		"unique native index":          `DROP INDEX sessions_spawn_idx; CREATE UNIQUE INDEX sessions_spawn_idx ON sessions(parent_id,json_extract(settings_json,'$.spawnCallID'))`,
		"version":                      `PRAGMA user_version=999`,
		"missing table":                `DROP TABLE model_metadata_cache`,
		"missing expiry index":         `DROP INDEX sessions_deadline_idx`,
		"changed expiry index":         `DROP INDEX sessions_deadline_idx; CREATE INDEX sessions_deadline_idx ON sessions(ttl_ns,id)`,
		"expression instead of column": `DROP INDEX sessions_updated_idx; CREATE INDEX sessions_updated_idx ON sessions((updated_ns+1),name)`,
		"missing display index":        `DROP INDEX swarm_display_facts_changed_idx`,
		"changed native index":         `DROP INDEX sessions_spawn_idx; CREATE INDEX sessions_spawn_idx ON sessions(parent_id,json_extract(settings_json,'$.other'))`,
		"changed transcript index":     `DROP INDEX transcript_display_users_idx; CREATE INDEX transcript_display_users_idx ON transcript_display(session_id,real_user,sequence) WHERE real_user>0`,
		"missing foreign keys":         `DROP TABLE session_artifacts; CREATE TABLE session_artifacts(session_id BLOB NOT NULL,digest BLOB NOT NULL,PRIMARY KEY(session_id,digest)) STRICT, WITHOUT ROWID; CREATE INDEX session_artifacts_digest_idx ON session_artifacts(digest)`,
		"missing constraint":           `DROP TABLE artifact_blobs; CREATE TABLE artifact_blobs(digest BLOB PRIMARY KEY NOT NULL,byte_count INTEGER NOT NULL CHECK(byte_count>=0),chunk_count INTEGER NOT NULL CHECK(chunk_count>=0),created_ns INTEGER NOT NULL) STRICT, WITHOUT ROWID`,
	} {
		t.Run(name, func(t *testing.T) {
			store, _ := openTestStore(t, ModeMemory, nil, 0)
			if _, err := store.db.Exec(query); err != nil {
				t.Fatal(err)
			}
			if err := store.CheckIntegrity(t.Context()); !errors.Is(err, ErrStoreCorrupt) {
				t.Fatalf("structural violation not classified: %v", err)
			}
			if err := store.PutModelCache(t.Context(), "blocked", []byte("no")); !errors.Is(err, ErrStoreCorrupt) {
				t.Fatalf("structural violation did not fence write: %v", err)
			}
		})
	}
}

func TestIntegritySchemaInspectionErrorIsNotCorruption(t *testing.T) {
	store, path := openTestStore(t, ModeDisk, nil, 0)
	d := maintenanceInjectDriver(t, store, path)
	d.failSchema.Store(true)
	if err := store.CheckIntegrity(t.Context()); !errors.Is(err, errMaintenanceInjected) {
		t.Fatalf("inspection error=%v", err)
	}
	status, _ := store.MaintenanceStatus(t.Context())
	if status.Corrupt || !errors.Is(status.LastIntegrityError, errMaintenanceInjected) {
		t.Fatalf("transient schema inspection fenced store: %+v", status)
	}
	if err := store.PutModelCache(t.Context(), "allowed", []byte("yes")); err != nil {
		t.Fatal(err)
	}
}

func TestHeartbeatObservedMismatchSurvivesUncertainCommit(t *testing.T) {
	overrideLeaseTiming(t, time.Hour, 2*time.Hour)
	store, path := openTestStore(t, ModeDisk, nil, 0)
	lost := acquireNamed(t, store, "lost").(*sqliteSession)
	owned := acquireNamed(t, store, "owned").(*sqliteSession)
	if _, err := store.db.Exec(`UPDATE session_leases SET owner_token=zeroblob(16) WHERE session_id=?`, lost.id); err != nil {
		t.Fatal(err)
	}
	d := maintenanceInjectDriver(t, store, path)
	d.failCommit.Store(true)
	if err := store.renewLeaseBatch(t.Context(), []*sqliteSession{lost, owned}); !errors.Is(err, errMaintenanceInjected) {
		t.Fatalf("uncertain commit=%v", err)
	}
	if !errors.Is(context.Cause(lost.ctx), ErrSessionLeaseLost) || owned.ctx.Err() != nil || d.commits.Load() != 1 || d.renewals.Load() != 2 {
		t.Fatalf("mismatch/uncertainty: lost=%v owned=%v commits=%d updates=%d", context.Cause(lost.ctx), owned.ctx.Err(), d.commits.Load(), d.renewals.Load())
	}
}
