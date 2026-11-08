package sessions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
)

func maintenanceSeedSessions(t *testing.T, store *SQLiteStore, n int, expired bool) [][]byte {
	t.Helper()
	ids := make([][]byte, n)
	err := store.withWrite(t.Context(), func(conn *sql.Conn) error {
		statement, err := conn.PrepareContext(t.Context(), `INSERT INTO sessions(id,name,retention,created_ns,updated_ns,ttl_ns,settings_json) VALUES(?,?,'named',1,?,1,x'7b7d')`)
		if err != nil {
			return err
		}
		defer statement.Close()
		updated := time.Now().Add(time.Hour).UnixNano()
		if expired {
			updated = 1
		}
		for i := range ids {
			ids[i] = []byte(fmt.Sprintf("%016d", i))
			if _, err := statement.ExecContext(t.Context(), ids[i], fmt.Sprintf("candidate-%d", i), updated); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

func maintenanceSeedBlobs(t *testing.T, store *SQLiteStore, n int, owner []byte) [][]byte {
	t.Helper()
	digests := make([][]byte, n)
	err := store.withWrite(t.Context(), func(conn *sql.Conn) error {
		for i := range digests {
			digests[i] = []byte(fmt.Sprintf("%032d", i))
			if _, err := conn.ExecContext(t.Context(), `INSERT INTO artifact_blobs VALUES(?,0,0,0)`, digests[i]); err != nil {
				return err
			}
			if owner != nil {
				if _, err := conn.ExecContext(t.Context(), `INSERT INTO session_artifacts VALUES(?,?)`, owner, digests[i]); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return digests
}

func maintenanceCount(t *testing.T, store *SQLiteStore, table string) int {
	t.Helper()
	var count int
	if err := store.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestMaintenanceBoundedCandidatesAndFairness(t *testing.T) {
	for _, n := range []int{32, 4096} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			store, _ := openTestStore(t, ModeMemory, nil, 0)
			ids := maintenanceSeedSessions(t, store, n, true)
			// Pin every expired candidate. They must consume the budget, not
			// force a scan through the whole index looking for a deletable row.
			if _, err := store.db.Exec(`INSERT INTO swarm_members(parent_id,member_id) SELECT id,id FROM sessions`); err != nil {
				t.Fatal(err)
			}
			maintenanceSeedBlobs(t, store, n, ids[0])
			if _, err := store.db.Exec(`DELETE FROM session_artifacts WHERE digest=?`, []byte(fmt.Sprintf("%032d", 1))); err != nil {
				t.Fatal(err)
			}
			for call := 0; call < 8; call++ {
				result, err := store.Maintain(t.Context(), MaintenanceOptions{Limit: 1})
				if err != nil || result.Examined != 1 || result.SessionsDeleted != 0 {
					t.Fatalf("call %d: %+v %v", call, result, err)
				}
				if call == 3 && result.ArtifactsDeleted != 1 {
					t.Fatalf("blob pass starved by pinned sessions: %+v", result)
				}
			}
			if got := maintenanceCount(t, store, "artifact_blobs"); got != n-1 {
				t.Fatalf("blobs=%d", got)
			}
			t.Logf("population=%d retained sessions + %d blobs: 8 calls, exactly 8 candidates examined, orphan reached on call 4", n, n)
		})
	}
}

func TestMaintenanceRetainsLeaseSwarmPinsAndSharedArtifacts(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	ids := maintenanceSeedSessions(t, store, 6, true)
	for _, query := range []string{
		`INSERT INTO session_leases VALUES(?,zeroblob(16),0,9223372036854775807)`,
	} {
		if _, err := store.db.Exec(query, ids[0]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.Exec(`INSERT INTO swarm_members VALUES(?,?)`, ids[1], ids[2]); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO swarm_records VALUES(?,'task','pinned',x'7b7d')`, ids[1]); err != nil {
		t.Fatal(err)
	}
	digests := maintenanceSeedBlobs(t, store, 4, ids[3])
	if _, err := store.db.Exec(`INSERT INTO session_artifacts VALUES(?,?)`, ids[0], digests[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO swarm_artifacts VALUES(?,?)`, ids[1], digests[1]); err != nil {
		t.Fatal(err)
	}
	for call := 0; call < 8; call++ {
		result, err := store.Maintain(t.Context(), MaintenanceOptions{Limit: 2})
		if err != nil || result.Examined > 2 {
			t.Fatalf("maintain=%+v %v", result, err)
		}
	}
	if got := maintenanceCount(t, store, "sessions"); got != 3 {
		t.Fatalf("sessions=%d, want live + parent + member", got)
	}
	if got := maintenanceCount(t, store, "artifact_blobs"); got != 2 {
		t.Fatalf("shared/pinned blobs=%d", got)
	}
	// A pin removed behind the cursor is reconsidered after wrap.
	if _, err := store.db.Exec(`DELETE FROM session_leases`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if _, err := store.Maintain(t.Context(), MaintenanceOptions{Limit: 2}); err != nil {
			t.Fatal(err)
		}
	}
	if got := maintenanceCount(t, store, "sessions"); got != 2 {
		t.Fatalf("released lease not reconsidered: %d", got)
	}
}

func TestMaintenanceExpiryOnlyAndOwnerScopedCollection(t *testing.T) {
	for _, operation := range []string{"startup", "expire", "delete", "close-auto", "clear", "remove-all", "reset"} {
		t.Run(operation, func(t *testing.T) {
			store, path := openTestStore(t, ModeDisk, nil, 0)
			session, err := store.Acquire(t.Context(), "owner", AcquireOptions{Auto: operation == "close-auto"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := session.ArtifactStore().Put(t.Context(), artifacts.Blob{Data: []byte("owned")}); err != nil {
				t.Fatal(err)
			}
			maintenanceSeedBlobs(t, store, 1, nil)
			switch operation {
			case "startup", "expire":
				if _, err := store.db.Exec(`UPDATE sessions SET ttl_ns=1,updated_ns=1`); err != nil {
					t.Fatal(err)
				}
				if err := session.Close(); err != nil {
					t.Fatal(err)
				}
				if operation == "startup" {
					if err := store.Close(); err != nil {
						t.Fatal(err)
					}
					store, err = OpenStore(StoreConfig{Mode: ModeDisk, Path: path})
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = store.Close() })
				} else {
					err = store.Expire(t.Context())
				}
			case "delete":
				if err = session.Close(); err == nil {
					err = store.Delete(t.Context(), "owner")
				}
			case "close-auto":
				err = session.Close()
			case "clear":
				err = session.Clear(t.Context())
			case "remove-all":
				err = session.ArtifactStore().RemoveAll(t.Context())
			case "reset":
				err = session.Reset(t.Context(), &Metadata{})
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := maintenanceCount(t, store, "artifact_blobs"); got != 1 {
				t.Fatalf("unrelated orphan collected or owner leaked: blobs=%d", got)
			}
		})
	}
}

func TestMaintenanceRollbackCancellationAndStatus(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	maintenanceSeedSessions(t, store, 4, true)
	if _, err := store.db.Exec(`CREATE TEMP TRIGGER veto_delete BEFORE DELETE ON sessions WHEN old.name='candidate-1' BEGIN SELECT RAISE(ABORT,'veto'); END`); err != nil {
		t.Fatal(err)
	}
	before := store.maintenancePosition
	result, err := store.Maintain(t.Context(), MaintenanceOptions{Limit: 4})
	if err == nil || !strings.Contains(err.Error(), "veto") || result != (MaintenanceResult{}) {
		t.Fatalf("rollback=%+v %v", result, err)
	}
	if maintenanceCount(t, store, "sessions") != 4 || !reflect.DeepEqual(store.maintenancePosition, before) {
		t.Fatal("rollback changed rows/cursors")
	}
	status, _ := store.MaintenanceStatus(t.Context())
	if status.LastMaintenance.IsZero() || status.LastMaintenanceError == nil || status.Corrupt {
		t.Fatalf("status=%+v", status)
	}
	if _, err := store.db.Exec(`DROP TRIGGER veto_delete`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := store.Maintain(ctx, MaintenanceOptions{Limit: 4}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v", err)
	}
	for _, limit := range []int{-1, 0, MaxMaintenanceLimit + 1} {
		if _, err := store.Maintain(t.Context(), MaintenanceOptions{Limit: limit}); !errors.Is(err, ErrInvalidPageRequest) {
			t.Fatalf("limit=%d: %v", limit, err)
		}
	}
	if _, err := store.Maintain(t.Context(), MaintenanceOptions{Limit: 4}); err != nil {
		t.Fatal(err)
	}
	status, _ = store.MaintenanceStatus(t.Context())
	if status.LastMaintenanceError != nil {
		t.Fatalf("status did not clear latest error: %+v", status)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Maintain(t.Context(), MaintenanceOptions{Limit: 1}); !errors.Is(err, ErrStoreClosed) {
		t.Fatalf("closed=%v", err)
	}
}

func TestMaintenanceExpiryIndexPlans(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	for _, position := range []expiryPosition{{}, {deadline: 2, id: []byte("0000000000000128")}} {
		query, args := expiryCandidatesQuery(position, time.Now().UnixNano(), 7)
		var plans []string
		err := queryEach(t.Context(), store.db, "EXPLAIN QUERY PLAN "+query, args, func(rows *sql.Rows) error {
			var id, parent, unused int
			var plan string
			if err := rows.Scan(&id, &parent, &unused, &plan); err != nil {
				return err
			}
			plans = append(plans, plan)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(plans, "\n")
		t.Log(joined)
		if !strings.Contains(joined, "SEARCH sessions USING INDEX sessions_deadline_idx") || strings.Contains(joined, "TEMP B-TREE") {
			t.Fatalf("expiry candidate selection is not indexed: %s", joined)
		}
	}
}
