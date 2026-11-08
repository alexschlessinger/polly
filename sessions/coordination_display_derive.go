package sessions

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/alexschlessinger/pollytool/internal/coordsem"
)

// Decision indexes contain only parent-facing semantic facts. Writer-side
// replacements dirty both former and new owners; readers never scan decisions
// belonging to unrelated retained tasks or mail.
func (s sqliteDerivedStore) displayDecisions(key RecordKey, before, after statusFact) error {
	dirty := func(f statusFact) {
		if s.displayDirty == nil {
			return
		}
		for _, item := range f.Items {
			if item.Section != "decisions" || item.Actor != "" || item.Decision == nil {
				continue
			}
			d := item.Decision
			if (d.Kind == "task" || d.Kind == "mail") && d.Member != "" {
				s.displayDirty[RecordKey{"member", d.Member}] = true
			}
			if d.Kind == "workflow" {
				s.displayDirty[RecordKey{"workflow", d.ID}] = true
			}
		}
	}
	dirty(before)
	dirty(after)
	nativeSQL(s.ctx)
	if _, err := s.conn.ExecContext(s.ctx, `DELETE FROM swarm_display_decisions WHERE parent_id=? AND kind=? AND id=?`, s.parent, key.Kind, key.ID); err != nil {
		return err
	}
	for _, item := range after.Items {
		if item.Section != "decisions" || item.Actor != "" || item.Decision == nil {
			continue
		}
		d := item.Decision
		member, workflow := "", ""
		if d.Kind == "task" || d.Kind == "mail" {
			member = d.Member
		}
		if d.Kind == "workflow" {
			workflow = d.ID
		}
		if member == "" && workflow == "" {
			continue
		}
		value, err := json.Marshal(d)
		if err != nil {
			return err
		}
		nativeSQL(s.ctx)
		if _, err = s.conn.ExecContext(s.ctx, `INSERT INTO swarm_display_decisions(parent_id,kind,id,member,workflow,sort_key,value) VALUES(?,?,?,?,?,?,?)`, s.parent, key.Kind, key.ID, member, workflow, item.Order, value); err != nil {
			return err
		}
	}
	return nil
}
func (s sqliteDerivedStore) displayDecisionCount(column, id string) (int, *SemanticDecision, error) {
	var count int
	nativeSQL(s.ctx)
	if err := s.conn.QueryRowContext(s.ctx, `SELECT count(*) FROM swarm_display_decisions WHERE parent_id=? AND `+column+`=?`, s.parent, id).Scan(&count); err != nil {
		return 0, nil, err
	}
	nativeRows(s.ctx, 1)
	if count == 0 || column == "workflow" {
		return count, nil, nil
	}
	var value []byte
	nativeSQL(s.ctx)
	if err := s.conn.QueryRowContext(s.ctx, `SELECT value FROM swarm_display_decisions WHERE parent_id=? AND member=? AND NOT (kind='mail' AND substr(json_extract(value,'$.label'),1,6)='reply ') ORDER BY sort_key LIMIT 1`, s.parent, id).Scan(&value); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return count, nil, nil
		}
		return 0, nil, err
	}
	nativeRows(s.ctx, 1)
	nativePayload(s.ctx, value)
	var first SemanticDecision
	err := json.Unmarshal(value, &first)
	return count, &first, err
}
func (s sqliteDerivedStore) displayLatestMail(id string) (json.RawMessage, error) {
	var best []byte
	var order []byte
	for _, kind := range []string{"mail", "mail_to"} {
		var value, position []byte
		nativeSQL(s.ctx)
		err := s.conn.QueryRowContext(s.ctx, `SELECT value,sort_key FROM swarm_display_sources WHERE parent_id=? AND kind=? AND subject=? ORDER BY sort_key DESC LIMIT 1`, s.parent, kind, id).Scan(&value, &position)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		nativeRows(s.ctx, 1)
		nativePayload(s.ctx, value)
		if best == nil || string(position) > string(order) {
			best, order = value, position
		}
	}
	return best, nil
}
func (s sqliteDerivedStore) displayMember(id string) error {
	value, err := s.displaySource(RecordKey{"member", id})
	if err != nil {
		return err
	}
	count, first, err := s.displayDecisionCount("member", id)
	if err != nil {
		return err
	}
	if value == nil {
		if count > 0 {
			return s.displaySave(RecordKey{"member", id}, DisplayMember{Decisions: count, First: first, Presentation: json.RawMessage(`{}`)})
		}
		return s.displaySave(RecordKey{"member", id}, nil)
	}
	var m struct{ ID, Task, Execution, Control string }
	if err = json.Unmarshal(value, &m); err != nil {
		return err
	}
	out := DisplayMember{Member: value}
	out.Execution, err = s.displaySource(RecordKey{"execution", m.Execution})
	if err != nil {
		return err
	}
	out.Task, err = s.displaySource(RecordKey{"task", m.Task})
	if err != nil {
		return err
	}
	out.Message, err = s.displayLatestMail(id)
	if err != nil {
		return err
	}
	source := (statusStore{s}).source()
	_, f, err := source.memberFacts(id)
	if err != nil {
		return err
	}
	p := struct {
		coordsem.MemberFacts
		Outcome, Control, StopReason, Workflow string
		Iterations, MaxIterations              int
		RetryPause                             json.RawMessage
	}{MemberFacts: f, Control: m.Control}
	if out.Execution != nil {
		var e struct {
			Status, StopReason string
			Iterations         int
			Request            struct{ MaxIterations int }
			RetryPause         json.RawMessage
		}
		if err = json.Unmarshal(out.Execution, &e); err != nil {
			return err
		}
		p.Outcome, p.StopReason, p.Iterations, p.MaxIterations, p.RetryPause = e.Status, e.StopReason, e.Iterations, e.Request.MaxIterations, e.RetryPause
		p.Workflow, _, err = source.statusExecutionWorkflow(m.Execution)
		if err != nil {
			return err
		}
	}
	out.Presentation, err = json.Marshal(p)
	if err != nil {
		return err
	}
	out.Decisions, out.First = count, first
	return s.displaySave(RecordKey{"member", id}, out)
}
func (s sqliteDerivedStore) displayWorkflow(id string) error {
	value, err := s.displaySource(RecordKey{"workflow", id})
	if err != nil {
		return err
	}
	if value == nil {
		return s.displaySave(RecordKey{"workflow", id}, nil)
	}
	var w map[string]json.RawMessage
	if err = json.Unmarshal(value, &w); err != nil {
		return err
	}
	var steps []json.RawMessage
	nativeSQL(s.ctx)
	err = queryEach(s.ctx, s.conn, `SELECT value FROM swarm_display_sources WHERE parent_id=? AND kind='workflow_step' AND subject=? ORDER BY sort_key`, []any{s.parent, id}, func(rows *sql.Rows) error {
		var v json.RawMessage
		if err := rows.Scan(&v); err != nil {
			return err
		}
		nativeRows(s.ctx, 1)
		nativePayload(s.ctx, v)
		// Host steps have no launch identity and are not displayed in the roster.
		var step struct{ Kind string }
		if err := json.Unmarshal(v, &step); err != nil {
			return err
		}
		if step.Kind == "agent" {
			steps = append(steps, v)
		}
		return nil
	})
	if err != nil {
		return err
	}
	w["steps"], err = json.Marshal(steps)
	if err != nil {
		return err
	}
	out := DisplayWorkflow{}
	out.Workflow, err = json.Marshal(w)
	if err != nil {
		return err
	}
	out.Decisions, _, err = s.displayDecisionCount("workflow", id)
	if err != nil {
		return err
	}
	return s.displaySave(RecordKey{"workflow", id}, out)
}
func (s sqliteDerivedStore) displayLaunch(id string) error {
	value, err := s.displaySource(RecordKey{"execution", id})
	if err != nil {
		return err
	}
	var e struct {
		Member  string
		Request struct{ CallID string }
	}
	if value != nil {
		if err = json.Unmarshal(value, &e); err != nil {
			return err
		}
	}
	if e.Member == "" || e.Request.CallID == "" {
		return s.displaySave(RecordKey{"launch", id}, nil)
	}
	return s.displaySave(RecordKey{"launch", id}, DisplayLaunch{CallID: e.Request.CallID, Member: e.Member})
}
func (s sqliteDerivedStore) refreshDisplay(key RecordKey, before, after []byte, affected map[RecordKey]bool) error {
	if err := s.displaySourceChange(key, after); err != nil {
		return err
	}
	for k := range affected {
		if k.Kind == "member" || k.Kind == "workflow" {
			s.displayDirty[k] = true
		}
	}
	switch key.Kind {
	case "member", "workflow":
		s.displayDirty[key] = true
	case "execution":
		if err := s.displayLaunch(key.ID); err != nil {
			return err
		}
	case "workflow_step":
		id, _, _ := strings.Cut(key.ID, "/")
		s.displayDirty[RecordKey{"workflow", id}] = true
	case "mail":
		for _, value := range [][]byte{before, after} {
			if value == nil {
				continue
			}
			var m struct{ From, To string }
			if err := json.Unmarshal(value, &m); err != nil {
				return err
			}
			for _, id := range []string{m.From, m.To} {
				if id != "" {
					s.displayDirty[RecordKey{"member", id}] = true
				}
			}
		}
	}
	for k := range s.displayDirty {
		switch k.Kind {
		case "member":
			if err := s.displayMember(k.ID); err != nil {
				return err
			}
		case "workflow":
			if err := s.displayWorkflow(k.ID); err != nil {
				return err
			}
		}
	}
	if s.displayDirty[RecordKey{"status", "parent"}] || key.Kind == "parent_turn" || key.Kind == "run" || key.Kind == "format" || key.Kind == "context" || key.Kind == "publication" || key.Kind == "snapshot" {
		return s.displayHeader()
	}
	return nil
}
