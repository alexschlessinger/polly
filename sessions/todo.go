package sessions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// TodoSession is the optional capability of a leased session that owns a todo
// list. Ordinary items need no plan. Only InitPlanTodos creates linked items.
type TodoSession interface {
	GetTodos(context.Context) ([]PlanTodo, error)
	AddTodo(ctx context.Context, text string) error
	RenameTodo(ctx context.Context, number int, text string) error
	SetTodoState(ctx context.Context, number int, state TodoState, reason string) error
	InitPlanTodos(ctx context.Context, revision string) error
}

type PlanTodo struct {
	Number   int // Stable one-based ID within this session.
	Text     string
	State    TodoState
	Reason   string // Nonempty only while blocked.
	PlanStep int    // One-based step in the frozen snapshot; zero for ordinary items.
}

type TodoState string

const (
	TodoPending TodoState = "pending"
	TodoActive  TodoState = "active"
	TodoBlocked TodoState = "blocked"
	TodoDone    TodoState = "done"
)

// In v10 a todo's number was both its ID and its ordinal in the plan. Keep
// both identities (and progress) across the move to session ownership.
func applySchemaV11(ctx context.Context, conn *sql.Conn) error {
	return execAll(ctx, conn,
		`CREATE TABLE IF NOT EXISTS session_todos (
			session_id BLOB NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
			number INTEGER NOT NULL CHECK(number>0), text TEXT NOT NULL CHECK(length(text)>0),
			state TEXT NOT NULL CHECK(state IN ('pending','active','blocked','done')),
			reason TEXT NOT NULL CHECK((state='blocked')=(length(reason)>0)),
			plan_step INTEGER NOT NULL CHECK(plan_step>=0),
			PRIMARY KEY(session_id,number)
		) STRICT, WITHOUT ROWID`,
		`CREATE UNIQUE INDEX IF NOT EXISTS session_todos_step_idx ON session_todos(session_id,plan_step) WHERE plan_step>0`,
		`CREATE UNIQUE INDEX IF NOT EXISTS session_todos_title_idx ON session_todos(session_id,text) WHERE plan_step=0`,
		`INSERT INTO session_todos(session_id,number,text,state,reason,plan_step)
		 SELECT session_id,number,text,state,reason,number FROM plan_todos`,
		`DROP TABLE plan_todos`,
	)
}

func (s *sqliteSession) readTodos(ctx context.Context, conn *sql.Conn, linkedOnly bool) ([]PlanTodo, error) {
	query := `SELECT number,text,state,reason,plan_step FROM session_todos WHERE session_id=?`
	if linkedOnly {
		query += ` AND plan_step>0 ORDER BY plan_step`
	} else {
		query += ` ORDER BY number`
	}
	var todos []PlanTodo
	err := queryEach(ctx, conn, query, []any{s.id}, func(rows *sql.Rows) error {
		var todo PlanTodo
		if err := rows.Scan(&todo.Number, &todo.Text, &todo.State, &todo.Reason, &todo.PlanStep); err != nil {
			return err
		}
		todos = append(todos, todo)
		return nil
	})
	return todos, err
}

func (s *sqliteSession) GetTodos(ctx context.Context) ([]PlanTodo, error) {
	var todos []PlanTodo
	err := s.leasedRead(ctx, func(ctx context.Context, conn *sql.Conn) error {
		var err error
		todos, err = s.readTodos(ctx, conn, false)
		return err
	})
	if err != nil {
		return nil, err
	}
	return todos, nil
}

func todoTitle(text string) (string, error) {
	const maxTitleBytes = 4096
	text = strings.TrimSpace(text)
	if text == "" || !utf8.ValidString(text) || strings.ContainsFunc(text, unicode.IsControl) {
		return "", fmt.Errorf("todo title must be a nonempty line of text without control characters")
	}
	if len(text) > maxTitleBytes {
		return "", fmt.Errorf("todo title exceeds %d bytes", maxTitleBytes)
	}
	return text, nil
}

