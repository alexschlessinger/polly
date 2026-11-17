package sessions

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// SpawnOutcomeSession is optional. It records Metadata.SpawnOutcome without
// reading or rewriting the rest of the session's settings.
type SpawnOutcomeSession interface {
	// RecordSpawnOutcome notes how the session's first run ended. A session
	// that has an outcome keeps it, and is not touched.
	RecordSpawnOutcome(context.Context, ReportStatus) error
}

func (s *sqliteSession) RecordSpawnOutcome(ctx context.Context, outcome ReportStatus) error {
	switch outcome {
	case ReportFinished, ReportCanceled, ReportFailed, ReportPaused:
	default:
		return fmt.Errorf("unknown spawn outcome %q", outcome)
	}
	nowNS := time.Now().UnixNano()
	queued, err := s.queueSpawnOutcome(ctx, outcome, nowNS)
	if queued || err != nil {
		return err
	}
	return s.leased(ctx, true, func(ctx context.Context, conn *sql.Conn) error {
		return writeSpawnOutcome(ctx, conn, s.id, outcome, nowNS)
	})
}

// queueSpawnOutcome hands the outcome to the writer when this store drives
// the session's family, and reports whether it did. The outcome is then in
// the update log by the time a later update of the family is, and in the
// database at the next checkpoint.
func (s *sqliteSession) queueSpawnOutcome(ctx context.Context, outcome ReportStatus, nowNS int64) (bool, error) {
	cache := s.store.coordination
	if err := cache.hold(ctx); err != nil {
		return false, err
	}
	defer cache.release()
	_, root, ok := s.remembered(ctx, true)
	if !ok {
		return false, nil
	}
	opCtx, cleanup, err := s.operationContext(ctx)
	if err != nil {
		return false, err
	}
	defer cleanup()
	_, _, err = s.store.writer.accept(opCtx, &queuedUpdate{
		parent: s.coord.parent, parentToken: root.ownerToken,
		session: s.id, sessionToken: s.ownerToken, sequence: s.coord.sequence,
		outcome: outcome, nowNS: nowNS,
	})
	return true, s.mapError(ctx, err)
}

// writeSpawnOutcome sets the outcome of a session that has none. Like the
// transcript binding it is one key of the stored settings, replaced where it
// sits.
func writeSpawnOutcome(ctx context.Context, conn *sql.Conn, id []byte, outcome ReportStatus, nowNS int64) error {
	_, err := conn.ExecContext(ctx, `
		UPDATE sessions
		SET settings_json = CAST(json_set(CAST(settings_json AS TEXT), '$.spawnOutcome', ?) AS BLOB), `+touchSQL+`
		WHERE id = ? AND coalesce(json_extract(CAST(settings_json AS TEXT), '$.spawnOutcome'), '') = ''`,
		string(outcome), nowNS, id)
	return err
}
