package swarm

import (
	"context"
	"errors"
	"time"

	"github.com/alexschlessinger/pollytool/internal/ids"
	"github.com/alexschlessinger/pollytool/worktree"
)

type IntegrationInput struct {
	TaskReference
	Base      worktree.Snapshot `json:"base"`
	Submitted worktree.Snapshot `json:"submitted"`
}

// IntegrationCandidate binds an ordered merge and every repair to exact task
// revisions. Snapshots are immutable; no checkout is allocated by preparation.
type IntegrationCandidate struct {
	ID          string              `json:"id"`
	Run         string              `json:"run"`
	Status      string              `json:"status"`
	Inputs      []IntegrationInput  `json:"inputs"`
	Repairs     []IntegrationInput  `json:"repairs"`
	Pending     []IntegrationInput  `json:"pending"`
	Parent      worktree.Snapshot   `json:"parent"`
	Merged      worktree.Snapshot   `json:"merged"`
	Conflicts   []worktree.Conflict `json:"conflicts"`
	Drift       string              `json:"drift"`
	Plan        worktree.ApplyPlan  `json:"plan"`
	Accepted    bool                `json:"accepted"`
	Predecessor string              `json:"predecessor,omitempty"`
	Successor   string              `json:"successor,omitempty"`
	Created     time.Time           `json:"created"`
	// Receipt is hydrated from the authoritative apply domain on reads.
	Receipt *ApplyRecord `json:"receipt,omitempty"`
}
type IntegrationRefresh struct {
	*IntegrationCandidate
	Changed bool `json:"changed"`
}

func integrationInputs(s *State, refs []TaskReference) ([]IntegrationInput, string, error) {
	if len(refs) == 0 {
		return nil, "", fail("invalid_args", "at least one task revision is required")
	}
	inputs := make([]IntegrationInput, 0, len(refs))
	seen := map[string]bool{}
	run := ""
	for _, ref := range refs {
		t := s.Tasks[ref.Task]
		if seen[ref.Task] || t == nil || t.Revision != ref.Revision || !integrationTask(s, t) || t.Snapshot == "" {
			return nil, "", fail("stale_task", "prepare requires unique current submitted editing task revisions")
		}
		seen[ref.Task] = true
		if run == "" {
			run = t.Run
		}
		if t.Run != run {
			return nil, "", fail("invalid_args", "integration tasks must belong to one run")
		}
		// Retained candidates remain inspectable after settlement. Acceptance
		// and application reactivate their run under the task lock separately.
		if r := s.Runs[run]; r == nil || r.Status != "running" && r.Status != "paused" && !TaskDeferred(s, t) {
			return nil, "", fail("stale_task", "task run is no longer active")
		}
		owner := s.Members[t.Owner]
		if owner == nil || owner.ReadOnly {
			return nil, "", fail("invalid_args", "integration requires an editing task")
		}
		base, snapshot := taskSnapshots(s, t)
		if base == nil || snapshot == nil {
			return nil, "", fail("stale_task", "submitted task provenance is unavailable; submit again")
		}
		inputs = append(inputs, IntegrationInput{TaskReference: ref, Base: *base, Submitted: *snapshot})
	}
	return inputs, run, nil
}

func (c *IntegrationCandidate) references() []TaskReference {
	refs := make([]TaskReference, 0, len(c.Inputs)+len(c.Repairs))
	for _, input := range c.Inputs {
		refs = append(refs, input.TaskReference)
	}
	for _, input := range c.Repairs {
		refs = append(refs, input.TaskReference)
	}
	return refs
}
func validCandidate(s *State, c *IntegrationCandidate) error {
	if c == nil {
		return fail("unknown_candidate", "unknown integration candidate; legacy previews require fresh preparation")
	}
	if c.Successor != "" || c.Status == "superseded" {
		return fail("superseded", "candidate was superseded by "+c.Successor)
	}
	if c.Status == "applied" {
		return fail("already_applied", "candidate was already applied")
	}
	if receipt := s.Applies[c.ID]; receipt != nil && receipt.Status != "not_applied" {
		return fail("recovery_required", "reconcile the existing apply before changing this candidate")
	}
	for _, input := range append(append([]IntegrationInput{}, c.Inputs...), c.Repairs...) {
		t := s.Tasks[input.Task]
		if t == nil || t.Run != c.Run || t.Revision != input.Revision || t.Snapshot != input.Submitted.ID || !integrationTask(s, t) {
			return fail("stale_task", "candidate no longer names current submitted task revisions")
		}
	}
	return nil
}