// Numbers are never reassigned: items can be renamed or completed but not
// removed. Allocation and insertion share the leased write transaction.
const insertTodoSQL = `INSERT INTO session_todos(session_id,number,text,state,reason,plan_step)
	SELECT ?,COALESCE(MAX(number),0)+1,?,'pending','',? FROM session_todos WHERE session_id=?`

func (s *sqliteSession) AddTodo(ctx context.Context, text string) error {
	text, err := todoTitle(text)
	if err != nil {
		return err
	}
	return s.leased(ctx, true, func(ctx context.Context, conn *sql.Conn) error {
		res, err := conn.ExecContext(ctx, insertTodoSQL+` ON CONFLICT(session_id,text) WHERE plan_step=0 DO NOTHING`, s.id, text, 0, s.id)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return err // A retry is not a new session mutation.
		}
		return s.touchSession(ctx, conn)
	})
}

func (s *sqliteSession) RenameTodo(ctx context.Context, number int, text string) error {
	text, err := todoTitle(text)
	if err != nil {
		return err
	}
	return s.leased(ctx, true, func(ctx context.Context, conn *sql.Conn) error {
		res, err := conn.ExecContext(ctx, `UPDATE session_todos SET text=? WHERE session_id=? AND number=? AND plan_step=0`, text, s.id, number)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("no ordinary todo %d to rename", number)
		}
		return s.touchSession(ctx, conn)
	})
}

// SetTodoState changes only progress. Items become active before they can be
// completed or blocked; resuming a blocked item clears its reason. Done is terminal.
func (s *sqliteSession) SetTodoState(ctx context.Context, number int, state TodoState, reason string) error {
	reason = strings.TrimSpace(reason)
	if (state == TodoBlocked) != (reason != "") || !validPlanText(reason) {
		return fmt.Errorf("provide a nonempty text reason only for blocked todos")
	}
	return s.leased(ctx, true, func(ctx context.Context, conn *sql.Conn) error {
		var old TodoState
		err := conn.QueryRowContext(ctx, `SELECT state FROM session_todos WHERE session_id=? AND number=?`, s.id, number).Scan(&old)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("no todo %d", number)
		}
		if err != nil {
			return err
		}
		canStart := state == TodoActive && (old == TodoPending || old == TodoBlocked)
		canReport := old == TodoActive && (state == TodoDone || state == TodoBlocked)
		if old != state && !canStart && !canReport {
			return fmt.Errorf("todo %d cannot go from %s to %s", number, old, state)
		}
		if _, err := conn.ExecContext(ctx, `UPDATE session_todos SET state=?,reason=? WHERE session_id=? AND number=?`, state, reason, s.id, number); err != nil {
			return err
		}
		return s.touchSession(ctx, conn)
	})
}

// InitPlanTodos reads only the reviewed, frozen bytes. A retry inserts no
// duplicates and never changes progress, including progress migrated from v10.
func (s *sqliteSession) InitPlanTodos(ctx context.Context, revision string) error {
	return s.leased(ctx, true, func(ctx context.Context, conn *sql.Conn) error {
		var actualRevision, snapshot string
		err := conn.QueryRowContext(ctx, `SELECT revision,snapshot FROM session_plans WHERE session_id=? AND mode='accepted'`, s.id).Scan(&actualRevision, &snapshot)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("no accepted plan to initialize")
		}
		if err != nil {
			return err
		}
		if revision != actualRevision {
			return ErrPlanRevision
		}
		steps, err := ParsePlanSteps(snapshot)
		if err != nil {
			return err
		}
		linked, err := s.readTodos(ctx, conn, true)
		if err != nil {
			return err
		}
		present := make([]bool, len(steps))
		for _, todo := range linked {
			i := todo.PlanStep - 1
			if i >= len(steps) || todo.Text != steps[i] {
				return fmt.Errorf("todo %d does not match frozen plan step %d", todo.Number, todo.PlanStep)
			}
			present[i] = true
		}
		if len(linked) == len(steps) {
			return nil
		}
		for i, step := range steps {
			if present[i] {
				continue
			}
			if _, err := conn.ExecContext(ctx, insertTodoSQL, s.id, step, i+1, s.id); err != nil {
				return err
			}
		}
		return s.touchSession(ctx, conn)
	})
}
