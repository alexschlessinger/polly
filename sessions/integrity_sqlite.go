package sessions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"modernc.org/sqlite"
)

func checkDatabaseForeignKeys(ctx context.Context, conn *sql.Conn) error {
	// A clean database returns no rows; one row is already conclusive evidence.
	var violation [4]any
	switch err := conn.QueryRowContext(ctx, "PRAGMA foreign_key_check").Scan(&violation[0], &violation[1], &violation[2], &violation[3]); {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("check session database foreign keys: %w", err)
	default:
		return fmt.Errorf("%w: session database contains foreign-key violations", ErrStoreCorrupt)
	}
}

func confirmedCorruption(err error) bool {
	if errors.Is(err, ErrStoreCorrupt) {
		return true
	}
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		// Extended result codes retain the primary code in the low byte.
		code := sqliteErr.Code() & 0xff
		return code == 11 /* SQLITE_CORRUPT */ || code == 26 /* SQLITE_NOTADB */
	}
	return false
}

func (s *SQLiteStore) recordIntegrity(err error) {
	s.maintenanceMu.Lock()
	defer s.maintenanceMu.Unlock()
	s.maintenanceStatus.LastIntegrityCheck = time.Now().UTC()
	s.maintenanceStatus.LastIntegrityError = err
	if confirmedCorruption(err) {
		s.corrupt.Store(true)
	}
}

// CheckIntegrity validates the same global SQLite invariants as a full open.
// It does not repair, replay transactions, or clear an earlier corruption latch.
func (s *SQLiteStore) CheckIntegrity(ctx context.Context) (err error) {
	ctx, cancel := s.maintenanceContext(ctx)
	defer cancel()
	if err = s.ensureOpen(); err != nil {
		s.recordIntegrity(err)
		return err
	}
	recorded := false
	err = s.withConnection(ctx, false, func(conn *sql.Conn) error {
		err := runTransaction(ctx, conn, "BEGIN", func() error {
			var version int
			if err := conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
				return err
			}
			if version != schemaVersion {
				return schemaCorruption("session database version %d differs from supported version %d", version, schemaVersion)
			}
			if s.mode == ModeDisk {
				var autoVacuum int
				if err := conn.QueryRowContext(ctx, "PRAGMA auto_vacuum").Scan(&autoVacuum); err != nil {
					return err
				}
				if autoVacuum != 2 {
					return schemaCorruption("session database auto-vacuum mode is %d, want incremental", autoVacuum)
				}
			}
			if err := quickCheckDatabase(ctx, conn); err != nil {
				return err
			}
			if err := checkDatabaseForeignKeys(ctx, conn); err != nil {
				return err
			}
			return validateSchema(ctx, conn)
		})
		// Record the verdict before releasing the connection, so a queued
		// write cannot race a confirmed failure. A canceled/failed check is
		// observable but does not itself prove corruption.
		s.recordIntegrity(err)
		recorded = true
		return err
	})
	if !recorded {
		s.recordIntegrity(err)
	}
	return err
}

// MaintenanceStatus remains readable after Close for shutdown diagnostics.
func (s *SQLiteStore) MaintenanceStatus(ctx context.Context) (MaintenanceStatus, error) {
	if err := ctx.Err(); err != nil {
		return MaintenanceStatus{}, err
	}
	s.maintenanceMu.Lock()
	defer s.maintenanceMu.Unlock()
	status := s.maintenanceStatus
	status.Corrupt = s.corrupt.Load()
	return status, nil
}

func (s *SQLiteStore) integrityLoop(interval time.Duration) {
	defer s.wg.Done()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			// A tick and cancellation can both be ready after an interrupted
			// check. Do not start another attempt and overwrite its result.
			if s.ctx.Err() != nil {
				return
			}
			_ = s.CheckIntegrity(s.ctx)
		}
	}
}
