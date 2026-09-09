package sessions

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
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
		for _, table := range []string{"sessions", "messages", "artifact_blobs", "artifact_chunks", "session_artifacts", "session_leases", "session_reports", "swarm_records", "swarm_artifacts", "swarm_members"} {
			rows, err := s.db.QueryContext(ctx, "SELECT * FROM "+table)
			if err != nil {
				return err
			}
			columns, err := rows.Columns()
			if err != nil {
				rows.Close()
				return err
			}
			for rows.Next() {
				values := make([]any, len(columns))
				pointers := make([]any, len(columns))
				for i := range pointers {
					pointers[i] = &values[i]
				}
				if err = rows.Scan(pointers...); err != nil {
					rows.Close()
					return err
				}
				if table == "sessions" {
					nameIndex := -1
					var identity any
					for i, col := range columns {
						if col == "name" {
							nameIndex = i
						}
						if col == "id" {
							identity = values[i]
						}
					}
					if nameIndex >= 0 {
						var exists bool
						if err = conn.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sessions WHERE name=?)", values[nameIndex]).Scan(&exists); err != nil {
							rows.Close()
							return err
						}
						if exists {
							values[nameIndex] = fmt.Sprintf("%s-%x", values[nameIndex], identity)
						}
					}
				}
				// Reports have database-local integer IDs, unlike every stable
				// session/coordination identity. Allocate new destination IDs.
				if table == "session_reports" {
					for i, col := range columns {
						if col == "id" {
							values[i] = nil
						}
					}
				}
				verb := "INSERT"
				if table == "artifact_blobs" || table == "artifact_chunks" {
					verb = "INSERT OR IGNORE"
				}
				query := verb + " INTO " + table + " (" + strings.Join(columns, ",") + ") VALUES (" + strings.TrimSuffix(strings.Repeat("?,", len(columns)), ",") + ")"
				if _, err = conn.ExecContext(ctx, query, values...); err != nil {
					rows.Close()
					return fmt.Errorf("promote %s: %w", table, err)
				}
			}
			if err = rows.Err(); err != nil {
				rows.Close()
				return err
			}
			if err = rows.Close(); err != nil {
				return err
			}
		}
		// Import may take longer than a lease heartbeat. Extend only this
		// process's live owners before releasing the source operation gate.
		s.mu.Lock()
		defer s.mu.Unlock()
		for session := range s.open {
			now := time.Now()
			expiry := now.Add(leaseStaleAfter).UnixNano()
			if _, err := conn.ExecContext(ctx, "UPDATE session_leases SET heartbeat_ns=?,expires_ns=? WHERE session_id=? AND owner_token=?", now.UnixNano(), expiry, session.id, session.ownerToken); err != nil {
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
