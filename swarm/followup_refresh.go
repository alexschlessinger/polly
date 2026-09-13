package swarm

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/alexschlessinger/pollytool/internal/ids"
	"github.com/alexschlessinger/pollytool/workflow"
	"github.com/alexschlessinger/pollytool/worktree"
)

// refreshEligible is also run in the launch transaction. ownExecution is only
// the invocation being committed, never authority to ignore another worker.
func (r *Runtime) refreshEligible(s *State, m *Member, f *FollowupCall, mailID, ownExecution string) error {
	if m == nil {
		return fail("unknown_member", "refresh requires an existing worker")
	}
	if err := refreshReservation(s, m.ID, mailID); err != nil {
		return err
	}
	if r.workflowReserved(m.Controller) {
		return fail("session_busy", "worker is reserved by an active workflow; wait for it to settle or cancel it")
	}
	r.mu.Lock()
	active := r.active[m.ID]
	busy := active != nil && active.id != ownExecution
	r.mu.Unlock()
	if busy {
		return fail("session_busy", "refresh requires an idle worker; wait for its execution to finish")
	}
	if ok, why := memberReleaseEligible(s, m, nil); !ok {
		return fail("refresh_refused", "refresh requires a completed, settled assignment: "+why+"; resume, accept, integrate or reconcile the existing work first")
	}
	if pendingFollowup(s, m.ID) {
		return fail("session_busy", "worker has a pending follow-up; let that work settle before refreshing")
	}
	t := s.Tasks[m.Task]
	if t == nil || t.Owner != m.ID || t.Status != "done" {
		return fail("refresh_refused", "refresh requires the worker's previous assignment to be done; it does not accept prior work")
	}
	if f != nil && (f.PreviousTask != t.ID || f.PreviousRevision != t.Revision) {
		return fail("stale_task", "assignment changed after the refresh selected its baseline; start a new follow-up")
	}
	for _, run := range s.Runs {
		if (run.Status == "running" || run.Status == "paused") && (run.Status == "paused" || run.Starts >= run.Limit) {
			return ErrBudget
		}
	}
	return validateRequirement(t, m.ReadOnly)
}

