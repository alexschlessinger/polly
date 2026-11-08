package sessions

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/alexschlessinger/pollytool/internal/coordindex"
	"github.com/alexschlessinger/pollytool/internal/coordsem"
)

func (s statusStore) firstItem(section, actor string) (*coordsem.DecisionItem, error) {
	var value []byte
	nativeSQL(s.ctx)
	err := s.conn.QueryRowContext(s.ctx, `SELECT value FROM swarm_status_items WHERE parent_id=? AND section=? AND actor=? ORDER BY sort_key LIMIT 1`, s.parent, section, actor).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	nativeRows(s.ctx, 1)
	nativePayload(s.ctx, value)
	var item coordsem.DecisionItem
	err = json.Unmarshal(value, &item)
	return &item, err
}
func (s statusStore) syncBudget() error {
	item, err := s.firstItem("budgets", "")
	if err != nil {
		return err
	}
	var f statusFact
	if item != nil {
		f.decision("decisions", "", 3, coordindex.Order{Kind: "status", ID: item.ID}, *item)
	}
	return s.save(RecordKey{"status", "budget"}, f)
}
func (r *sqliteRecordTx) statusStore() statusStore {
	return statusStore{sqliteDerivedStore{ctx: r.ctx, conn: r.conn, parent: r.parent, stamp: r.stamp, displayScalars: r.displayScalars}}
}
func (r *sqliteRecordTx) SemanticStatus(actor string, includeNext bool) (out SemanticStatusHeader, err error) {
	if _, err = r.semanticToken(); err != nil {
		return out, err
	}
	return r.semanticStatusHeader(actor, includeNext)
}

func (r *sqliteRecordTx) semanticStatusHeader(actor string, includeNext bool) (out SemanticStatusHeader, err error) {
	s := r.statusStore()
	source := s.source()
	out.Run, err = s.run()
	if err != nil {
		return out, err
	}
	runs, err := s.sourceCount(coordindex.Kind, "run")
	if err != nil {
		return out, err
	}
	out.HasRuns = runs > 0
	out.WorkRunning, err = s.running()
	if err != nil {
		return out, err
	}
	cold, err := s.total("cold", "")
	if err != nil {
		return out, err
	}
	out.ColdCompatibility = cold > 0
	get := func(name, key string, target *int) error { n, err := s.total(name, key); *target = n; return err }
	if err = get("done", out.Run, &out.Counts.Done); err != nil {
		return out, err
	}
	repaired, err := s.total("repaired", "")
	if err != nil {
		return out, err
	}
	out.Counts.Done += repaired
	for name, target := range map[string]*int{"deferred": &out.Counts.Deferred, "delivering": &out.Counts.Delivering, "dormant": &out.Counts.Dormant, "deliveries": &out.Deliveries} {
		if err = get(name, "", target); err != nil {
			return out, err
		}
	}
	out.Counts.Retained, err = s.sourceCount(coordindex.ContextRelease, "retained")
	if err != nil {
		return out, err
	}
	out.Counts.Publications, err = s.sourceCount(coordindex.PublicationSurvivor, coordindex.Key("machine"))
	if err != nil {
		return out, err
	}
	runPubs, err := s.sourceCount(coordindex.PublicationSurvivor, coordindex.Key("run", out.Run))
	if err != nil {
		return out, err
	}
	out.Counts.Publications += runPubs
	disp := coordsem.Disposition{HasRuns: out.HasRuns}
	for name, target := range map[string]*bool{"review": &disp.Review, "integration": &disp.Integration, "halted": &disp.Halted, "open": &disp.Open, "delivering": &disp.Delivering} {
		n, err := s.total("disposition-"+name, "")
		if err != nil {
			return out, err
		}
		*target = n > 0
	}
	out.ParentDisposition = disp.Detail()
	if actor == r.identity.ParentID {
		if err = get("decisions", "", &out.Counts.NeedsDecision); err != nil {
			return out, err
		}
		if err = get("working", "", &out.Counts.Working); err != nil {
			return out, err
		}
		if out.Run != "" {
			run, err := semanticGet[struct{ Starts, Limit int }](source, "run", out.Run)
			if err != nil {
				return out, err
			}
			if run != nil {
				out.Budget = &SemanticBudget{Unit: "starts", Used: run.Starts, Limit: run.Limit, Exhausted: run.Starts >= run.Limit}
			}
		}
		if !includeNext {
			return out, nil
		}
		first, err := s.firstItem("decisions", "")
		if err != nil {
			return out, err
		}
		var refs []coordsem.TaskReference
		if first != nil && first.Kind == "task" && first.Why == "candidate ready" {
			// Exact batch Next intentionally returns every candidate-ready ref.
			q := SemanticStatusQuery{Actor: actor, Section: "ready", Limit: 128}
			for {
				p, err := r.statusPage(q, true)
				if err != nil {
					return out, err
				}
				for _, item := range p.Decisions {
					task, err := semanticGet[coordsem.Task](source, "task", item.ID)
					if err != nil {
						return out, err
					}
					if task == nil {
						return out, fmt.Errorf("status reference without task")
					}
					refs = append(refs, coordsem.TaskReference{Task: task.ID, Revision: task.Revision})
				}
				if p.Next == "" {
					break
				}
				q.After = p.Next
			}
		}
		out.Next = coordsem.ParentNext(first, refs, out.Deliveries, out.Counts.Working, out.HasRuns)
	} else {
		if err = get("requests", actor, &out.Counts.NeedsDecision); err != nil {
			return out, err
		}
		if err = get("teammates", "", &out.Counts.Working); err != nil {
			return out, err
		}
		m, p, err := source.memberFacts(actor)
		if err != nil {
			return out, err
		}
		if p.Busy {
			out.Counts.Working--
		}
		out.Next = "Continue your task and return the final result; successful work is captured and submitted automatically. Use wait_agent to park until addressed input."
		if m != nil {
			f, err := source.taskBase(m.Task)
			if err != nil {
				return out, err
			}
			requirement := ""
			if f.task != nil {
				requirement = coordsem.Requirement(&f.task.Task, f.owner != nil, f.owner != nil && f.owner.ReadOnly)
			}
			out.Next = coordsem.CompletionGuidance(requirement) + " Use wait_agent to park until addressed input."
			e, err := source.execution(m.Execution)
			if err != nil {
				return out, err
			}
			if e != nil {
				out.Budget = &SemanticBudget{Unit: "iterations", Used: e.Iterations, Limit: e.MaxIterations, Exhausted: e.MaxIterations > 0 && e.Iterations >= e.MaxIterations}
				raw, err := semanticGet[struct {
					Request struct{ Schema json.RawMessage }
				}](source, "execution", m.Execution)
				if err != nil {
					return out, err
				}
				if raw != nil && len(raw.Request.Schema) > 0 && string(raw.Request.Schema) != "null" {
					out.Next = "Finish with swarm_complete({value: ...}) matching your assigned schema; pass the value itself, not a JSON string. Use wait_agent to park until addressed input."
				}
			}
		}
		if out.Counts.NeedsDecision > 0 {
			out.Next = "Reply to the requests above, then continue your task."
		}
	}
	return out, nil
}

