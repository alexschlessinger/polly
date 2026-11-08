package swarm

import (
	"context"
	"errors"
	"reflect"
	"strings"

	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/workflow"
)

// A backend error after a successful callback is not a failed release. Callers
// must preserve its receipts/held scratch rather than perform failure cleanup.
type releaseCommitUncertain struct{ cause error }

func (e *releaseCommitUncertain) Error() string { return e.cause.Error() }
func (e *releaseCommitUncertain) Unwrap() error { return e.cause }
func uncertainReleaseCommit(err error) bool {
	var uncertain *releaseCommitUncertain
	return errors.As(err, &uncertain)
}

// Release observations must not promote an empty root or create swarm format.
// Actual marks still go through updateRecords and its prepare/lease fences.
func (r *Runtime) readReleaseRecords(ctx context.Context, fn func(*recordReader) error) error {
	repo, err := openRecordRepository(r.parent)
	if err != nil {
		return err
	}
	repo.parentJournal = true
	return repo.Read(ctx, func(rd *recordReader) error {
		if rd.journalOnly {
			// The format-less guard checks record keys, not journal payloads.
			// Preserve the old read's typed validation without preparing a swarm.
			if _, err := rd.ParentTurn(r.ID); err != nil {
				return err
			}
		}
		return fn(rd)
	})
}

// Selectors only find candidates. Authority is always rechecked against owned,
// exact records in the same callback, never a display or derived status row.
func releaseCandidate(rd *recordReader, selector sessions.CoordinationCandidateSelector, key string, accept func(sessions.RecordKey) (bool, error)) (bool, error) {
	if rd.journalOnly {
		return false, nil
	}
	q := sessions.CoordinationCandidateQuery{Selector: selector, Key: key, Limit: 32, MaxBytes: 256 << 10}
	for {
		page, err := sessions.QueryCoordinationCandidates(rd.raw, q)
		if err != nil {
			return false, err
		}
		for _, record := range page.Records {
			ok, err := accept(record.Key)
			if err != nil || ok {
				return ok, err
			}
		}
		if page.Next == "" {
			return false, nil
		}
		if page.Next == q.After {
			return false, sessions.ErrCoordinationCursor
		}
		q.After = page.Next
	}
}

func ownerHasOpenTaskRecords(rd *recordReader, owner string) (bool, error) {
	return releaseCandidate(rd, sessions.CandidateTaskOpenOwner, owner, func(key sessions.RecordKey) (bool, error) {
		t, err := rd.Task(key.ID)
		return t != nil && t.Owner == owner && t.Status != "done" && t.Status != "canceled", err
	})
}

func memberReleaseEligibleRecords(rd *recordReader, m *Member) (bool, string, error) {
	if m.Execution != "" {
		e, err := rd.Execution(m.Execution)
		if err != nil {
			return false, "", err
		}
		if e != nil {
			switch e.Status {
			case "queued", "running", "waiting", "paused":
				return false, "member has an active or paused execution", nil
			}
		}
	}
	open, err := ownerHasOpenTaskRecords(rd, m.ID)
	if err != nil || open {
		return false, "member has an open task", err
	}
	unresolved, err := releaseCandidate(rd, sessions.CandidateApplyUnresolvedOwner, m.ID, func(key sessions.RecordKey) (bool, error) {
		a, err := getRecord[ApplyRecord](rd, "apply", key.ID)
		if err != nil || a == nil || a.Status != "applying" && a.Status != "recovery_required" {
			return false, err
		}
		for _, ref := range a.Tasks {
			t, err := rd.Task(ref.Task)
			if err != nil || t != nil && t.Owner == m.ID {
				return t != nil && t.Owner == m.ID, err
			}
		}
		return false, nil
	})
	if err != nil || unresolved {
		return false, "member has an unresolved integration", err
	}
	return true, "", nil
}

type releaseProof struct {
	context   *ExecutionContext
	member    *Member
	execution *Execution
}

func releaseProofRecords(rd *recordReader, c *ExecutionContext) (*releaseProof, error) {
	p := &releaseProof{context: c}
	if c == nil || c.Owner == "" {
		return p, nil
	}
	var err error
	p.member, err = rd.Member(c.Owner)
	if err == nil && p.member != nil && p.member.Execution != "" {
		p.execution, err = rd.Execution(p.member.Execution)
	}
	return p, err
}

func sameReleaseProof(a, b *releaseProof) bool {
	if !reflect.DeepEqual(a.context, b.context) {
		return false
	}
	if a.member == nil || b.member == nil {
		return a.member == nil && b.member == nil
	}
	x, y := a.member, b.member
	if x.ID != y.ID || x.Context != y.Context || x.Controller != y.Controller || x.Execution != y.Execution || x.Task != y.Task || x.Control != y.Control {
		return false
	}
	if a.execution == nil || b.execution == nil {
		return a.execution == nil && b.execution == nil
	}
	u, v := a.execution, b.execution
	return u.ID == v.ID && u.Generation == v.Generation && u.Status == v.Status && u.Member == v.Member && u.Workspace == v.Workspace
}

