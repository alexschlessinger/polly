package sessions

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"log/slog"
	"time"
)

// leaseKeep is how long the store keeps the lease on a session after its
// handle closed.
var leaseKeep = 15 * time.Second

// releaseBatch bounds how many leases one transaction gives up.
const releaseBatch = 1000

// lease is one session lease this store holds.
type lease struct {
	id, token []byte
}

// keptLease is the lease on a member of a family this store drives, after
// the member's handle closed. A member runs in slices, each on a handle of
// its own, and the store that drives its family is the one that starts the
// next. So the store keeps the lease for leaseKeep and hands it to the next
// handle on the session without asking the database. To everyone else the
// session stays leased meanwhile; to this store's own callers it is free.
type keptLease struct {
	lease
	// name is the session's name when its handle closed, nil if unknown.
	name *string
	// root is the family's root. The lease is kept while this store has it open.
	root   string
	expiry int64
	since  time.Time
	// sequence is the transcript's next sequence when known is set: nothing
	// writes the session's row while no handle has it open.
	sequence int64
	known    bool
}

func (s *SQLiteStore) newSession(id, owner []byte, name string, expiry int64) *sqliteSession {
	ctx, cancel := context.WithCancelCause(s.ctx)
	session := &sqliteSession{
		store:      s,
		id:         id,
		ownerToken: owner,
		ctx:        ctx,
		cancel:     cancel,
		closeDone:  make(chan struct{}),
	}
	session.artifacts = &sqliteArtifactStore{session: session}
	session.name.Store(&name)
	session.leaseExpiry.Store(expiry)
	return session
}

// keep takes over the lease of the closing handle session when it is a
// member of a family this store drives, and reports whether it did.
func (s *SQLiteStore) keep(session *sqliteSession, kept *keptLease) bool {
	if kept.root == "" || kept.root == string(session.id) || !session.leaseFresh() {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	root := s.byID[kept.root]
	if s.closed.Load() || root == nil || root.closed.Load() || !root.leaseFresh() {
		return false
	}
	delete(s.open, session)
	if s.byID[string(session.id)] == session {
		delete(s.byID, string(session.id))
	}
	s.kept[string(session.id)] = kept
	return true
}

// retake opens a handle on the lease the store keeps on the session with
// this name and id, if it keeps one that cannot have run out.
func (s *SQLiteStore) retake(name, expectedID string) *sqliteSession {
	if expectedID == "" {
		return nil
	}
	id, err := hex.DecodeString(expectedID)
	if err != nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.kept[string(id)]
	if kept == nil || s.closed.Load() || kept.name == nil || *kept.name != name ||
		time.Now().UnixNano() >= kept.expiry-int64(leaseStaleAfter/4) {
		return nil
	}
	delete(s.kept, string(id))
	session := s.newSession(kept.id, kept.token, name, kept.expiry)
	session.coord.parent, session.coord.sequence, session.coord.known = []byte(kept.root), kept.sequence, kept.known
	session.family.Store(&kept.root)
	s.open[session] = struct{}{}
	s.byID[string(id)] = session
	return session
}

// keeps reports whether the lease on the session id is one the store keeps:
// no handle has the session open.
func (s *SQLiteStore) keeps(id []byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.kept[string(id)] != nil
}

// yieldKept gives up the lease the store keeps on the session id, for a
// caller that wants the session some other way than retake hands it out. It
// reports whether there was one.
func (s *SQLiteStore) yieldKept(ctx context.Context, id []byte) bool {
	s.mu.Lock()
	kept := s.kept[string(id)]
	delete(s.kept, string(id))
	s.mu.Unlock()
	if kept == nil {
		return false
	}
	_ = s.release(ctx, kept.lease)
	return true
}

// yieldKeptNamed is yieldKept for a session known by name.
func (s *SQLiteStore) yieldKeptNamed(ctx context.Context, name string) {
	s.mu.Lock()
	none := len(s.kept) == 0
	s.mu.Unlock()
	if none {
		return
	}
	var id []byte
	s.dbMu.RLock()
	err := s.look(ctx, func(db queryer) (err error) {
		id, err = sessionIDByName(ctx, db, name)
		return err
	})
	s.dbMu.RUnlock()
	if err == nil {
		s.yieldKept(ctx, id)
	}
}

// releaseKept gives up the kept leases that match, or all of them.
func (s *SQLiteStore) releaseKept(ctx context.Context, match func(*keptLease) bool) error {
	var leases []lease
	s.mu.Lock()
	for id, kept := range s.kept {
		if match == nil || match(kept) {
			delete(s.kept, id)
			leases = append(leases, kept.lease)
		}
	}
	s.mu.Unlock()
	return s.release(ctx, leases...)
}

// release gives up leases no handle uses any longer. The lease is what lets
// a checkpoint write a session's pending updates, so they are written before
// it goes: those of a family the session roots by a checkpoint, and the
// session's own here. A session created for a turn it never had goes with
// its lease.
func (s *SQLiteStore) release(ctx context.Context, leases ...lease) error {
	var result error
	for len(leases) > 0 {
		batch := leases[:min(len(leases), releaseBatch)]
		leases = leases[len(batch):]
		var err error
		for _, held := range batch {
			if err = s.writer.settleFamily(ctx, held.id); err != nil {
				break
			}
		}
		deleted := false
		if err == nil {
			// The leases stay with a failure and so does what was pending,
			// so a checkpoint can still write it.
			var ended []pendingPart
			err = s.withTransactionThen(ctx, "BEGIN IMMEDIATE", func(conn *sql.Conn) error {
				for _, held := range batch {
					pending, err := s.writer.pendingFor(held.id, held.token)
					if err != nil {
						return err
					}
					gone, err := endLease(ctx, conn, held, pending)
					if err != nil {
						return err
					}
					deleted = deleted || gone
					ended = append(ended, pending...)
				}
				return nil
			}, func() { s.writer.wrote(ended) })
		}
		if err == nil && deleted {
			s.incrementalVacuum(ctx)
		}
		result = errors.Join(result, err)
	}
	return result
}

// endLease writes what is pending for the session and ends the lease held on
// it, if that is still the stored one. It reports whether the session
// was deleted instead: an automatic one that never had a turn.
func endLease(ctx context.Context, conn *sql.Conn, held lease, pending []pendingPart) (deleted bool, err error) {
	var retention string
	var hasTurn, owned int
	err = conn.QueryRowContext(ctx, `
		SELECT sessions.retention,sessions.has_turn,
		       COALESCE(session_leases.owner_token = ?, 0)
		FROM sessions
		LEFT JOIN session_leases ON session_leases.session_id = sessions.id
		WHERE sessions.id = ?`, held.token, held.id).Scan(&retention, &hasTurn, &owned)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	messages := 0
	for i := range pending {
		messages += len(pending[i].session.tail)
	}
	if err != nil || owned == 0 {
		if err == nil && messages > 0 {
			// Someone else has the session now and has gone on from what
			// the database held.
			slog.Warn("coordination_updates_dropped", "session", hex.EncodeToString(held.id), "messages", messages)
		}
		return false, err
	}
	for i := range pending {
		if err := writePendingSession(ctx, conn, &pending[i].session, false); err != nil {
			return false, err
		}
	}
	if messages > 0 {
		hasTurn = 1
	}
	if retention == retentionAuto && hasTurn == 0 {
		deleted, err = deleteSessions(ctx, conn, "id = ? AND NOT "+swarmPinnedSQL, held.id)
		if err != nil || deleted {
			return deleted, err
		}
	}
	_, err = conn.ExecContext(ctx,
		"DELETE FROM session_leases WHERE session_id = ? AND owner_token = ?", held.id, held.token)
	return false, err
}

// leaseLoop renews the store's leases once per heartbeat.
func (s *SQLiteStore) leaseLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(leaseHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.renewLeases()
		}
	}
}

