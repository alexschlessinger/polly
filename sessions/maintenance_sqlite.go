package sessions

import (
	"context"
	"database/sql"
	"time"
)

var _ MaintenanceStore = (*SQLiteStore)(nil)

// Saturation keeps the key an INTEGER even for a far-future TTL. The original
// expiry predicate is still checked at deletion, including its boundary rules.
const expiryKeySQL = `(CASE WHEN updated_ns > 9223372036854775807 - ttl_ns THEN 9223372036854775807 ELSE updated_ns + ttl_ns END)`
const expiryIndexSQL = `CREATE INDEX IF NOT EXISTS sessions_deadline_idx ON sessions(` + expiryKeySQL + `,id) WHERE ttl_ns > 0`

type expiryPosition struct {
	deadline int64
	id       []byte
}

type maintenancePosition struct {
	expiry    expiryPosition
	blob      []byte
	blobFirst bool
}

// maintenanceContext also interrupts explicit maintenance during store shutdown.
func (s *SQLiteStore) maintenanceContext(ctx context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.ctx, cancel)
	return ctx, func() { stop(); cancel() }
}

func (s *SQLiteStore) lockMaintenance(ctx context.Context) error {
	if err := s.ensureOpen(); err != nil {
		return err
	}
	select {
	case s.maintenanceGate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-s.maintenanceGate
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *SQLiteStore) recordMaintenance(err error) {
	s.maintenanceMu.Lock()
	defer s.maintenanceMu.Unlock()
	s.maintenanceStatus.LastMaintenance = time.Now().UTC()
	s.maintenanceStatus.LastMaintenanceError = err
}

// Maintain inspects keyset windows before applying retention/reference filters.
// A retained session or referenced blob therefore consumes a candidate too;
// neither can hide an unbounded anti-join scan behind LIMIT. Cursors are only
// advanced after successful commit and wrap at the end of each index.
func (s *SQLiteStore) Maintain(ctx context.Context, options MaintenanceOptions) (result MaintenanceResult, err error) {
	if options.Limit < 1 || options.Limit > MaxMaintenanceLimit {
		return result, ErrInvalidPageRequest
	}
	ctx, cancel := s.maintenanceContext(ctx)
	defer cancel()
	defer func() { s.recordMaintenance(err) }()
	if err = s.lockMaintenance(ctx); err != nil {
		return result, err
	}
	defer func() { <-s.maintenanceGate }()
	position := s.maintenancePosition
	err = s.withWrite(ctx, func(conn *sql.Conn) error {
		first := (options.Limit + 1) / 2
		if position.blobFirst {
			if err := maintainBlobs(ctx, conn, &position.blob, first, &result); err != nil {
				return err
			}
			return maintainExpiry(ctx, conn, &position.expiry, options.Limit-result.Examined, time.Now().UnixNano(), &result)
		}
		if err := maintainExpiry(ctx, conn, &position.expiry, first, time.Now().UnixNano(), &result); err != nil {
			return err
		}
		return maintainBlobs(ctx, conn, &position.blob, options.Limit-result.Examined, &result)
	})
	if err != nil {
		// COMMIT failures can be uncertain. Do not replay or advertise counts.
		return MaintenanceResult{}, err
	}
	position.blobFirst = !position.blobFirst
	s.maintenancePosition = position
	if result.SessionsDeleted > 0 || result.ArtifactsDeleted > 0 {
		s.incrementalVacuum(ctx)
	}
	return result, nil
}

const expiryCandidatesBase = `SELECT ` + expiryKeySQL + ` AS deadline,id FROM sessions INDEXED BY sessions_deadline_idx WHERE ttl_ns > 0 AND ` + expiryKeySQL + ` <= ?`

func expiryCandidatesQuery(position expiryPosition, nowNS int64, limit int) (string, []any) {
	if position.id == nil {
		return expiryCandidatesBase + ` ORDER BY deadline,id LIMIT ?`, []any{nowNS, limit}
	}
	// SQLite does not seek an expression index from a row-value comparison.
	// Read the equal-deadline suffix first, then fill the remaining budget
	// from later deadlines. Both queries seek and stream without a temp sort.
	return expiryCandidatesBase + ` AND ` + expiryKeySQL + ` = ? AND id > ? ORDER BY id LIMIT ?`, []any{nowNS, position.deadline, position.id, limit}
}

func laterExpiryCandidatesQuery(deadline, nowNS int64, limit int) (string, []any) {
	return expiryCandidatesBase + ` AND ` + expiryKeySQL + ` > ? ORDER BY deadline,id LIMIT ?`, []any{nowNS, deadline, limit}
}

func maintainExpiry(ctx context.Context, conn *sql.Conn, position *expiryPosition, limit int, nowNS int64, result *MaintenanceResult) error {
	if limit == 0 {
		return nil
	}
	query, args := expiryCandidatesQuery(*position, nowNS, limit)
	var candidates []expiryPosition
	collect := func(rows *sql.Rows) error {
		var candidate expiryPosition
		if err := rows.Scan(&candidate.deadline, &candidate.id); err != nil {
			return err
		}
		candidates = append(candidates, candidate)
		return nil
	}
	if err := queryEach(ctx, conn, query, args, collect); err != nil {
		return err
	}
	if position.id != nil && len(candidates) < limit {
		query, args = laterExpiryCandidatesQuery(position.deadline, nowNS, limit-len(candidates))
		if err := queryEach(ctx, conn, query, args, collect); err != nil {
			return err
		}
	}
	for _, candidate := range candidates {
		result.Examined++
		deleted, blobs, err := deleteSessionCandidates(ctx, conn, "id = ? AND "+expiredSQL+" AND NOT "+retainedSQL, candidate.id, nowNS, nowNS, nowNS)
		if err != nil {
			return err
		}
		result.SessionsDeleted += deleted
		result.ArtifactsDeleted += blobs
	}
	if len(candidates) == limit {
		*position = candidates[len(candidates)-1]
		result.More = true
	} else {
		*position = expiryPosition{}
	}
	return nil
}

func maintainBlobs(ctx context.Context, conn *sql.Conn, position *[]byte, limit int, result *MaintenanceResult) error {
	if limit == 0 {
		return nil
	}
	query := `SELECT digest FROM artifact_blobs`
	var args []any
	if *position != nil {
		query += ` WHERE digest > ?`
		args = append(args, *position)
	}
	query += ` ORDER BY digest LIMIT ?`
	args = append(args, limit)
	var candidates [][]byte
	if err := queryEach(ctx, conn, query, args, func(rows *sql.Rows) error {
		var digest []byte
		if err := rows.Scan(&digest); err != nil {
			return err
		}
		candidates = append(candidates, digest)
		return nil
	}); err != nil {
		return err
	}
	for _, digest := range candidates {
		result.Examined++
		deleted, err := collectArtifactCandidate(ctx, conn, digest)
		if err != nil {
			return err
		}
		result.ArtifactsDeleted += deleted
	}
	if len(candidates) == limit {
		*position = candidates[len(candidates)-1]
		result.More = true
	} else {
		*position = nil
	}
	return nil
}

// expireBatch is expiry-only. Startup/Expire exhaust a local cursor, while the
// background sweep keeps a cursor between ticks so pinned rows cannot starve
// later sessions. Neither path collects unrelated orphan artifacts.
func (s *SQLiteStore) expireBatch(ctx context.Context, position *expiryPosition, nowNS int64) (result MaintenanceResult, err error) {
	defer func() { s.recordMaintenance(err) }()
	next := *position
	err = s.withWrite(ctx, func(conn *sql.Conn) error {
		return maintainExpiry(ctx, conn, &next, MaxMaintenanceLimit, nowNS, &result)
	})
	if err != nil {
		return MaintenanceResult{}, err
	}
	*position = next
	if result.SessionsDeleted > 0 {
		s.incrementalVacuum(ctx)
	}
	return result, nil
}

const unreferencedArtifactSQL = `NOT EXISTS (SELECT 1 FROM session_artifacts WHERE session_artifacts.digest=artifact_blobs.digest)
	AND NOT EXISTS (SELECT 1 FROM swarm_artifacts WHERE swarm_artifacts.digest=artifact_blobs.digest)`

func collectArtifactCandidate(ctx context.Context, conn *sql.Conn, digest []byte) (int, error) {
	result, err := conn.ExecContext(ctx, `DELETE FROM artifact_blobs WHERE digest=? AND `+unreferencedArtifactSQL, digest)
	if err != nil {
		return 0, err
	}
	count, err := result.RowsAffected()
	return int(count), err
}

// collectRemovedArtifacts considers only references removed by this operation.
// An atomic session deletion/reset can touch all of that session's artifacts,
// but never scans the global blob table to find unrelated garbage.
func collectRemovedArtifacts(ctx context.Context, conn *sql.Conn, query string, args []any, remove func() error) (int, error) {
	var candidates [][]byte
	if err := queryEach(ctx, conn, query, args, func(rows *sql.Rows) error {
		var digest []byte
		if err := rows.Scan(&digest); err != nil {
			return err
		}
		candidates = append(candidates, digest)
		return nil
	}); err != nil {
		return 0, err
	}
	if err := remove(); err != nil {
		return 0, err
	}
	count := 0
	for _, digest := range candidates {
		deleted, err := collectArtifactCandidate(ctx, conn, digest)
		if err != nil {
			return 0, err
		}
		count += deleted
	}
	return count, nil
}

func deleteSessionCandidates(ctx context.Context, conn *sql.Conn, where string, args ...any) (int, int, error) {
	var ids [][]byte
	if err := queryEach(ctx, conn, `SELECT id FROM sessions WHERE `+where, args, func(rows *sql.Rows) error {
		var id []byte
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
		return nil
	}); err != nil {
		return 0, 0, err
	}
	sessions, blobs := 0, 0
	for _, id := range ids {
		count, err := collectRemovedArtifacts(ctx, conn,
			`SELECT digest FROM session_artifacts WHERE session_id=? UNION SELECT digest FROM swarm_artifacts WHERE parent_id=?`, []any{id, id}, func() error {
				result, err := conn.ExecContext(ctx, `DELETE FROM sessions WHERE id=?`, id)
				if err != nil {
					return err
				}
				count, err := result.RowsAffected()
				sessions += int(count)
				return err
			})
		if err != nil {
			return 0, 0, err
		}
		blobs += count
	}
	return sessions, blobs, nil
}

func removeSessionArtifactReferences(ctx context.Context, conn *sql.Conn, where string, args ...any) error {
	_, err := collectRemovedArtifacts(ctx, conn, `SELECT digest FROM session_artifacts WHERE `+where, args, func() error {
		_, err := conn.ExecContext(ctx, `DELETE FROM session_artifacts WHERE `+where, args...)
		return err
	})
	return err
}
