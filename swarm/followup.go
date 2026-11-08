package swarm

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/alexschlessinger/pollytool/internal/ids"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/worktree"
)

type FollowupRequest struct {
	Task       string `json:"task"`
	Question   string `json:"question"`
	Snapshot   string `json:"snapshot,omitempty"`
	Label      string `json:"label,omitempty"`
	Background bool   `json:"background,omitempty"`
	CallID     string `json:"callID,omitempty"`
}

// Followup creates a new obligation without changing the original result.
// The caller launches it on its durable owner through Spawn or Agent.
func (r *Runtime) Followup(ctx context.Context, controller string, req FollowupRequest) (*Task, error) {
	if strings.TrimSpace(req.Question) == "" {
		return nil, fail("invalid_args", "follow-up question is required")
	}
	var original, replay *Task
	var owner *Member
	var saved *worktree.Snapshot
	var snapshot, source string
	err := r.readRecords(ctx, func(rd *recordReader) error {
		var err error
		if replay, err = rd.followupTaskReplay(req.Task, req.CallID); err != nil || replay != nil {
			return err
		}
		if req.Task != "" {
			if original, err = rd.Task(req.Task); err != nil {
				return err
			}
		}
		if original == nil || original.Status != "done" {
			status := "missing"
			if original != nil {
				status = original.Status
			}
			return fail("invalid_args", fmt.Sprintf("follow-up requires a done task; %s is %s", req.Task, status))
		}
		if original.Owner != "" {
			if owner, err = rd.Member(original.Owner); err != nil {
				return err
			}
		}
		if owner == nil {
			return fail("unknown_member", "original task owner is unavailable")
		}
		if owner.Controller != "" && owner.Controller != controller && r.workflowReserved(owner.Controller) {
			return fail("session_busy", "member is reserved by another workflow")
		}
		if err := validateRequirement(original, owner.ReadOnly); err != nil {
			return err
		}
		snapshot, source = followupSource(original, owner.ReadOnly, req.Snapshot)
		if snapshot != "" {
			saved, err = rd.Snapshot(snapshot)
		}
		return err
	})
	if err != nil || replay != nil {
		return replay, err
	}
	if err := r.validateWorkspaceSourceRecords(ctx, saved, snapshot, source); err != nil {
		return nil, err
	}
	r.launchMu.Lock()
	defer r.launchMu.Unlock()
	if r.closing {
		return nil, context.Canceled
	}
	var c *ExecutionContext
	var actualSnapshot *worktree.Snapshot
	err = r.readRecords(ctx, func(rd *recordReader) error {
		var err error
		if replay, err = rd.followupTaskReplay(req.Task, req.CallID); err != nil || replay != nil {
			return err
		}
		if owner, err = rd.Member(original.Owner); err != nil {
			return err
		}
		if err := rd.refreshReservation(original.Owner, ""); err != nil {
			return err
		}
		if err := launchRefusalRecords(rd, owner, false, false); err != nil {
			return err
		}
		if owner.Controller != "" && owner.Controller != controller && r.workflowReserved(owner.Controller) {
			return fail("session_busy", "member is reserved by another workflow")
		}
		if owner.Context != "" {
			if c, err = rd.Context(owner.Context); err != nil {
				return err
			}
		}
		if snapshot != "" {
			if saved, err = rd.Snapshot(snapshot); err != nil {
				return err
			}
		}
		if c != nil {
			actual, _ := workspaceSource(c)
			if actual != "" {
				actualSnapshot, err = rd.Snapshot(actual)
			}
		}
		return err
	})
	if err != nil || replay != nil {
		return replay, err
	}
	if c != nil && c.Release != WorkspaceReleasing {
		if c.Release == WorkspaceRetained {
			return nil, fail("workspace_retained", c.Reason)
		}
		lock := r.contextMutex(c.ID)
		if !lock.TryLock() {
			return nil, fail("context_busy", "workspace is being used or released; retry")
		}
		defer lock.Unlock()
		actual, root := workspaceSource(c)
		if !sameWorkspaceBaseline(actual, snapshot, actualSnapshot, saved) || root != source {
			return nil, workspaceMismatchRecords(c, snapshot, source, actualSnapshot, saved)
		}
		if !owner.ReadOnly && c.Checkout != nil {
			manager, err := r.manager(ctx)
			if err != nil {
				return nil, err
			}
			current, err := manager.Capture(ctx, c.Root)
			if err != nil {
				return nil, err
			}
			if saved == nil || current.Tree != saved.Tree {
				return nil, fail("workspace_mismatch", "workspace has additional edits; release it and retry")
			}
		}
	}
	r.parentTools.Lock()
	defer r.parentTools.Unlock()
	var task *Task
	err = r.updateRecords(ctx, func(tx *recordTx) error {
		prior, err := tx.Task(original.ID)
		if err != nil {
			return err
		}
		if prior == nil || prior.Status != "done" || prior.Revision != original.Revision || prior.Owner != owner.ID {
			return fail("stale_task", "original task changed during follow-up creation")
		}
		if snapshot != "" {
			saved, err := tx.Snapshot(snapshot)
			if err != nil {
				return err
			}
			if saved == nil {
				return unavailableWorkspace()
			}
		}
		run, err := r.currentRunRecords(tx)
		if err != nil {
			return err
		}
		currentOwner, err := tx.Member(prior.Owner)
		if err != nil {
			return err
		}
		requirement := (&taskDependencies{Task: prior, Owner: currentOwner}).requirement()
		task = &Task{ID: ids.New(), Run: run.ID, Follows: prior.ID, FollowupCallID: req.CallID, Owner: owner.ID, Requirement: requirement, Criteria: criteriaFor(requirement), Description: req.Question, StartingSnapshot: snapshot, SourceRoot: source, Status: "pending", Revision: 1}
		return tx.PutTask(task)
	})
	return task, err
}

