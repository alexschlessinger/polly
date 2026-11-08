package sessions

import (
	"encoding/json"

	"github.com/alexschlessinger/pollytool/internal/coordindex"
	"github.com/alexschlessinger/pollytool/internal/coordsem"
)

// semanticSource resolves explicit dependency inputs; the pure rules never see
// a record store or an invented partial swarm.State.
type semanticSource struct {
	store  derivedStore
	parent string
}

func semanticGet[T any](s semanticSource, kind, id string) (*T, error) {
	if id == "" {
		return nil, nil
	}
	b, err := s.store.get(RecordKey{kind, id})
	if err != nil || b == nil {
		return nil, err
	}
	var out T
	if err = json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
func (s semanticSource) execution(id string) (*coordsem.Execution, error) {
	v, err := semanticGet[struct {
		ID, Run, Member, Workflow, Status, StopReason string
		Generation, Iterations                        int
		Request                                       struct{ MaxIterations int }
		RetryPause                                    json.RawMessage
	}](s, "execution", id)
	if err != nil || v == nil {
		return nil, err
	}
	return &coordsem.Execution{ID: v.ID, Run: v.Run, Member: v.Member, Workflow: v.Workflow, Status: v.Status, StopReason: v.StopReason, Generation: v.Generation, Iterations: v.Iterations, MaxIterations: v.Request.MaxIterations, RetryPause: len(v.RetryPause) > 0 && string(v.RetryPause) != "null"}, nil
}
func (s semanticSource) controlled(e *coordsem.Execution) (bool, error) {
	if e == nil || e.Workflow == "" {
		return false, nil
	}
	w, err := semanticGet[coordsem.Workflow](s, "workflow", e.Workflow)
	return coordsem.Controlled(e, w), err
}
func (s semanticSource) deferred(t *coordsem.Task, e *coordsem.Execution) (bool, error) {
	if t == nil || t.Deferral == nil {
		return false, nil
	}
	w, err := semanticGet[coordsem.Workflow](s, "workflow", t.Deferral.Workflow)
	return coordsem.Deferred(t, e, w), err
}
func (s semanticSource) news(key RecordKey) (value []byte, controlled bool, err error) {
	switch key.Kind {
	case "task":
		t, err := semanticGet[coordsem.Task](s, "task", key.ID)
		if err != nil || t == nil {
			return nil, false, err
		}
		e, err := s.execution(t.Execution)
		if err != nil {
			return nil, false, err
		}
		deferred, err := s.deferred(t, e)
		if err != nil {
			return nil, false, err
		}
		controlled, err := s.controlled(e)
		if err != nil {
			return nil, false, err
		}
		// Notice existence is not pending delivery, and ID order is not posting
		// order. Only this task's indexed mail dependencies are candidates.
		var notice string
		pending := false
		d := derivedEngine{store: s.store, parent: s.parent}
		err = d.each(coordindex.OverviewDependency, coordindex.Key("task", key.ID), "mail", func(item derivedItem) error {
			m, err := semanticGet[struct {
				ID, Task, Execution string
				Revision            int
				Delivered           bool
			}](s, "mail", item.key.ID)
			if err != nil {
				return err
			}
			if m != nil && m.Task == t.ID && m.Execution == t.Execution && m.Revision == t.Revision && (notice == "" || item.key.ID < notice) {
				notice, pending = item.key.ID, !m.Delivered
			}
			return nil
		})
		if err != nil {
			return nil, false, err
		}
		value, err := json.Marshal(coordsem.NewsTask(*t, deferred, pending))
		return value, controlled, err
	case "member":
		m, err := semanticGet[coordsem.Member](s, "member", key.ID)
		if err != nil || m == nil {
			return nil, false, err
		}
		e, err := s.execution(m.Execution)
		if err != nil {
			return nil, false, err
		}
		controlled, err := s.controlled(e)
		if err != nil {
			return nil, false, err
		}
		if !controlled && m.Controller != "" {
			w, err := semanticGet[coordsem.Workflow](s, "workflow", m.Controller)
			if err != nil {
				return nil, false, err
			}
			controlled = w != nil && w.Status == "running"
		}
		value, err := json.Marshal(coordsem.NewsMember(*m, e))
		return value, controlled, err
	case "workflow":
		w, err := semanticGet[coordsem.Workflow](s, "workflow", key.ID)
		if err != nil || w == nil {
			return nil, false, err
		}
		value, err := json.Marshal(coordsem.NewsWorkflow(*w))
		return value, false, err
	}
	return nil, false, nil
}

// newsAffected visits old AND new edges. Source record replacement must call it
// before removing old links and after installing new ones. No kind scan is used.
func (s semanticSource) newsAffected(key RecordKey, payload []byte, affected map[RecordKey]bool) error {
	d := derivedEngine{store: s.store, parent: s.parent}
	add := func(item derivedItem) error { affected[item.key] = true; return nil }
	switch key.Kind {
	case "task", "member":
		affected[key] = true
	case "mail":
		if payload != nil {
			var m struct{ Task string }
			if err := json.Unmarshal(payload, &m); err != nil {
				return err
			}
			if m.Task != "" {
				affected[RecordKey{"task", m.Task}] = true
			}
		}
	case "execution":
		for _, kind := range []string{"member", "task"} {
			if err := d.each(coordindex.OverviewDependency, coordindex.Key("execution", key.ID), kind, add); err != nil {
				return err
			}
		}
	case "workflow":
		affected[key] = true
		for _, kind := range []string{"member", "task"} {
			if err := d.each(coordindex.OverviewDependency, coordindex.Key("workflow", key.ID), kind, add); err != nil {
				return err
			}
		}
		return d.each(coordindex.ExecutionWorkflow, key.ID, "execution", func(item derivedItem) error {
			return s.newsAffected(item.key, nil, affected)
		})
	}
	return nil
}