// refreshFollowup pins intent before release. Preparation is never Start mail:
// only the assignment/execution transaction can make it runnable. Maintenance
// precedes launchMu, as it does for release and forget.
func (r *Runtime) refreshFollowup(ctx context.Context, target, message, callID string) (*Mail, error) {
	r.launchMu.Lock()
	if r.closing {
		r.launchMu.Unlock()
		return nil, context.Canceled
	}
	r.wg.Add(1)
	r.launchMu.Unlock()
	defer r.wg.Done()
	unlock, err := r.lockMaintenance(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	r.launchMu.Lock()
	defer r.launchMu.Unlock()
	if r.closing || r.ctx.Err() != nil {
		return nil, context.Canceled
	}
	if err := r.prepare(ctx); err != nil {
		return nil, err
	}
	s, err := r.read(ctx)
	if err != nil {
		return nil, err
	}
	id, err := r.resolveTarget(s, r.ID, target)
	if err != nil {
		return nil, err
	}
	if id == r.ID {
		return nil, errors.New("followup_task requires a child target")
	}
	mail, err := followupReplay(s, r.ID, id, message, callID, true)
	if err != nil {
		return nil, err
	}
	var f *FollowupCall
	if mail != nil {
		f = s.Followups[mail.ID]
		if f.Phase == "launched" {
			return mail, nil
		}
	}
	m := s.Members[id]
	mailID := ""
	if mail != nil {
		mailID = mail.ID
	}
	if err := r.refreshEligible(s, m, f, mailID, ""); err != nil {
		if mail != nil {
			err = errors.Join(err, r.failFollowup(ctx, mail.ID, err))
		}
		return nil, err
	}
	var captured *worktree.Snapshot
	if f == nil {
		t := s.Tasks[m.Task]
		base, source := continuationSource(s, m, t)
		f = &FollowupCall{Member: id, Refresh: true, Phase: "preparing", PreviousTask: t.ID, PreviousRevision: t.Revision, Operation: "new_task", Task: ids.New(), BaseOrigin: "parent"}
		if base != "" {
			manager, err := r.manager(ctx)
			if err != nil {
				return nil, err
			}
			capture, err := manager.Capture(ctx, r.config.Root)
			if err != nil {
				return nil, err
			}
			captured, f.Base = &capture, capture.ID
		} else {
			if !m.ReadOnly {
				return nil, unavailableWorkspace()
			}
			if err := r.validateWorkspaceSource(ctx, s, "", source); err != nil {
				return nil, err
			}
			f.Source, f.BaseOrigin = source, "live_source"
		}
		mail = &Mail{ID: ids.New(), From: r.ID, To: id, Kind: "info", Text: message, CallID: callID, Posted: time.Now().UTC()}
	}
	// Reserve even on retry, with the original selection. No task exists yet.
	err = r.update(ctx, func(s *State) error {
		if err := r.refreshEligible(s, s.Members[id], f, mail.ID, ""); err != nil {
			return err
		}
		if captured != nil {
			s.Snapshots[captured.ID] = captured
		} else if f.Base != "" && !validTaskSnapshot(s.Snapshots[f.Base], f.Base) {
			return unavailableWorkspace()
		}
		f.Phase = "preparing"
		s.Followups[mail.ID], s.Messages[mail.ID] = f, mail
		return nil
	})
	if err == nil {
		err = r.prepareRefreshWorkspace(ctx, mail.ID)
	}
	if err == nil {
		_, err = r.startLocked(ctx, "", AgentRequest{Session: id, Task: "Complete the new assignment in your addressed follow-up. Inspect your current workspace before relying on earlier file descriptions."}, launchIntent{resume: true, followup: mail.ID})
	}
	if err != nil {
		return nil, errors.Join(err, r.failFollowup(ctx, mail.ID, err))
	}
	r.changed()
	return mail, nil
}

// Caller holds maintenance and launchMu. Reuse release's filesystem proofs,
// context lock and durable removal path, without a family-wide cleanup pass.
func (r *Runtime) prepareRefreshWorkspace(ctx context.Context, mailID string) error {
	s, err := r.read(ctx)
	if err != nil {
		return err
	}
	f := s.Followups[mailID]
	if f == nil || !f.Refresh || f.Phase != "preparing" {
		return fail("stale_task", "refresh preparation is no longer available; retry explicitly")
	}
	m := s.Members[f.Member]
	if err := r.refreshEligible(s, m, f, mailID, ""); err != nil {
		return err
	}
	if err := r.validateWorkspaceSource(ctx, s, f.Base, f.Source); err != nil {
		return err
	}
	c := s.Contexts[m.Context]
	if c == nil {
		return nil
	}
	if c.Owner != m.ID {
		return fail("workspace_changed", "worker does not own its recorded workspace")
	}
	if c.Release == WorkspaceRetained {
		return &workflow.Error{Code: "workspace_retained", Message: fmt.Sprintf("refresh preserves the retained workspace at %s: %s; preserve its edits, then use client /swarm cleanup %s once safe before a new refresh", c.Root, c.Reason, c.ID), Result: map[string]any{"context": c.ID, "root": c.Root, "reason": c.Reason}}
	}
	lock := r.contextMutex(c.ID)
	if !lock.TryLock() {
		return fail("context_busy", "worker's workspace is in use; retry after the operation finishes")
	}
	defer lock.Unlock()
	defer r.notifyRelease(c.ID)
	tree, err := r.contextCleanupTree(ctx, s, c)
	if err != nil {
		return err
	}
	r.parentTools.Lock()
	err = r.update(ctx, func(s *State) error {
		if err := r.refreshEligible(s, s.Members[f.Member], f, mailID, ""); err != nil {
			return err
		}
		stored := s.Contexts[c.ID]
		if stored == nil || stored.Owner != f.Member || s.Members[f.Member].Context != c.ID || stored.Release == WorkspaceRetained {
			return fail("workspace_changed", "worker's workspace changed during refresh; retry")
		}
		stored.Release = WorkspaceReleasing
		return nil
	})
	r.parentTools.Unlock()
	if err != nil {
		return err
	}
	_, err = r.finishRelease(ctx, []*ExecutionContext{c}, map[string]string{c.ID: tree})
	return err
}

// The pending task is constructed for workspace selection, then inserted only
// inside the transaction committing the execution and launch receipt.
func (r *Runtime) refreshLaunchTask(s *State, m *Member, mailID, ownExecution string) (*Task, error) {
	f, mail := s.Followups[mailID], s.Messages[mailID]
	if f == nil || mail == nil || !f.Refresh || f.Phase != "preparing" || f.Member != m.ID {
		return nil, fail("stale_task", "refresh preparation is no longer available; retry explicitly")
	}
	if err := r.refreshEligible(s, m, f, mailID, ownExecution); err != nil {
		return nil, err
	}
	if f.Base != "" && !validTaskSnapshot(s.Snapshots[f.Base], f.Base) {
		return nil, unavailableWorkspace()
	}
	prior := s.Tasks[f.PreviousTask]
	return &Task{ID: f.Task, Follows: prior.ID, Owner: m.ID, Requirement: requirementOf(s, prior), Criteria: criteriaFor(requirementOf(s, prior)), Description: mail.Text, StartingSnapshot: f.Base, SourceRoot: f.Source, Status: "pending"}, nil
}
