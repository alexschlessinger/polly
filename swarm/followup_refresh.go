package swarm

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/alexschlessinger/pollytool/internal/ids"
	"github.com/alexschlessinger/pollytool/internal/scratch"
	"github.com/alexschlessinger/pollytool/workflow"
	"github.com/alexschlessinger/pollytool/worktree"
	"log/slog"
	"os"
	"path/filepath"
)

// refreshEligible is also run in the launch transaction. ownExecution is only
// the invocation being committed, never authority to ignore another worker.
func (r *Runtime) refreshEligible(s *State, m *Member, f *FollowupCall, mailID, ownExecution string) error {
	if m == nil {
		return fail("unknown_member", "refresh requires an existing worker")
	}
	if err := userStopRefusal(m); err != nil {
		return err
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
		if run.Status == "paused" || run.Status == "running" && run.Starts >= run.Limit {
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
	var id string
	var mail *Mail
	var f *FollowupCall
	var m *Member
	var b *memberWorkspace
	err = r.readRecords(ctx, func(rd *recordReader) error {
		var err error
		if id, err = rd.resolveTarget(r.ID, r.ID, target); err != nil {
			return err
		}
		if id == r.ID {
			return errors.New("followup_task requires a child target")
		}
		if mail, err = rd.followupReplay(r.ID, id, message, callID, true); err != nil {
			return err
		}
		mailID := ""
		if mail != nil {
			mailID = mail.ID
			if f, err = rd.Followup(mail.ID); err != nil {
				return err
			}
			if f != nil && f.Phase == "launched" {
				return nil
			}
		}
		if m, err = rd.Member(id); err != nil {
			return err
		}
		if err := r.refreshEligibleRecords(rd, m, f, mailID, ""); err != nil {
			return err
		}
		t, err := rd.Task(m.Task)
		if err != nil {
			return err
		}
		b, err = rd.MemberWorkspace(m, t)
		return err
	})
	if err != nil {
		if mail != nil {
			err = errors.Join(err, r.failFollowup(ctx, mail.ID, err))
		}
		return nil, err
	}
	if f != nil && f.Phase == "launched" {
		return mail, nil
	}
	var captured *worktree.Snapshot
	if f == nil {
		t := b.Task.Task
		base, source := b.Base, b.Source
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
			if err := r.validateWorkspaceSourceRecords(ctx, nil, "", source); err != nil {
				return nil, err
			}
			f.Source, f.BaseOrigin = source, "live_source"
		}
		mail = &Mail{ID: ids.New(), From: r.ID, To: id, Kind: "info", Text: message, CallID: callID, Posted: time.Now().UTC()}
	}
	// Reserve even on retry, with the original selection. No task exists yet.
	err = r.updateRecords(ctx, func(tx *recordTx) error {
		member, err := tx.Member(id)
		if err != nil {
			return err
		}
		if err := r.refreshEligibleRecords(tx.recordReader, member, f, mail.ID, ""); err != nil {
			return err
		}
		if captured != nil {
			if err := tx.PutSnapshot(captured); err != nil {
				return err
			}
		} else if f.Base != "" {
			snapshot, err := tx.Snapshot(f.Base)
			if err != nil {
				return err
			}
			if !validTaskSnapshot(snapshot, f.Base) {
				return unavailableWorkspace()
			}
		}
		f.Phase = "preparing"
		if err := tx.PutFollowup(mail.ID, f); err != nil {
			return err
		}
		return tx.PutMail(mail)
	})
	if err == nil {
		err = r.prepareRefreshWorkspace(ctx, mail.ID)
	}
	if err == nil {
		_, err = r.startLocked(ctx, "", AgentRequest{Session: id, Task: "Complete the new assignment from your parent below. Inspect your current workspace before relying on earlier file descriptions."}, launchIntent{resume: true, followup: mail.ID})
	}
	if err != nil {
		if uncertainReleaseCommit(err) {
			// The mark/removal may have committed. Failure cleanup would erase
			// its HeldScratch receipt and delete the only surviving scratch.
			return nil, err
		}
		return nil, errors.Join(err, r.failFollowup(ctx, mail.ID, err))
	}
	return mail, nil
}

// Caller holds maintenance and launchMu. Reuse release's filesystem proofs,
// context lock and durable removal path, without a family-wide cleanup pass.
func (r *Runtime) prepareRefreshWorkspace(ctx context.Context, mailID string) error {
	var f *FollowupCall
	var m *Member
	var c *ExecutionContext
	var saved *worktree.Snapshot
	err := r.readRecords(ctx, func(rd *recordReader) error {
		var err error
		if f, err = rd.Followup(mailID); err != nil {
			return err
		}
		if f == nil || !f.Refresh || f.Phase != "preparing" {
			return fail("stale_task", "refresh preparation is no longer available; retry explicitly")
		}
		if m, err = rd.Member(f.Member); err != nil {
			return err
		}
		if err := r.refreshEligibleRecords(rd, m, f, mailID, ""); err != nil {
			return err
		}
		if f.Base != "" {
			if saved, err = rd.Snapshot(f.Base); err != nil {
				return err
			}
		}
		if m.Context != "" {
			c, err = rd.Context(m.Context)
		}
		return err
	})
	if err != nil {
		return err
	}
	if err := r.validateWorkspaceSourceRecords(ctx, saved, f.Base, f.Source); err != nil {
		return err
	}
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
	tree, err := r.contextCleanupTreeRecords(ctx, c)
	if err != nil {
		return err
	}
	// The worker keeps its scratch across the refresh: it is parked under
	// the scratch root before the workspace goes, and the launch adopts it
	// into the new workspace. A hold outlives a crash: the record names it,
	// the retry adopts it, and a stale one is swept at the next start.
	held, carry := r.parkScratch(mailID, c)
	callbackSucceeded := false
	r.parentTools.Lock()
	err = r.updateRecords(ctx, func(tx *recordTx) error {
		member, err := tx.Member(f.Member)
		if err != nil {
			return err
		}
		if err := r.refreshEligibleRecords(tx.recordReader, member, f, mailID, ""); err != nil {
			return err
		}
		stored, err := tx.Context(c.ID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(stored, c) || stored.Owner != f.Member || member.Context != c.ID || stored.Release == WorkspaceRetained {
			return fail("workspace_changed", "worker's workspace changed during refresh; retry")
		}
		current, err := tx.Followup(mailID)
		if err != nil {
			return err
		}
		if current == nil || current.Member != f.Member || current.Phase != "preparing" || current.PreviousTask != f.PreviousTask || current.PreviousRevision != f.PreviousRevision {
			return fail("stale_task", "refresh preparation changed; retry explicitly")
		}
		stored.Release = WorkspaceReleasing
		if held != "" {
			current.HeldScratch = held
		}
		if carry != "" {
			current.ScratchCarry = carry
		}
		if err := tx.PutFollowup(mailID, current); err != nil {
			return err
		}
		err = tx.PutContext(stored)
		callbackSucceeded = err == nil
		return err
	})
	r.parentTools.Unlock()
	if err != nil {
		if callbackSucceeded {
			r.quarantineRelease([]*releaseProof{{context: c}})
			err = &releaseCommitUncertain{cause: err}
		}
		if held != "" && !callbackSucceeded {
			// A definite veto/rollback restores the old workspace's scratch.
			// An uncertain commit keeps the owned hold: the saved refresh may
			// already name it, and adopting it back would strand that receipt.
			if e := scratch.Adopt(held, c.Scratch); e != nil {
				slog.Warn("refresh_scratch_lost", "member", f.Member, "error", e)
			}
		}
		return err
	}
	return r.finishRefreshRelease(c, tree)
}

// heldScratchName is the hold a refresh parks a worker's scratch under.
func heldScratchName(swarmID, mailID string) string { return "held-" + swarmID + "-" + mailID }

// parkScratch moves a workspace's scratch aside for the refresh's launch to
// adopt. It reports the hold and the outcome for the brief: "carried" once
// parked, "lost" when the move failed and the scratch goes with the
// workspace, and nothing when there was no scratch to keep.
func (r *Runtime) parkScratch(mailID string, c *ExecutionContext) (held, carry string) {
	if c.Scratch == "" {
		return "", ""
	}
	if _, err := os.Stat(c.Scratch); err != nil {
		return "", ""
	}
	dir, err := r.runtimeDirectory()
	if err != nil {
		slog.Warn("refresh_scratch_lost", "member", c.Owner, "error", err)
		return "", "lost"
	}
	// The record names a path under the runtime directory, which exists for
	// as long as the swarm does: a sweep reclaims the hold only after that.
	held, err = scratch.Park(c.Scratch, heldScratchName(r.ID, mailID), filepath.Join(dir, heldScratchName(r.ID, mailID)))
	if err != nil {
		slog.Warn("refresh_scratch_lost", "member", c.Owner, "error", err)
		return "", "lost"
	}
	return held, "carried"
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
