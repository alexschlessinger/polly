package swarm

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/alexschlessinger/pollytool/worktree"
)

type TaskReference struct {
	Task     string `json:"task"`
	Revision int    `json:"revision"`
}

// ApplyRecord is the durable receipt for a candidate, independent of whether
// the initiating tool or JavaScript turn received its response.
type ApplyRecord struct {
	ID             string             `json:"id"`
	Tasks          []TaskReference    `json:"tasks"`
	Plan           worktree.ApplyPlan `json:"plan"`
	ObservedParent worktree.Snapshot  `json:"observedParent"`
	Status         string             `json:"status"`
	Error          string             `json:"error,omitempty"`
	Started        time.Time          `json:"started"`
	Finished       time.Time          `json:"finished,omitempty"`
}

const applyOutcomeTimeout = 10 * time.Second

// withApplyLock includes queued calls in shutdown accounting, but never holds
// launchMu while waiting. The lock order is gate, task mutations, runtime Git.
func (r *Runtime) withApplyLock(ctx context.Context, work func(context.Context) error) error {
	r.launchMu.Lock()
	if r.closing || r.ctx.Err() != nil {
		r.launchMu.Unlock()
		return context.Canceled
	}
	r.wg.Add(1)
	r.launchMu.Unlock()
	defer r.wg.Done()
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(r.ctx, cancel)
	defer func() { stop(); cancel() }()
	release, err := r.gate.Exclusive(ctx)
	if err != nil {
		return err
	}
	defer release()
	r.parentTools.Lock()
	defer r.parentTools.Unlock()
	return work(ctx)
}

func acceptedTasks(s *State, refs []TaskReference) error {
	for _, ref := range refs {
		t := s.Tasks[ref.Task]
		if t == nil || t.Revision != ref.Revision || t.AcceptedRevision != ref.Revision || (t.Status != "awaiting_review" && t.Status != "done") {
			return fail("stale_task", "application no longer names the accepted task revisions")
		}
	}
	return nil
}

// applyPlanLocked requires the gate and parentTools. Keeping the write's
// detached context inside this lifetime prevents cancellation from releasing
// the gate, session, or task lock before the outcome is recorded.
func (r *Runtime) applyPlanLocked(ctx context.Context, plan worktree.ApplyPlan, refs []TaskReference) error {
	s, err := r.read(ctx)
	if err != nil {
		return err
	}
	if prior := s.Applies[plan.ID]; prior != nil {
		if prior.Status == "applied" {
			return nil
		}
		if prior.Status != "not_applied" {
			return fail("recovery_required", "inspect the interrupted application before retrying")
		}
	}
	if err = acceptedTasks(s, refs); err != nil {
		return err
	}
	for _, ref := range refs {
		if err = reactivateTask(s, s.Tasks[ref.Task]); err != nil {
			return err
		}
	}
	m, err := r.manager(ctx)
	if err != nil {
		return err
	}
	observed, err := m.PreflightApply(ctx, plan)
	if err != nil {
		if errors.Is(err, worktree.ErrParentChanged) {
			return fail("parent_changed", err.Error())
		}
		return err
	}
	record := ApplyRecord{ID: plan.ID, Tasks: refs, Plan: plan, ObservedParent: observed, Status: "applying", Started: time.Now().UTC()}
	if err = r.update(ctx, func(s *State) error {
		if err := acceptedTasks(s, refs); err != nil {
			return err
		}
		for _, ref := range refs {
			if err := reactivateTask(s, s.Tasks[ref.Task]); err != nil {
				return err
			}
		}
		s.Applies[plan.ID] = &record
		return nil
	}); err != nil {
		return err
	}
	if err = m.RecordApply(plan.ID, record); err != nil {
		return err
	}
	// This is the commit boundary. Cancellation before it leaves a durable,
	// reconcilable intent; after it only the write timeout or lease loss stops
	// the command. Closing the runtime waits for this call before its session.
	if err = ctx.Err(); err != nil {
		return err
	}
	writeCtx, cancelWrite := context.WithTimeout(r.config.Parent.Context(), r.config.ApplyTimeout)
	finishing := context.AfterFunc(ctx, func() { r.event("integration", "", "finishing apply "+plan.ID) })
	err = m.WriteApply(writeCtx, plan)
	finishing()
	cancelWrite()
	record.Status = "applied"
	record.Finished = time.Now().UTC()
	if err != nil {
		record.Status, record.Error = "recovery_required", err.Error()
	}
	outcomeCtx, stopOutcome := context.WithTimeout(context.WithoutCancel(ctx), applyOutcomeTimeout)
	defer stopOutcome()
	manifestErr := m.RecordApply(plan.ID, record)
	storeErr := r.saveApplyOutcome(outcomeCtx, record)
	if storeErr == nil {
		var saved *State
		saved, storeErr = r.read(outcomeCtx)
		if storeErr == nil && saved.Applies[plan.ID].Status != "applied" {
			storeErr = fail("recovery_required", saved.Applies[plan.ID].Error)
		}
	}
	if err != nil || manifestErr != nil || storeErr != nil {
		return fail("recovery_required", fmt.Sprintf("application outcome requires inspection: %v", errors.Join(err, manifestErr, storeErr)))
	}
	r.event("integration", "", "applied "+plan.ID)
	return nil
}

// ReconcileApply observes an interrupted write under the same exclusion as
// apply. It never executes a patch. Only a not_applied receipt permits retry.
func (r *Runtime) ReconcileApply(ctx context.Context, id string) (*ApplyRecord, error) {
	var result *ApplyRecord
	err := r.withApplyLock(ctx, func(ctx context.Context) error {
		s, err := r.read(ctx)
		if err != nil {
			return err
		}
		prior := s.Applies[id]
		if prior == nil {
			return errors.New("unknown apply intent")
		}
		if prior.Status != "applied" {
			if err := r.reconcileRecord(ctx, *prior); err != nil {
				return err
			}
			s, err = r.read(ctx)
			if err != nil {
				return err
			}
		}
		result = s.Applies[id]
		return nil
	})
	return result, err
}

func (r *Runtime) reconcileRecord(ctx context.Context, record ApplyRecord) error {
	m, err := r.manager(ctx)
	if err != nil {
		record.Status, record.Error = "recovery_required", err.Error()
	} else {
		record.Status, err = m.ReconcileApply(ctx, record.Plan)
		record.Error = ""
		if err != nil {
			record.Error = err.Error()
		}
	}
	record.Finished = time.Now().UTC()
	return r.saveApplyOutcome(ctx, record)
}

func (r *Runtime) saveApplyOutcome(ctx context.Context, record ApplyRecord) error {
	return r.update(ctx, func(s *State) error {
		if record.Status == "applied" {
			if err := acceptedTasks(s, record.Tasks); err != nil {
				record.Status, record.Error = "recovery_required", err.Error()
			} else {
				for _, ref := range record.Tasks {
					s.Tasks[ref.Task].Status = "done"
					s.Tasks[ref.Task].Deferral = nil
				}
			}
		}
		if c := s.Integrations[record.ID]; c != nil && record.Status == "applied" {
			c.Status = "applied"
		}
		s.Applies[record.ID] = &record
		return nil
	})
}

func (r *Runtime) recoverApplies(ctx context.Context) error {
	s, err := r.read(ctx)
	if err != nil {
		return err
	}
	for _, prior := range s.Applies {
		if prior.Status != "applying" && prior.Status != "recovery_required" {
			continue
		}
		if err := r.reconcileRecord(ctx, *prior); err != nil {
			return err
		}
	}
	return nil
}