// currentCandidate is the newest usable, outstanding decision for a task.
// The persisted supersession links and this projection share the same validity
// checks, so stale submissions and completed no-ops cannot demand attention.
func currentCandidate(s *State, task *Task) *IntegrationCandidate {
	var newest *IntegrationCandidate
	for _, c := range s.Integrations {
		if c.Status != "ready" && c.Status != "conflicted" || validCandidate(s, c) != nil || completedUnchangedCandidate(s, c) {
			continue
		}
		for _, ref := range c.references() {
			if ref.Task == task.ID && (newest == nil || c.Created.After(newest.Created) || c.Created.Equal(newest.Created) && c.ID > newest.ID) {
				newest = c
			}
		}
	}
	return newest
}

func (r *Runtime) pinIntegrationSnapshot(ctx context.Context, snapshot worktree.Snapshot) error {
	return r.update(ctx, func(s *State) error { s.Snapshots[snapshot.ID] = &snapshot; return nil })
}

func (r *Runtime) finishCandidate(ctx context.Context, c *IntegrationCandidate) error {
	m, err := r.manager(ctx)
	if err != nil {
		return err
	}
	for len(c.Pending) > 0 && len(c.Conflicts) == 0 {
		input := c.Pending[0]
		merged, err := m.Merge(ctx, input.Base, c.Merged, input.Submitted)
		if err != nil {
			return err
		}
		if err = r.pinIntegrationSnapshot(ctx, merged.Snapshot); err != nil {
			return err
		}
		c.Merged, c.Conflicts = merged.Snapshot, merged.Conflicts
		c.Pending = c.Pending[1:]
	}
	c.Status = "ready"
	if len(c.Conflicts) > 0 {
		c.Status = "conflicted"
	}
	c.Plan, err = m.BuildApplyPlan(ctx, c.ID, c.Parent, c.Merged, c.Drift)
	return err
}

// storeCandidate is shared by prepare, revise, refresh, and Integrate. Every
// current overlapping candidate points to its replacement, including candidates
// prepared independently rather than through an explicit predecessor.
func storeCandidate(s *State, c *IntegrationCandidate) error {
	if err := validCandidate(s, c); err != nil {
		return err
	}
	if c.Predecessor != "" {
		if err := validCandidate(s, s.Integrations[c.Predecessor]); err != nil {
			return err
		}
	}
	tasks := map[string]bool{}
	for _, ref := range c.references() {
		tasks[ref.Task] = true
	}
	for _, old := range s.Integrations {
		if old.ID == c.ID || validCandidate(s, old) != nil {
			continue
		}
		for _, ref := range old.references() {
			if tasks[ref.Task] {
				old.Status, old.Successor = "superseded", c.ID
				break
			}
		}
	}
	s.Integrations[c.ID] = c
	return nil
}

func (r *Runtime) saveCandidate(ctx context.Context, c *IntegrationCandidate) error {
	return r.update(ctx, func(s *State) error { return storeCandidate(s, c) })
}

