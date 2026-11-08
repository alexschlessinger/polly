// Package coordsem contains pure coordination presentation and news rules.
// Inputs name the exact dependencies of a rule; none is a partial family state.
// These rules do not authorize a transition or perform settlement repairs.
package coordsem

import (
	"fmt"
	"strings"
)

const Version = 1

type Deferral struct {
	Workflow, Execution, Owner             string
	Revision, Generation, AcceptedRevision int
	Status, Note                           string
}

type Task struct {
	ID, Run, Status, Owner, Execution, Snapshot, StartingSnapshot, Feedback, Requirement string
	Revision, AcceptedRevision                                                           int
	Deferral                                                                             *Deferral
}

type Execution struct {
	ID, Run, Member, Workflow, Status, StopReason string
	Generation, Iterations, MaxIterations         int
	RetryPause                                    bool
}

type Workflow struct {
	ID, Run, Status string
	Acknowledged    bool
}

type Member struct {
	ID, Control, Execution, Task, Controller string
}

// Deferred validates the saved fence, rather than treating the presence of a
// deferral record as an exemption. Inferred display ownership is not authority.
func Deferred(t *Task, e *Execution, w *Workflow) bool {
	if t == nil || t.Deferral == nil || t.Status == "done" || t.Status == "canceled" {
		return false
	}
	d := t.Deferral
	return e != nil && w != nil && w.Acknowledged && (w.Status == "failed" || w.Status == "canceled" || w.Status == "interrupted") &&
		e.Workflow == d.Workflow && e.Run == w.Run && e.Run == t.Run && e.Member == t.Owner &&
		e.ID == d.Execution && e.Generation == d.Generation &&
		t.Owner == d.Owner && t.Revision == d.Revision && t.Status == d.Status &&
		t.AcceptedRevision == d.AcceptedRevision && e.Status != "running" && e.Status != "queued" && e.Status != "waiting"
}

func Activity(control string, e *Execution, taskStatus string) (lifecycle, detail string) {
	switch {
	case control == "stopped":
		return "paused", "stopped"
	case e == nil || e.Status == "completed":
		return "idle", taskStatus
	case e.Status == "queued":
		return "active", "queued"
	case e.Status == "running":
		return "active", ""
	case e.Status == "waiting":
		return "waiting", ""
	case e.Status == "failed":
		return "paused", "failed"
	case e.RetryPause:
		return "paused", "retry budget exhausted"
	case e.StopReason == "max_iterations":
		return "paused", fmt.Sprintf("iteration limit (%d/%d)", e.Iterations, e.MaxIterations)
	default:
		return "paused", "interrupted"
	}
}

func Display(lifecycle, detail string, deferred bool) string {
	if detail != "" {
		lifecycle += " · " + detail
	}
	if deferred {
		lifecycle += " · deferred"
	}
	return lifecycle
}

func Accepted(t *Task) bool { return t != nil && t.Revision > 0 && t.AcceptedRevision == t.Revision }
func TaskStatus(t *Task) string {
	if t == nil {
		return ""
	}
	if t.Status == "awaiting_review" && Accepted(t) && t.Snapshot != "" {
		return "integration pending"
	}
	return strings.ReplaceAll(t.Status, "_", " ")
}
func TaskDisplay(t *Task, delivering, editing, conflicted bool) string {
	if delivering {
		return "delivering"
	}
	if t != nil && t.Status == "awaiting_review" && editing && conflicted {
		return "integration halted"
	}
	return TaskStatus(t)
}

func Controlled(e *Execution, w *Workflow) bool {
	return e != nil && e.Workflow != "" && w != nil && w.Status == "running"
}

// WorkStepRunning and DisplayStepRunning intentionally differ for followups.
func WorkStepRunning(kind, status string) bool {
	return status == "running" && kind != "agent" && kind != "followup"
}
func DisplayStepRunning(kind, status string) bool { return status == "running" && kind != "agent" }
func ExecutionRunning(status string) bool         { return status == "queued" || status == "running" }
func ActiveRun(status string) bool                { return status == "running" || status == "paused" }
func UncertainApply(status string) bool           { return status == "applying" || status == "recovery_required" }
func Unsettled(t *Task, run string, deferred bool) bool {
	return t != nil && (t.Run == run || t.Deferral != nil) && t.Status != "done" && t.Status != "canceled" && !deferred
}