func (r *Runtime) releaseEligibleRecords(rd *recordReader, c *ExecutionContext, controller string) (bool, string, error) {
	if c == nil {
		return false, "unknown workspace", nil
	}
	if c.Release == WorkspaceRetained {
		return false, "workspace is retained", nil
	}
	// A durable receipt can outlive its former assignment, but never authorizes
	// removing a context rebound to a live execution. The proof fences changes
	// during this attempt; this check also protects receipts loaded on restart.
	if c.Release == WorkspaceReleasing {
		if c.Owner == "" {
			return true, "", nil
		}
		m, err := rd.Member(c.Owner)
		if err != nil || m == nil || m.Context != c.ID {
			return err == nil, "", err
		}
		r.mu.Lock()
		active := r.active[m.ID] != nil
		r.mu.Unlock()
		if active {
			return false, "member is executing", nil
		}
		if m.Execution != "" {
			e, err := rd.Execution(m.Execution)
			if err != nil {
				return false, "", err
			}
			if e != nil {
				switch e.Status {
				case "queued", "running", "waiting", "paused":
					return false, "member has an active or paused execution", nil
				}
			}
		}
		return true, "", nil
	}
	if controller != "" && c.Owner == controller {
		return true, "", nil
	}
	if c.Owner == "" {
		return false, "workspace has no owner", nil
	}
	m, err := rd.Member(c.Owner)
	if err != nil {
		return false, "", err
	}
	if m == nil || m.ID != c.Owner || m.Context != c.ID {
		return false, "workspace is not the member's current workspace", nil
	}
	if controller != "" && m.Controller != controller {
		return false, "workspace belongs to another controller", nil
	}
	r.mu.Lock()
	active := r.active[m.ID] != nil
	reserved := m.Controller != "" && r.workflowCancels[m.Controller] != nil
	r.mu.Unlock()
	if active {
		return false, "member is executing", nil
	}
	if controller == "" && reserved {
		return false, "member is reserved by a running workflow", nil
	}
	return memberReleaseEligibleRecords(rd, m)
}

func releasedWorkspaceRecords(rd *recordReader, id, controller string) (bool, error) {
	if rd.journalOnly {
		return false, nil
	}
	q := sessions.RecordQuery{Index: sessions.CoordinationIndexOverviewDependency, Key: sessions.CoordinationIndexKey("context", id), Kind: "execution", Limit: 32}
	for {
		page, err := rd.Executions(q)
		if err != nil {
			return false, err
		}
		for _, record := range page.Records {
			e := record.Value
			if e.Workspace == id && (controller == "" || e.Workflow == controller) {
				return true, nil
			}
		}
		if page.Next == "" {
			return false, nil
		}
		if page.Next == q.After {
			return false, sessions.ErrCoordinationCursor
		}
		q.After = page.Next
	}
}

func (h *workflowHost) releaseAuthorityRecords(rd *recordReader, c *ExecutionContext, id string) (bool, error) {
	if id == "" {
		return false, nil
	}
	if c != nil {
		if c.Owner == h.controller {
			return true, nil
		}
		if c.Owner != "" {
			m, err := rd.Member(c.Owner)
			if err != nil {
				return false, err
			}
			if m != nil && m.Context == id && m.Controller == h.controller {
				return true, nil
			}
		}
		if c.Release != WorkspaceReleasing {
			return false, nil
		}
	}
	if ok, err := releasedWorkspaceRecords(rd, id, h.controller); ok || err != nil {
		return ok, err
	}
	return releaseCandidate(rd, sessions.CandidateWorkflowContextStep, sessions.CoordinationIndexKey(h.controller, id), func(key sessions.RecordKey) (bool, error) {
		controller, _, canonical := strings.Cut(key.ID, "/")
		if key.Kind != "workflow_step" || !canonical || controller != h.controller {
			return false, nil
		}
		step, err := getRecord[workflow.Step](rd, "workflow_step", key.ID)
		if err != nil || step == nil {
			return false, err
		}
		value, _ := step.Value.(string)
		return step.Kind == "context" && step.Status == "completed" && value == id, nil
	})
}

func (r *Runtime) readReleaseProof(ctx context.Context, id string) (*releaseProof, error) {
	var p *releaseProof
	err := r.readReleaseRecords(ctx, func(rd *recordReader) error {
		c, err := rd.Context(id)
		if err == nil {
			p, err = releaseProofRecords(rd, c)
		}
		return err
	})
	return p, err
}