func (rd *recordReader) followupTaskReplay(task, callID string) (*Task, error) {
	if callID == "" {
		return nil, nil
	}
	page, err := rd.Tasks(sessions.RecordQuery{Index: sessions.CoordinationIndexTaskFollowupCall, Key: sessions.CoordinationIndexKey(task, callID), Limit: 1})
	if err != nil || len(page.Records) == 0 {
		return nil, err
	}
	return page.Records[0].Value, nil
}

func followupSource(original *Task, readOnly bool, explicit string) (snapshot, source string) {
	if explicit != "" {
		return explicit, ""
	}
	if !readOnly && original.Snapshot != "" {
		return original.Snapshot, ""
	}
	return original.StartingSnapshot, original.SourceRoot
}

func unavailableWorkspace() error {
	return fail("workspace_unavailable", "the task's captured commit is unavailable; explicitly choose a retained commit for a new follow-up or start new work")
}

func (r *Runtime) validateWorkspaceSource(ctx context.Context, s *State, snapshot, source string) error {
	return r.validateWorkspaceSourceRecords(ctx, s.Snapshots[snapshot], snapshot, source)
}

func (r *Runtime) validateWorkspaceSourceRecords(ctx context.Context, saved *worktree.Snapshot, snapshot, source string) error {
	if snapshot != "" {
		if !validTaskSnapshot(saved, snapshot) {
			return unavailableWorkspace()
		}
		manager, err := r.manager(ctx)
		if err != nil {
			return err
		}
		if err := manager.ValidateSnapshot(ctx, *saved); err != nil {
			return unavailableWorkspace()
		}
		return nil
	}
	if source == "" {
		return fail("workspace_unavailable", "task has no saved source; start new work")
	}
	stat, err := os.Stat(source)
	if err != nil || !stat.IsDir() {
		return fail("workspace_unavailable", "the saved live root is unavailable: "+source)
	}
	return nil
}

// continuationSource also covers a newly assigned task without preset provenance.
// The last execution's resolved input is authoritative, never Request.Source.
func continuationSource(s *State, m *Member, task *Task) (snapshot, source string) {
	var deps *taskDependencies
	if task != nil {
		deps = &taskDependencies{Task: task, Owner: s.Members[task.Owner], Execution: s.Executions[task.Execution]}
	}
	return continuationSourceRecords(m, deps, s.Executions[m.Execution])
}

func workspaceMismatch(s *State, c *ExecutionContext, snapshot, source string) error {
	actual, _ := workspaceSource(c)
	return workspaceMismatchRecords(c, snapshot, source, s.Snapshots[actual], s.Snapshots[snapshot])
}
