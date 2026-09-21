package sessions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// DurableStore is the optional promotion interface. Promotion preserves live
// Session handles, stable IDs, cache identity, artifacts and owner tokens.
type DurableStore interface {
	Promote(context.Context, string) error
	Location() (StoreMode, string)
}

func (s *SQLiteStore) Location() (StoreMode, string) {
	s.dbMu.RLock()
	defer s.dbMu.RUnlock()
	return s.mode, s.path
}

// Promote merges this memory database into the normal disk store in one
// transaction, then atomically redirects all existing handles. All source
// operations, including artifact pages and heartbeats, share dbMu's read gate.
func (s *SQLiteStore) Promote(ctx context.Context, path string) error {
	s.dbMu.Lock()
	defer s.dbMu.Unlock()
	if err := s.ensureOpen(); err != nil {
		return err
	}
	if s.mode == ModeDisk {
		return nil
	}
	destination, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path, DefaultMetadata: s.defaults, AutoSessionTTL: s.autoTTL, CleanupInterval: s.cleanup})
	if err != nil {
		return err
	}
	transferred := false
	extended := map[*sqliteSession]int64{}
	defer func() {
		if !transferred {
			_ = destination.Close()
		}
	}()
	// Foreign keys are checked at commit so self-referential parent rows can
	// be copied in any order. Existing unrelated disk sessions are retained.
	err = destination.withWrite(ctx, func(conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, "PRAGMA defer_foreign_keys=ON"); err != nil {
			return err
		}
		for _, table := range []string{"sessions", "messages", "artifact_blobs", "artifact_chunks", "session_artifacts", "session_leases", "swarm_records", "swarm_artifacts", "swarm_members"} {
			if err := copyTable(ctx, s.db, conn, table); err != nil {
				return fmt.Errorf("promote %s: %w", table, err)
			}
		}
		// Import may take longer than a lease heartbeat. Extend only this
		// process's live owners before releasing the source operation gate.
		s.mu.Lock()
		defer s.mu.Unlock()
		now, expiry := leaseWindow()
		for session := range s.open {
			if _, err := extendLease(ctx, conn, session.id, session.ownerToken, now.UnixNano(), expiry); err != nil {
				return err
			}
			extended[session] = expiry
		}
		return nil
	})
	if err != nil {
		return err
	}
	for session, expiry := range extended {
		session.expiresNS.Store(expiry)
	}
	destination.cancel(ErrStoreClosed)
	destination.wg.Wait()
	destination.closed.Store(true)
	old := s.db
	s.db = destination.db
	s.mode = ModeDisk
	s.path = destination.path
	transferred = true
	return old.Close()
}

// copyTable inserts every row of table from source into conn. A session whose
// name is already taken on the destination is renamed with its id suffix.
// Shared artifact bytes may already exist on the destination and are kept.
func copyTable(ctx context.Context, source *sql.DB, conn *sql.Conn, table string) error {
	rows, err := source.QueryContext(ctx, "SELECT * FROM "+table)
	if err != nil {
		return err
	}
	columns, err := rows.Columns()
	if err != nil {
		rows.Close()
		return err
	}
	verb := "INSERT"
	if table == "artifact_blobs" || table == "artifact_chunks" {
		verb = "INSERT OR IGNORE"
	}
	insert := verb + " INTO " + table + " (" + strings.Join(columns, ",") + ") VALUES (" + strings.TrimSuffix(strings.Repeat("?,", len(columns)), ",") + ")"
	idIndex, nameIndex := slices.Index(columns, "id"), -1
	if table == "sessions" {
		nameIndex = slices.Index(columns, "name")
	}
	values := make([]any, len(columns))
	pointers := make([]any, len(columns))
	for i := range pointers {
		pointers[i] = &values[i]
	}
	return eachRow(rows, func() error {
		if err := rows.Scan(pointers...); err != nil {
			return err
		}
		if nameIndex >= 0 {
			name, _ := values[nameIndex].(string)
			if _, err := sessionIDByName(ctx, conn, name); err == nil {
				values[nameIndex] = fmt.Sprintf("%s-%x", name, values[idIndex])
			} else if !errors.Is(err, ErrSessionNotFound) {
				return err
			}
		}
		_, err := conn.ExecContext(ctx, insert, values...)
		return err
	})
}
