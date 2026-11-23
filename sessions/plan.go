package sessions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// PlanSession is the optional capability of a leased session that owns one
// plan. No operation starts work. Only a true result from AcceptPlan authorizes
// the accepting caller to start a new implementation turn.
type PlanSession interface {
	GetPlan(context.Context) (*Plan, error) // nil, nil when there is no plan
	StartPlan(ctx context.Context, path, base string) error
	ExitPlan(context.Context) error
	AcceptPlan(ctx context.Context, reviewedRevision, currentText string) (bool, error)
}

// Plan is a detached, consistent read of the plan and its progress. Path and
// Base identify the editable draft, but are never read to restore an accepted
// plan. Revision and Snapshot are set only at acceptance.
type Plan struct {
	Mode               PlanMode
	Path, Base         string
	Revision, Snapshot string
	Todos              []PlanTodo // Only initialized plan-linked items, in step order.
}

type PlanMode string

const (
	PlanDraft    PlanMode = "draft"
	PlanExited   PlanMode = "exited"
	PlanAccepted PlanMode = "accepted"
)

var ErrPlanRevision = errors.New("plan revision changed; review the current plan")

// Use a versioned migration: an older binary that does not enforce planning
// permissions must not silently resume a draft.
func applySchemaV10(ctx context.Context, conn *sql.Conn) error {
	return execAll(ctx, conn,
		`CREATE TABLE IF NOT EXISTS session_plans (
			session_id BLOB PRIMARY KEY NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
			mode TEXT NOT NULL CHECK(mode IN ('draft','exited','accepted')),
			path TEXT NOT NULL CHECK(length(path)>0), base TEXT NOT NULL,
			revision TEXT NOT NULL, snapshot TEXT NOT NULL,
			CHECK((mode='accepted' AND length(revision)=64 AND length(snapshot)>0) OR
			      (mode!='accepted' AND revision='' AND snapshot=''))
		) STRICT, WITHOUT ROWID`,
		`CREATE TABLE IF NOT EXISTS plan_todos (
			session_id BLOB NOT NULL REFERENCES session_plans(session_id) ON DELETE CASCADE,
			number INTEGER NOT NULL CHECK(number>0), text TEXT NOT NULL CHECK(length(text)>0),
			state TEXT NOT NULL CHECK(state IN ('pending','active','blocked','done')),
			reason TEXT NOT NULL CHECK((state='blocked')=(length(reason)>0)),
			PRIMARY KEY(session_id,number)
		) STRICT, WITHOUT ROWID`,
	)
}

func (s *sqliteSession) GetPlan(ctx context.Context) (*Plan, error) {
	var out *Plan
	err := s.leasedRead(ctx, func(ctx context.Context, conn *sql.Conn) error {
		var plan Plan
		err := conn.QueryRowContext(ctx, `SELECT mode,path,base,revision,snapshot FROM session_plans WHERE session_id=?`, s.id).
			Scan(&plan.Mode, &plan.Path, &plan.Base, &plan.Revision, &plan.Snapshot)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		out = &plan
		if plan.Mode != PlanAccepted {
			return nil
		}
		plan.Todos, err = s.readTodos(ctx, conn, true)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Plan and todo mutations participate in the normal session picker ordering and TTL.
func (s *sqliteSession) touchSession(ctx context.Context, conn *sql.Conn) error {
	_, err := conn.ExecContext(ctx, "UPDATE sessions SET "+touchSQL+" WHERE id=?", time.Now().UnixNano(), s.id)
	return err
}

// StartPlan enters or resumes planning; it cannot redirect an existing draft
// or replace an accepted plan. The store treats base as opaque path metadata.
func (s *sqliteSession) StartPlan(ctx context.Context, path, base string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("plan draft path is required")
	}
	return s.leased(ctx, true, func(ctx context.Context, conn *sql.Conn) error {
		res, err := conn.ExecContext(ctx, `INSERT INTO session_plans(session_id,mode,path,base,revision,snapshot) VALUES(?,'draft',?,?,'','')
			ON CONFLICT(session_id) DO UPDATE SET mode='draft' WHERE mode!='accepted' AND path=excluded.path AND base=excluded.base`, s.id, path, base)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("cannot replace an existing plan; use its original draft or start a new session")
		}
		return s.touchSession(ctx, conn)
	})
}

func (s *sqliteSession) ExitPlan(ctx context.Context) error {
	return s.leased(ctx, true, func(ctx context.Context, conn *sql.Conn) error {
		res, err := conn.ExecContext(ctx, `UPDATE session_plans SET mode='exited' WHERE session_id=? AND mode IN ('draft','exited')`, s.id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("no draft to exit")
		}
		return s.touchSession(ctx, conn)
	})
}

// AcceptPlan compares the exact, freshly read file bytes to the review token.
// It freezes only the snapshot; InitPlanTodos separately materializes its steps.
// Retrying the same review returns false, nil even if progress has since changed.
func (s *sqliteSession) AcceptPlan(ctx context.Context, reviewedRevision, currentText string) (bool, error) {
	if PlanRevision(currentText) != reviewedRevision {
		return false, ErrPlanRevision
	}
	if _, err := ParsePlanSteps(currentText); err != nil {
		return false, err
	}
	accepted := false
	err := s.leased(ctx, true, func(ctx context.Context, conn *sql.Conn) error {
		var mode PlanMode
		var revision string
		err := conn.QueryRowContext(ctx, `SELECT mode,revision FROM session_plans WHERE session_id=?`, s.id).Scan(&mode, &revision)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("no plan to accept")
		}
		if err != nil {
			return err
		}
		switch mode {
		case PlanAccepted:
			if revision != reviewedRevision {
				return ErrPlanRevision
			}
			return nil
		case PlanDraft:
		default:
			return fmt.Errorf("resume the draft before accepting it")
		}
		if _, err := conn.ExecContext(ctx, `UPDATE session_plans SET mode='accepted',revision=?,snapshot=? WHERE session_id=?`, reviewedRevision, currentText, s.id); err != nil {
			return err
		}
		accepted = true
		return s.touchSession(ctx, conn)
	})
	return accepted && err == nil, err
}
