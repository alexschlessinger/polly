package sessions

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIntegrityMetadataSkipsOnlyCurrentSchemaScan(t *testing.T) {
	for _, tt := range []struct {
		name     string
		metadata bool
		wantErr  bool
	}{
		{"current default", false, true},
		{"current metadata", true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store, path := openTestStore(t, ModeDisk, nil, 0)
			// A CHECK violation in an otherwise unread artifact row makes
			// quick_check observable without timing assertions or mocks.
			for _, query := range []string{
				"PRAGMA ignore_check_constraints = ON",
				`INSERT INTO artifact_blobs(digest,byte_count,chunk_count,created_ns)
				 VALUES(zeroblob(32),-1,0,0)`,
			} {
				if _, err := store.db.Exec(query); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}

			mode := IntegrityStartup
			if tt.metadata {
				mode = IntegrityMetadata
			}
			reopened, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path, Integrity: mode})
			if tt.wantErr {
				if reopened != nil {
					_ = reopened.Close()
				}
				if err == nil || !strings.Contains(err.Error(), "integrity check failed") {
					t.Fatalf("open = %v, want integrity check failure", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				defer reopened.Close()
				if _, err := reopened.ListSummaries(context.Background()); err != nil {
					t.Fatal(err)
				}
				// Skipping the scan must not disable constraints on new writes.
				if _, err := reopened.db.Exec(`INSERT INTO messages(session_id,sequence,payload_json)
					VALUES(zeroblob(16),0,x'7b7d')`); err == nil {
					t.Fatal("foreign-key enforcement was disabled")
				}
				if _, err := reopened.db.Exec(`INSERT INTO artifact_blobs(digest,byte_count,chunk_count,created_ns)
					VALUES(randomblob(32),-1,0,0)`); err == nil {
					t.Fatal("CHECK enforcement was disabled")
				}
			}

			raw, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			var version int
			if err := raw.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != schemaVersion {
				t.Fatalf("version after open = %d, %v; want unchanged %d", version, err, schemaVersion)
			}
		})
	}
	t.Run("migration scans", func(t *testing.T) {
		path, _ := nativeSchema9Fixture(t, false)
		raw, err := sql.Open("sqlite", sqliteDataSource(ModeDisk, path))
		if err != nil {
			t.Fatal(err)
		}
		for _, query := range []string{
			"PRAGMA ignore_check_constraints = ON",
			`INSERT INTO artifact_blobs(digest,byte_count,chunk_count,created_ns)
			 VALUES(zeroblob(32),-1,0,0)`,
		} {
			if _, err := raw.Exec(query); err != nil {
				t.Fatal(err)
			}
		}
		if err := raw.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path, Integrity: IntegrityMetadata})
		if reopened != nil {
			_ = reopened.Close()
		}
		if err == nil || !strings.Contains(err.Error(), "integrity check failed") {
			t.Fatalf("migrating open = %v, want integrity check failure", err)
		}
	})
}

func TestIntegrityMetadataRetainsValidation(t *testing.T) {
	for _, tt := range []struct {
		name  string
		query string
		want  string
	}{
		{"future schema", "PRAGMA user_version = 999", "newer"},
		{"missing column", "DROP INDEX sessions_spawn_idx; ALTER TABLE sessions DROP COLUMN settings_json", "table sessions"},
		{"foreign key", `INSERT INTO messages(session_id,sequence,payload_json)
		 VALUES(zeroblob(16),0,x'7b7d')`, "foreign-key"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store, path := openTestStore(t, ModeDisk, nil, 0)
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			raw, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			if _, err := raw.Exec(tt.query); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path, Integrity: IntegrityMetadata})
			if reopened != nil {
				_ = reopened.Close()
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("open = %v, want %q", err, tt.want)
			}
		})
	}
	t.Run("invalid file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "polly.db")
		const original = "not a sqlite database"
		if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
			t.Fatal(err)
		}
		store, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path, Integrity: IntegrityMetadata})
		if store != nil {
			_ = store.Close()
		}
		if err == nil {
			t.Fatal("invalid database was silently opened")
		}
		if got, err := os.ReadFile(path); err != nil || string(got) != original {
			t.Fatalf("invalid database was changed: %q, %v", got, err)
		}
	})
}

func TestIntegrityMetadataCreatesAndMigrates(t *testing.T) {
	path, _ := nativeSchema9Fixture(t, false)
	store, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path, Integrity: IntegrityMetadata})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var version int
	if err := store.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("migrated version = %d, %v; want %d", version, err, schemaVersion)
	}
}
