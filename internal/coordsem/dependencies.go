package coordsem

// Snapshot and Submission describe only immutable provenance required by the
// status rules. Opaque results and plans never pass through these projections.
type Snapshot struct{ ID, Tree, Commit, Source string }

func ValidSnapshot(s *Snapshot, id string) bool {
	return s != nil && id != "" && s.ID == id && s.Tree != "" && s.Commit != "" && s.Source != ""
}
func IntegrationTask(t *Task, unchanged bool) bool {
	return t != nil && (t.Status == "awaiting_review" || t.Status == "done" && Accepted(t) && unchanged)
}
func CandidateTaskValid(t *Task, run, snapshot string, revision int, unchanged bool) bool {
	return t != nil && t.Run == run && t.Revision == revision && t.Snapshot == snapshot && IntegrationTask(t, unchanged)
}
func CompletedCandidate(status, parentTree, mergedTree string, pending, conflicts int) bool {
	return status == "ready" && pending == 0 && conflicts == 0 && parentTree != "" && parentTree == mergedTree
}
func AcceptedUnchanged(t *Task, deferred, unchanged bool) bool {
	return t != nil && t.Status == "awaiting_review" && !deferred && Accepted(t) && unchanged
}
func TaskCounts(t Task, run string, deferred bool) (done, retained int) {
	if t.Status == "done" && t.Run == run {
		return 1, 0
	}
	if deferred {
		return 0, 1
	}
	return 0, 0
}
func MemberCounts(control string, delivering, busy, attention bool) (delivery, dormant int) {
	if delivering {
		return 1, 0
	}
	if control == "" && !busy && !attention {
		return 0, 1
	}
	return 0, 0
}
func Requirement(t *Task, ownerPresent, readOnly bool) string {
	if t == nil {
		return ""
	}
	if t.Requirement != "" {
		return t.Requirement
	}
	if ownerPresent && !readOnly {
		return "applied"
	}
	return "reviewed"
}
func Editing(t *Task, ownerPresent, readOnly bool) bool {
	return Requirement(t, ownerPresent, readOnly) == "applied" || ownerPresent && !readOnly
}

type Delivery struct {
	Revision  int
	Execution string
}
type ResultIdentity struct {
	Task, Execution string
	Revision        int
}

func Delivering(t *Task, e *Execution, requirement string, delivery *Delivery, result *ResultIdentity) bool {
	return t != nil && t.Status == "running" && requirement == "delivered" && !(delivery != nil && delivery.Revision == t.Revision && delivery.Execution == t.Execution) && e != nil && e.Status == "completed" && e.Member == t.Owner && result != nil && result.Task == t.ID && result.Execution == e.ID && result.Revision == t.Revision
}

type MemberFacts struct {
	Lifecycle, Detail, Display, TaskStatus string
	Busy, Attention, Deferred, Delivering  bool
}

func DescribeMember(control string, e *Execution, taskStatus string, open, deferred, delivering bool) MemberFacts {
	p := MemberFacts{TaskStatus: taskStatus, Deferred: deferred, Delivering: delivering && !deferred}
	p.Lifecycle, p.Detail = Activity(control, e, taskStatus)
	p.Busy = p.Lifecycle == "active" || p.Lifecycle == "waiting"
	p.Attention = !p.Deferred && !p.Busy && !p.Delivering && open
	if control != "" && p.Attention && taskStatus != "" {
		p.Detail += " · " + taskStatus
	}
	p.Display = Display(p.Lifecycle, p.Detail, p.Deferred)
	return p
}