type Dependency struct {
	ID             string
	Canceled, Open bool
}

type Bucket int

const (
	Decision Bucket = iota
	FoldedDecision
	Working
	FoldedWorking
	Done
)

// Classification contains only the facts used to fold one unsettled task.
// Dependency buckets are deliberately not inputs: the historical recursion
// visited pending dependencies but did not use their buckets as readiness.
type Classification struct {
	Status                                                                string
	Controlled, Unchanged, Delivering, Advancing, Parked, Asking, Running bool
	FirstApply                                                            string
	Dependencies                                                          []Dependency
}

func Classify(f Classification) (Bucket, string) {
	switch {
	case f.Controlled:
		return FoldedWorking, ""
	case f.Unchanged && f.FirstApply != "":
		return Working, "accepted · settles after integration " + f.FirstApply + " is reconciled"
	case f.Unchanged:
		return Done, ""
	case f.Delivering:
		return Working, "delivering"
	case f.Advancing:
		return FoldedWorking, ""
	case f.Parked && f.Asking:
		return FoldedDecision, ""
	case f.Parked && f.Running:
		return FoldedWorking, ""
	case f.Parked:
		return Decision, ""
	case f.Status == "pending":
		for _, dep := range f.Dependencies {
			if dep.Canceled {
				return Decision, dep.ID
			}
		}
		if len(f.Dependencies) == 0 {
			return Decision, ""
		}
		ids := make([]string, 0, len(f.Dependencies))
		for _, dep := range f.Dependencies {
			if !dep.Open {
				return Decision, ""
			}
			ids = append(ids, dep.ID)
		}
		return Working, "pending · waiting on " + strings.Join(ids, ", ")
	}
	return Decision, ""
}

// The canonical values are comparable. Labels, outputs and execution progress
// never enter them. Control is a separate mask, not part of the value.
type TaskNews struct {
	Status, Owner, Execution, Snapshot, Feedback string
	Revision, Accepted                           int
	Deferred, PendingNotice                      bool
}

func NewsTask(t Task, deferred, pending bool) TaskNews {
	return TaskNews{t.Status, t.Owner, t.Execution, t.Snapshot, t.Feedback, t.Revision, t.AcceptedRevision, deferred, pending}
}

type MemberNews struct {
	Lifecycle, Control, Execution string
	Generation                    int
}

func NewsMember(m Member, e *Execution) MemberNews {
	lifecycle, _ := Activity(m.Control, e, "")
	generation := 0
	if e != nil {
		generation = e.Generation
	}
	return MemberNews{lifecycle, m.Control, m.Execution, generation}
}

type WorkflowNews struct {
	Status       string
	Acknowledged bool
}

func NewsWorkflow(w Workflow) WorkflowNews { return WorkflowNews{w.Status, w.Acknowledged} }

// ParentDisposition is global; an active-run page is not a settlement gate.
type Disposition struct{ Review, Integration, Halted, Open, Delivering, HasRuns bool }

func (d Disposition) Detail() string {
	switch {
	case d.Halted:
		return "integration halted"
	case d.Review:
		return "awaiting review"
	case d.Integration:
		return "integration pending"
	case d.Delivering:
		return "delivering"
	case d.HasRuns && !d.Open:
		return "done"
	}
	return ""
}
func TaskDisposition(t Task, display string, deferred bool) Disposition {
	if deferred {
		return Disposition{}
	}
	switch {
	case display == "delivering":
		return Disposition{Delivering: true}
	case display == "integration halted":
		return Disposition{Halted: true}
	case display == "integration pending":
		return Disposition{Integration: true}
	case t.Status == "awaiting_review":
		return Disposition{Review: true}
	case t.Status != "done" && t.Status != "canceled":
		return Disposition{Open: true}
	}
	return Disposition{}
}