func (r *Runtime) prepareCandidateLocked(ctx context.Context, inputs []IntegrationInput, run, drift string) (*IntegrationCandidate, error) {
	m, err := r.manager(ctx)
	if err != nil {
		return nil, err
	}
	parent, err := m.Capture(ctx, r.config.Root)
	if err != nil {
		return nil, err
	}
	if err = r.pinIntegrationSnapshot(ctx, parent); err != nil {
		return nil, err
	}
	c := &IntegrationCandidate{ID: ids.New(), Run: run, Inputs: inputs, Pending: append([]IntegrationInput{}, inputs...), Parent: parent, Merged: parent, Drift: drift, Created: time.Now().UTC()}
	if err = r.finishCandidate(ctx, c); err != nil {
		return nil, err
	}
	return c, nil
}

// PrepareIntegration combines submissions in order using each task's own base.
func (r *Runtime) PrepareIntegration(ctx context.Context, refs []TaskReference, drift string) (*IntegrationCandidate, error) {
	if drift == "" {
		drift = "paths"
	}
	if drift != "paths" && drift != "tree" {
		return nil, fail("invalid_args", "drift must be paths or tree")
	}
	var result *IntegrationCandidate
	err := r.withApplyLock(ctx, func(ctx context.Context) error {
		s, err := r.read(ctx)
		if err != nil {
			return err
		}
		inputs, run, err := integrationInputs(s, refs)
		if err != nil {
			return err
		}
		c, err := r.prepareCandidateLocked(ctx, inputs, run, drift)
		if err != nil {
			return err
		}
		if err = r.saveCandidate(ctx, c); err != nil {
			return err
		}
		result = c
		return nil
	})
	return result, err
}

func (r *Runtime) ReadIntegration(ctx context.Context, id string) (*IntegrationCandidate, error) {
	s, err := r.read(ctx)
	if err != nil {
		return nil, err
	}
	c := s.Integrations[id]
	if c == nil {
		return nil, fail("unknown_candidate", "unknown integration candidate; legacy previews require fresh preparation")
	}
	c.Receipt = s.Applies[id]
	return c, nil
}

// ReviseIntegration adopts an independently submitted repair of this exact
// intermediate snapshot, then continues the unmerged input tail.
func (r *Runtime) ReviseIntegration(ctx context.Context, id string, repair TaskReference) (*IntegrationCandidate, error) {
	var result *IntegrationCandidate
	err := r.withApplyLock(ctx, func(ctx context.Context) error {
		s, err := r.read(ctx)
		if err != nil {
			return err
		}
		old := s.Integrations[id]
		if err = validCandidate(s, old); err != nil {
			return err
		}
		inputs, run, err := integrationInputs(s, []TaskReference{repair})
		if err != nil {
			return err
		}
		if run != old.Run || inputs[0].Base.ID != old.Merged.ID {
			return fail("invalid_repair", "repair must start from the exact candidate snapshot")
		}
		for _, ref := range old.references() {
			if ref.Task == repair.Task {
				return fail("invalid_repair", "repair requires a distinct task")
			}
		}
		c := *old
		c.ID = ids.New()
		c.Predecessor = old.ID
		c.Accepted = false
		c.Created = time.Now().UTC()
		c.Receipt = nil
		c.Repairs = append(append([]IntegrationInput{}, old.Repairs...), inputs[0])
		c.Pending = append([]IntegrationInput{}, old.Pending...)
		c.Merged = inputs[0].Submitted
		c.Conflicts = nil
		if err = r.finishCandidate(ctx, &c); err != nil {
			return err
		}
		if err = r.saveCandidate(ctx, &c); err != nil {
			return err
		}
		result = &c
		return nil
	})
	return result, err
}

func (r *Runtime) RefreshIntegration(ctx context.Context, id string) (*IntegrationRefresh, error) {
	var result *IntegrationRefresh
	err := r.withApplyLock(ctx, func(ctx context.Context) error {
		s, err := r.read(ctx)
		if err != nil {
			return err
		}
		old := s.Integrations[id]
		if err = validCandidate(s, old); err != nil {
			return err
		}
		result, err = r.refreshCandidateLocked(ctx, old)
		if err != nil || !result.Changed {
			return err
		}
		return r.saveCandidate(ctx, result.IntegrationCandidate)
	})
	return result, err
}