// renewLeases extends every lease the store holds, under a handle or kept,
// in one transaction, and gives up the kept ones whose time is over or whose
// family the store no longer drives.
func (s *SQLiteStore) renewLeases() {
	now := time.Now()
	var held []lease
	var done []lease
	s.mu.Lock()
	handles := make([]*sqliteSession, 0, len(s.open))
	for session := range s.open {
		if !session.closed.Load() {
			handles = append(handles, session)
			held = append(held, lease{id: session.id, token: session.ownerToken})
		}
	}
	for id, kept := range s.kept {
		if root := s.byID[kept.root]; now.Sub(kept.since) >= leaseKeep || root == nil || root.closed.Load() {
			delete(s.kept, id)
			done = append(done, kept.lease)
			continue
		}
		held = append(held, kept.lease)
	}
	s.mu.Unlock()

	if len(held) > 0 {
		ctx, cancel := context.WithTimeout(s.ctx, leaseHeartbeatInterval)
		owned := make([]bool, len(held))
		var expiresNS int64
		err := s.withTransactionNow(ctx, "BEGIN IMMEDIATE", func(conn *sql.Conn) error {
			// Taken once the transaction has the database, so the stored
			// expiry is not short by the wait for it.
			renewed, expires := leaseWindow()
			expiresNS = expires
			for i, one := range held {
				var err error
				if owned[i], err = extendLease(ctx, conn, one.id, one.token, renewed.UnixNano(), expires); err != nil {
					return err
				}
			}
			return nil
		})
		cancel()
		// Waiting for the store's sole pooled connection can outlive the
		// stale threshold during a legitimate long transaction. Only an
		// owner-token mismatch proves that this process lost a lease.
		if err == nil {
			for i, session := range handles {
				if owned[i] {
					session.leaseExpiry.Store(expiresNS)
				} else {
					session.cancel(ErrSessionLeaseLost)
				}
			}
			s.mu.Lock()
			for i, one := range held[len(handles):] {
				kept := s.kept[string(one.id)]
				if kept == nil || !bytes.Equal(kept.token, one.token) {
					continue
				}
				if owned[len(handles)+i] {
					kept.expiry = expiresNS
				} else {
					delete(s.kept, string(one.id))
				}
			}
			s.mu.Unlock()
		}
	}
	if len(done) > 0 {
		// Not the store's context: a lease taken out of the pool is
		// released even as the store closes.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = s.release(ctx, done...)
		cancel()
	}
}
