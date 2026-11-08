package sessions

import (
	"fmt"

	"github.com/alexschlessinger/pollytool/internal/coordsem"
)

// CoordinationInspectorFacts names observation-only display facts for one
// selected record. Values cannot authorize task acceptance or workflow control.
type CoordinationInspectorFacts struct {
	Display                        string
	Deferred                       bool
	Active, Pending, DeferredItems int
}
type CoordinationInspectorReader interface {
	InspectorFacts(RecordKey) (CoordinationInspectorFacts, error)
}

func (r sqliteSemanticReader) InspectorFacts(key RecordKey) (CoordinationInspectorFacts, error) {
	return r.CoordinationReader.(CoordinationInspectorReader).InspectorFacts(key)
}
func (r *sqliteRecordTx) InspectorFacts(key RecordKey) (out CoordinationInspectorFacts, err error) {
	if err = r.ready(); err != nil {
		return out, err
	}
	if err = r.checkKind(key.Kind); err != nil {
		return out, err
	}
	s := r.statusStore()
	source := s.source()
	switch key.Kind {
	case "task":
		f, err := source.taskBase(key.ID)
		if err != nil || f.task == nil {
			return out, err
		}
		conflicted := false
		if f.task.Status == "awaiting_review" && f.editing && !f.delivering {
			c, err := source.candidate(f.task)
			if err != nil {
				return out, err
			}
			conflicted = c != nil && c.Status == "conflicted"
		}
		out.Display = coordsem.TaskDisplay(&f.task.Task, f.delivering, f.editing, conflicted)
		out.Deferred = f.deferred
	case "member":
		_, f, err := source.memberFacts(key.ID)
		if err != nil {
			return out, err
		}
		out.Display = f.Display
	case "run":
		out.Pending, err = s.total("run-open", key.ID)
		if err != nil {
			return out, err
		}
		out.Active, err = s.total("inspector-run-active", key.ID)
	case "workflow":
		out.DeferredItems, err = s.total("inspector-workflow-deferred", key.ID)
	default:
		return out, fmt.Errorf("%w: inspector facts kind %q", ErrCoordinationRequest, key.Kind)
	}
	return out, err
}