func (r *Runtime) refreshCandidateLocked(ctx context.Context, old *IntegrationCandidate) (*IntegrationRefresh, error) {
	if old.Status != "ready" || len(old.Pending) > 0 {
		return nil, fail("conflicts", "resolve pending inputs before refreshing")
	}
	m, err := r.manager(ctx)
	if err != nil {
		return nil, err
	}
	parent, err := m.CaptureCurrent(ctx, old.Parent)
	if err != nil {
		return nil, err
	}
	if parent.Tree == old.Parent.Tree {
		return &IntegrationRefresh{IntegrationCandidate: old, Changed: false}, nil
	}
	if err = r.pinIntegrationSnapshot(ctx, parent); err != nil {
		return nil, err
	}
	merged, err := m.Merge(ctx, old.Parent, parent, old.Merged)
	if err != nil {
		return nil, err
	}
	if err = r.pinIntegrationSnapshot(ctx, merged.Snapshot); err != nil {
		return nil, err
	}
	c := *old
	c.ID = ids.New()
	c.Predecessor = old.ID
	c.Accepted = false
	c.Created = time.Now().UTC()
	c.Receipt = nil
	c.Parent, c.Merged, c.Conflicts = parent, merged.Snapshot, merged.Conflicts
	if err = r.finishCandidate(ctx, &c); err != nil {
		return nil, err
	}
	return &IntegrationRefresh{IntegrationCandidate: &c, Changed: true}, nil
}

func (r *Runtime) AcceptIntegration(ctx context.Context, id string) (*IntegrationCandidate, error) {
	r.parentTools.Lock()
	defer r.parentTools.Unlock()
	err := r.update(ctx, func(s *State) error {
		c := s.Integrations[id]
		if err := validCandidate(s, c); err != nil {
			return err
		}
		if c.Status != "ready" || len(c.Pending) > 0 || len(c.Conflicts) > 0 {
			return fail("conflicts", "candidate has unresolved inputs")
		}
		for _, ref := range c.references() {
			if err := reactivateTask(s, s.Tasks[ref.Task]); err != nil {
				return err
			}
			s.Tasks[ref.Task].AcceptedRevision = ref.Revision
		}
		c.Accepted = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	return r.ReadIntegration(ctx, id)
}

func (r *Runtime) ApplyIntegration(ctx context.Context, id string) (*ApplyRecord, error) {
	var result *ApplyRecord
	err := r.withApplyLock(ctx, func(ctx context.Context) error {
		s, err := r.read(ctx)
		if err != nil {
			return err
		}
		c := s.Integrations[id]
		if c == nil {
			return fail("unknown_candidate", "prepare a candidate with task revision provenance first")
		}
		if c.Successor != "" {
			return fail("superseded", "candidate was superseded by "+c.Successor)
		}
		if receipt := s.Applies[id]; receipt != nil && receipt.Status == "applied" {
			result = receipt
			return nil
		}
		if err = validCandidate(s, c); err != nil {
			return err
		}
		if !c.Accepted || c.Status != "ready" {
			return fail("not_accepted", "accept this exact completed candidate before applying")
		}
		result, err = r.applyLocked(ctx, c)
		return err
	})
	return result, err
}

// applyLocked keeps the receipt read inside the same exclusion even when the
// caller was canceled after the write's commit boundary.
func (r *Runtime) applyLocked(ctx context.Context, c *IntegrationCandidate) (*ApplyRecord, error) {
	if err := r.applyPlanLocked(ctx, c.Plan, c.references()); err != nil {
		return nil, err
	}
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), applyOutcomeTimeout)
	defer cancel()
	s, err := r.read(readCtx)
	if err != nil {
		return nil, err
	}
	receipt := s.Applies[c.ID]
	if receipt == nil {
		return nil, errors.New("missing apply receipt")
	}
	return receipt, nil
}