type statusCursor struct {
	Root, Actor, Section string
	Version              int
	Order                []byte
}

func (r *sqliteRecordTx) SemanticStatusPage(q SemanticStatusQuery) (SemanticStatusPage, error) {
	return r.statusPage(q, false)
}
func (r *sqliteRecordTx) statusPage(q SemanticStatusQuery, internal bool) (out SemanticStatusPage, err error) {
	if _, err = r.semanticToken(); err != nil {
		return out, err
	}
	if q.Limit <= 0 || q.Limit == int(^uint(0)>>1) || q.Offset < 0 || q.MaxBytes < 0 || q.After != "" && q.Offset != 0 || q.Section != "decisions" && q.Section != "working" && !(internal && q.Section == "ready") {
		return out, fmt.Errorf("%w: semantic status page", ErrCoordinationRequest)
	}
	section, actor := q.Section, ""
	if q.Actor != r.identity.ParentID {
		if section == "decisions" {
			section, actor = "requests", q.Actor
		} else {
			section = "teammates"
		}
	}
	s := r.statusStore()
	out.Total, err = s.total(section, actor)
	if err != nil {
		return out, err
	}
	where := `parent_id=? AND section=? AND actor=?`
	args := []any{r.parent, section, actor}
	if section == "teammates" {
		where += ` AND id<>?`
		args = append(args, q.Actor)
		_, self, err := s.source().memberFacts(q.Actor)
		if err != nil {
			return out, err
		}
		if self.Busy {
			out.Total--
		}
	}
	if q.After != "" {
		var cursor statusCursor
		if err := semanticDecode(q.After, &cursor); err != nil {
			return out, err
		}
		if cursor.Root != r.identity.ParentID || cursor.Actor != q.Actor || cursor.Section != q.Section || cursor.Version != coordsem.Version || len(cursor.Order) == 0 {
			return out, ErrCoordinationCursor
		}
		where += ` AND sort_key>?`
		args = append(args, cursor.Order)
	}
	args = append(args, q.Limit+1, q.Offset)
	// Select ordering keys and byte lengths before hydrating any value. Even
	// the lookahead row cannot force an unrelated oversized payload read.
	var orders [][]byte
	used, more := 0, false
	stop := errors.New("status page selected")
	nativeSQL(r.ctx)
	err = queryEach(r.ctx, r.conn, `SELECT sort_key,length(value) FROM swarm_status_items WHERE `+where+` ORDER BY sort_key LIMIT ? OFFSET ?`, args, func(rows *sql.Rows) error {
		var order []byte
		var size int
		if err := rows.Scan(&order, &size); err != nil {
			return err
		}
		nativeRows(r.ctx, 1)
		if len(orders) == q.Limit || len(orders) > 0 && q.MaxBytes > 0 && size > q.MaxBytes-used {
			more = true
			return stop
		}
		if len(orders) == 0 && q.MaxBytes > 0 && size > q.MaxBytes {
			out.Oversized = true
		}
		orders = append(orders, order)
		used += size
		return nil
	})
	if err != nil && !errors.Is(err, stop) {
		return out, err
	}
	for _, order := range orders {
		var payload []byte
		nativeSQL(r.ctx)
		if err := r.conn.QueryRowContext(r.ctx, `SELECT value FROM swarm_status_items WHERE parent_id=? AND section=? AND actor=? AND sort_key=?`, r.parent, section, actor, order).Scan(&payload); err != nil {
			return out, err
		}
		nativeRows(r.ctx, 1)
		nativePayload(r.ctx, payload)
		if q.Section == "working" {
			var value coordsem.WorkingItem
			if err = json.Unmarshal(payload, &value); err != nil {
				return out, err
			}
			out.Working = append(out.Working, value)
		} else {
			var value coordsem.DecisionItem
			if err = json.Unmarshal(payload, &value); err != nil {
				return out, err
			}
			out.Decisions = append(out.Decisions, value)
		}
	}
	if more {
		out.Next = semanticEncode(statusCursor{r.identity.ParentID, q.Actor, q.Section, coordsem.Version, orders[len(orders)-1]})
	}
	return out, nil
}
