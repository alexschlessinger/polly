package sessions

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/alexschlessinger/pollytool/internal/coordindex"
	"github.com/alexschlessinger/pollytool/internal/coordsem"
)

type statusMember struct {
	coordsem.Member
	Name, Label string
	ReadOnly    bool
}
type statusTask struct {
	coordsem.Task
	Dependencies []string
	Delivery     *coordsem.Delivery
}
type statusCandidate struct {
	ID, Run, Status, Successor string
	Created                    time.Time
	Inputs, Repairs            []struct {
		Task      string
		Revision  int
		Submitted coordsem.Snapshot
	}
	Merged    coordsem.Snapshot
	Parent    coordsem.Snapshot
	Pending   []json.RawMessage
	Conflicts []struct{ Paths []string }
}
type statusTaskBase struct {
	task                                                 *statusTask
	owner                                                *statusMember
	execution                                            *coordsem.Execution
	deferred, delivering, unchanged, provenance, editing bool
}

func (s semanticSource) taskBase(id string) (f statusTaskBase, err error) {
	f.task, err = semanticGet[statusTask](s, "task", id)
	if err != nil || f.task == nil {
		return f, err
	}
	t := f.task
	f.owner, err = semanticGet[statusMember](s, "member", t.Owner)
	if err != nil {
		return f, err
	}
	f.execution, err = s.execution(t.Execution)
	if err != nil {
		return f, err
	}
	f.deferred, err = s.deferred(&t.Task, f.execution)
	if err != nil {
		return f, err
	}
	readOnly := f.owner != nil && f.owner.ReadOnly
	requirement := coordsem.Requirement(&t.Task, f.owner != nil, readOnly)
	f.editing = coordsem.Editing(&t.Task, f.owner != nil, readOnly)
	e, err := semanticGet[struct{ Result *coordsem.ResultIdentity }](s, "execution", t.Execution)
	if err != nil {
		return f, err
	}
	if e != nil {
		f.delivering = coordsem.Delivering(&t.Task, f.execution, requirement, t.Delivery, e.Result)
	}
	if f.owner != nil && f.owner.ID == t.Owner && !f.owner.ReadOnly && t.Snapshot != "" {
		base, err := semanticGet[coordsem.Snapshot](s, "snapshot", t.StartingSnapshot)
		if err != nil {
			return f, err
		}
		submitted, err := semanticGet[coordsem.Snapshot](s, "snapshot", t.Snapshot)
		if err != nil {
			return f, err
		}
		f.provenance = coordsem.ValidSnapshot(base, t.StartingSnapshot) && coordsem.ValidSnapshot(submitted, t.Snapshot)
		f.unchanged = f.provenance && base.Tree == submitted.Tree
	}
	return f, nil
}
func (s semanticSource) candidate(t *statusTask) (*statusCandidate, error) {
	var newest *statusCandidate
	d := derivedEngine{store: s.store, parent: s.parent}
	err := d.each(coordindex.OverviewDependency, coordindex.Key("task", t.ID), "integration", func(item derivedItem) error {
		c, err := semanticGet[statusCandidate](s, "integration", item.key.ID)
		if err != nil {
			return err
		}
		if c == nil || c.Status != "ready" && c.Status != "conflicted" || c.Successor != "" {
			return nil
		}
		a, err := semanticGet[struct{ Status string }](s, "apply", c.ID)
		if err != nil {
			return err
		}
		if a != nil && a.Status != "not_applied" {
			return nil
		}
		completed, contains := coordsem.CompletedCandidate(c.Status, c.Parent.Tree, c.Merged.Tree, len(c.Pending), len(c.Conflicts)), false
		refs := slices.Concat(c.Inputs, c.Repairs)
		if len(refs) == 0 {
			completed = false
		}
		for _, ref := range refs {
			f, err := s.taskBase(ref.Task)
			if err != nil {
				return err
			}
			if f.task == nil || !coordsem.CandidateTaskValid(&f.task.Task, c.Run, ref.Submitted.ID, ref.Revision, f.unchanged) {
				return nil
			}
			completed = completed && f.task.Status == "done" && coordsem.Accepted(&f.task.Task) && f.unchanged
			contains = contains || ref.Task == t.ID
		}
		if !completed && contains && (newest == nil || c.Created.After(newest.Created) || c.Created.Equal(newest.Created) && c.ID > newest.ID) {
			newest = c
		}
		return nil
	})
	return newest, err
}
func statusCandidateAction(c *statusCandidate) string {
	if c.Status != "conflicted" {
		return coordsem.IntegrateCandidate(c.ID)
	}
	var paths []string
	for _, conflict := range c.Conflicts {
		paths = append(paths, conflict.Paths...)
	}
	sort.Strings(paths)
	paths = slices.Compact(paths)
	return coordsem.IntegrationHalt(c.ID, c.Merged.Commit, paths)
}
func (s semanticSource) memberFacts(id string) (*statusMember, coordsem.MemberFacts, error) {
	m, err := semanticGet[statusMember](s, "member", id)
	if err != nil || m == nil {
		return m, coordsem.MemberFacts{}, err
	}
	e, err := s.execution(m.Execution)
	if err != nil {
		return nil, coordsem.MemberFacts{}, err
	}
	f, err := s.taskBase(m.Task)
	if err != nil {
		return nil, coordsem.MemberFacts{}, err
	}
	var display string
	open := false
	if f.task != nil {
		conflicted := false
		if f.task.Status == "awaiting_review" && f.editing && !f.delivering {
			c, err := s.candidate(f.task)
			if err != nil {
				return nil, coordsem.MemberFacts{}, err
			}
			conflicted = c != nil && c.Status == "conflicted"
		}
		display = coordsem.TaskDisplay(&f.task.Task, f.delivering, f.editing, conflicted)
		open = f.task.Status != "done" && f.task.Status != "canceled"
	}
	return m, coordsem.DescribeMember(m.Control, e, display, open, f.deferred, f.delivering), nil
}
func (s semanticSource) taskDecision(f statusTaskBase, c *statusCandidate, reason string, deps int) (coordsem.DecisionItem, error) {
	in := coordsem.TaskDecisionInputs{Task: f.task.Task, Execution: f.execution, Delivering: f.delivering, Editing: f.editing, Provenance: f.provenance, DependencyReason: reason, UnfinishedDependencies: deps}
	if f.owner != nil {
		in.Owner = &f.owner.Member
	}
	_, owner, err := s.memberFacts(f.task.Owner)
	if err != nil {
		return coordsem.DecisionItem{}, err
	}
	in.OwnerDisplay = owner.Display
	if c != nil {
		in.CandidateID, in.CandidateStatus, in.CandidateAction = c.ID, c.Status, statusCandidateAction(c)
	}
	e, err := semanticGet[struct {
		RetryPause *struct {
			Attempts    int
			Reason      string
			NextRetryAt time.Time
		}
	}](s, "execution", f.task.Execution)
	if err != nil {
		return coordsem.DecisionItem{}, err
	}
	if e != nil && e.RetryPause != nil {
		p := e.RetryPause
		in.RetryDescription = fmt.Sprintf("retry budget exhausted after %d attempts: %s", p.Attempts, p.Reason)
		if !p.NextRetryAt.IsZero() {
			in.RetryDescription += "; eligible for explicit continuation at " + p.NextRetryAt.UTC().Format(time.RFC3339Nano)
		}
		in.RetryDescription += "; no automatic retry"
	}
	return coordsem.TaskDecision(in), nil
}

