package sessions

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
)

func TestReviewSchemaMismatchFencesWrites(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	if _, err := store.db.Exec(`DROP INDEX sessions_deadline_idx`); err != nil {
		t.Fatal(err)
	}
	if err := store.CheckIntegrity(t.Context()); err == nil {
		t.Fatal("missing required index accepted")
	}
	status, err := store.MaintenanceStatus(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !status.Corrupt {
		t.Fatal("confirmed missing schema object did not latch corruption")
	}
	if err := store.PutModelCache(t.Context(), "after-corruption", []byte(`{}`)); !errors.Is(err, ErrStoreCorrupt) {
		t.Fatalf("direct writer after schema corruption = %v", err)
	}
}

func TestReviewConfirmedLostLeaseSurvivesPeerFailure(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	lost := acquireNamed(t, store, "lost").(*sqliteSession)
	poisoned := acquireNamed(t, store, "poisoned").(*sqliteSession)
	if _, err := store.db.Exec(`UPDATE session_leases SET owner_token=randomblob(16) WHERE session_id=?`, lost.id); err != nil {
		t.Fatal(err)
	}
	trigger := fmt.Sprintf(`CREATE TRIGGER reject_review_renewal BEFORE UPDATE ON session_leases WHEN NEW.session_id=x'%x' BEGIN SELECT RAISE(ABORT,'injected peer renewal failure'); END`, poisoned.id)
	if _, err := store.db.Exec(trigger); err != nil {
		t.Fatal(err)
	}
	err := store.renewLeaseBatch(t.Context(), []*sqliteSession{lost, poisoned})
	if err == nil {
		t.Fatal("injected peer failure missing")
	}
	if !errors.Is(context.Cause(lost.Context()), ErrSessionLeaseLost) {
		t.Fatalf("proved token mismatch hidden by peer failure: cause=%v batchErr=%v", context.Cause(lost.Context()), err)
	}
	if poisoned.Context().Err() != nil {
		t.Fatal("peer error incorrectly proved peer ownership lost")
	}
}

func TestReviewOwnerScopedGCKeepsSharedAndPinnedBlobs(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	a := acquireNamed(t, store, "gc-a").(*sqliteSession)
	b := acquireNamed(t, store, "gc-b").(*sqliteSession)
	parent := acquireNamed(t, store, "gc-parent").(*sqliteSession)
	blob := artifacts.Blob{Data: []byte("shared review blob")}
	ref, err := a.ArtifactStore().Put(t.Context(), blob)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.ArtifactStore().Put(t.Context(), blob); err != nil {
		t.Fatal(err)
	}
	digest, err := artifactDigest(ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO swarm_artifacts(parent_id,digest) VALUES(?,?)`, parent.id, digest); err != nil {
		t.Fatal(err)
	}
	// An unrelated orphan must not be collected by an owner-local removal.
	if _, err := store.db.Exec(`INSERT INTO artifact_blobs(digest,byte_count,chunk_count,created_ns) VALUES(zeroblob(32),0,0,0)`); err != nil {
		t.Fatal(err)
	}
	if err := a.ArtifactStore().RemoveAll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := b.ArtifactStore().RemoveAll(t.Context()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.db.QueryRow(`SELECT count(*) FROM artifact_blobs`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("private removals removed pinned/shared or unrelated blob: count=%d", count)
	}
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(t.Context(), "gc-parent"); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT count(*) FROM artifact_blobs`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("parent deletion did not collect only removed pin's blob: count=%d", count)
	}
}

func TestReviewMaintenanceFairnessAndOverflow(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	pinned := acquireNamed(t, store, "retained").(*sqliteSession)
	expired := acquireNamed(t, store, "expired").(*sqliteSession)
	future := acquireNamed(t, store, "overflow").(*sqliteSession)
	if err := expired.Close(); err != nil {
		t.Fatal(err)
	}
	if err := future.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE sessions SET updated_ns=1,ttl_ns=1 WHERE id=?`, pinned.id); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE sessions SET updated_ns=2,ttl_ns=1 WHERE id=?`, expired.id); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE sessions SET updated_ns=?,ttl_ns=9223372036854775807 WHERE id=?`, time.Now().UnixNano(), future.id); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO artifact_blobs(digest,byte_count,chunk_count,created_ns) VALUES(zeroblob(32),0,0,0)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		result, err := store.Maintain(t.Context(), MaintenanceOptions{Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		if result.Examined > 1 {
			t.Fatalf("Limit=1 exceeded: %+v", result)
		}
	}
	for _, tt := range []struct {
		name string
		want int
	}{{"retained", 1}, {"expired", 0}, {"overflow", 1}} {
		var count int
		if err := store.db.QueryRow(`SELECT count(*) FROM sessions WHERE name=?`, tt.name).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != tt.want {
			t.Fatalf("%s count=%d want=%d", tt.name, count, tt.want)
		}
	}
	var count int
	if err := store.db.QueryRow(`SELECT count(*) FROM artifact_blobs`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expiry candidates starved blob collection: %d", count)
	}
}
