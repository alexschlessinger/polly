package sessions

import (
	"context"
	"database/sql"
	"time"
)

// One FULL-durability commit per bounded group instead of one autocommit/fsync
// per active session. No lease is selected from the database: only this store's
// tracked owners can renew, with the original owner-token fence on each UPDATE.
const leaseHeartbeatBatchSize = 128

func (s *SQLiteStore) heartbeatLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(leaseHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.mu.Lock()
			owners := make([]*sqliteSession, 0, len(s.open))
			for session := range s.open {
				if !session.closed.Load() && session.ctx.Err() == nil {
					owners = append(owners, session)
				}
			}
			s.mu.Unlock()
			for len(owners) > 0 && s.ctx.Err() == nil {
				n := min(len(owners), leaseHeartbeatBatchSize)
				ctx, cancel := context.WithTimeout(s.ctx, leaseHeartbeatInterval)
				_ = s.renewLeaseBatch(ctx, owners[:n])
				cancel()
				owners = owners[n:]
			}
		}
	}
}

func (s *SQLiteStore) renewLeaseBatch(ctx context.Context, owners []*sqliteSession) error {
	var lost []*sqliteSession
	err := s.withWrite(ctx, func(conn *sql.Conn) error {
		// Compute the window after waiting for the connection/write lock. A
		// legitimate long read must not make a just-renewed lease already stale.
		now, expiresNS := leaseWindow()
		for _, session := range owners {
			if session.closed.Load() || session.ctx.Err() != nil {
				continue
			}
			owned, err := extendLease(ctx, conn, session.id, session.ownerToken, now.UnixNano(), expiresNS)
			if err != nil {
				return err
			}
			if !owned {
				lost = append(lost, session)
			}
		}
		return nil
	})
	// A successful zero-row UPDATE proves token mismatch under the write
	// lock even if a later peer UPDATE or COMMIT fails. Only these observed
	// mismatches cancel handles; unvisited/uncertain renewals do not.
	for _, session := range lost {
		session.cancel(ErrSessionLeaseLost)
	}
	// No automatic replay on a failed/uncertain COMMIT. Starvation or a
	// transient error is not evidence of lost ownership; the next scheduled
	// tick is a new renewal attempt, still fenced by the exact owner token.
	return err
}