// statusExecutionWorkflow is display provenance, never workflow-control authority.
// Ambiguous pre-binding call IDs retain the cold full-state compatibility path.
func (s semanticSource) statusExecutionWorkflow(id string) (string, bool, error) {
	e, err := s.execution(id)
	if err != nil || e == nil {
		return "", false, err
	}
	if e.Workflow != "" {
		return e.Workflow, false, nil
	}
	m, err := semanticGet[coordsem.Member](s, "member", e.Member)
	if err != nil {
		return "", false, err
	}
	if m != nil && m.Execution == e.ID && m.Controller != "" {
		return m.Controller, false, nil
	}
	raw, err := semanticGet[struct{ Request struct{ CallID string } }](s, "execution", id)
	if err != nil || raw == nil || raw.Request.CallID == "" {
		return "", false, err
	}
	var matches []string
	err = (derivedEngine{store: s.store, parent: s.parent}).each(statusStepCall, raw.Request.CallID, "workflow_step", func(item derivedItem) error {
		step, err := semanticGet[struct{ Kind string }](s, "workflow_step", item.key.ID)
		if err != nil {
			return err
		}
		if step != nil && step.Kind == "agent" {
			matches = append(matches, item.order.Workflow)
		}
		return nil
	})
	sort.Strings(matches)
	matches = slices.Compact(matches)
	if len(matches) == 0 {
		return "", false, err
	}
	return matches[0], len(matches) > 1, err
}

const (
	statusStepCall  coordindex.Index = "semantic.step-call"
	statusOpenTask  coordindex.Index = "semantic.open-task"
	statusUncertain coordindex.Index = "semantic.uncertain-apply"
)
