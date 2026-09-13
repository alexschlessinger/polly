package swarm

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/alexschlessinger/pollytool/internal/ids"
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
	s, err := r.read(ctx)
	if err != nil {
		return nil, err
	}
	if req.CallID != "" {
		for _, t := range s.Tasks {
			if t.Follows == req.Task && t.FollowupCallID == req.CallID {
				return t, nil
			}
		}
	}
	original := s.Tasks[req.Task]
	if original == nil || original.Status != "done" {
		status := "missing"
		if original != nil {
			status = original.Status
		}
		return nil, fail("invalid_args", fmt.Sprintf("follow-up requires a done task; %s is %s", req.Task, status))
	}
	owner := s.Members[original.Owner]
	if owner == nil {
		return nil, fail("unknown_member", "original task owner is unavailable")
	}
	if owner.Controller != "" && owner.Controller != controller && r.workflowReserved(owner.Controller) {
		return nil, fail("session_busy", "member is reserved by another workflow")
	}
	if err := validateRequirement(original, owner.ReadOnly); err != nil {
		return nil, err
	}
	snapshot, source := followupSource(original, owner.ReadOnly, req.Snapshot)
	if err := r.validateWorkspaceSource(ctx, s, snapshot, source); err != nil {
		return nil, err
	}
	r.launchMu.Lock()
	defer r.launchMu.Unlock()
	if r.closing {
		return nil, context.Canceled
	}
	s, err = r.read(ctx)
	if err != nil {
		return nil, err
	}
	if req.CallID != "" {
		for _, t := range s.Tasks {
			if t.Follows == req.Task && t.FollowupCallID == req.CallID {
				return t, nil
			}
		}
	}
	owner = s.Members[original.Owner]
	if err := refreshReservation(s, original.Owner, ""); err != nil {
		return nil, err
	}
	if err := launchRefusal(s, owner, false); err != nil {
		return nil, err
	}
	if owner.Controller != "" && owner.Controller != controller && r.workflowReserved(owner.Controller) {
		return nil, fail("session_busy", "member is reserved by another workflow")
	}
	if c := s.Contexts[owner.Context]; c != nil && c.Release != WorkspaceReleasing {
		if c.Release == WorkspaceRetained {
			return nil, fail("workspace_retained", c.Reason)
		}
		lock := r.contextMutex(c.ID)
		if !lock.TryLock() {
			return nil, fail("context_busy", "workspace is being used or released; retry")
		}
		defer lock.Unlock()
		actual, root := workspaceSource(c)
		if !sameBaseline(s, actual, snapshot) || root != source {
			return nil, workspaceMismatch(s, c, snapshot, source)
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
			if current.Tree != s.Snapshots[snapshot].Tree {
				return nil, fail("workspace_mismatch", "workspace has additional edits; release it and retry")
			}
		}
	}
	r.parentTools.Lock()
	defer r.parentTools.Unlock()
	var task *Task
	err = r.update(ctx, func(current *State) error {
		prior := current.Tasks[original.ID]
		if prior == nil || prior.Status != "done" || prior.Revision != original.Revision || prior.Owner != owner.ID {
			return fail("stale_task", "original task changed during follow-up creation")
		}
		if snapshot != "" && current.Snapshots[snapshot] == nil {
			return unavailableWorkspace()
		}
		requirement := requirementOf(current, prior)
		task = &Task{ID: ids.New(), Run: r.currentRun(current).ID, Follows: prior.ID, FollowupCallID: req.CallID, Owner: owner.ID, Requirement: requirement, Criteria: criteriaFor(requirement), Description: req.Question, StartingSnapshot: snapshot, SourceRoot: source, Status: "pending", Revision: 1}
		current.Tasks[task.ID] = task
		return nil
	})
	return task, err
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
	if snapshot != "" {
		saved := s.Snapshots[snapshot]
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
	if task != nil {
		if task.Status == "done" || deliveringTask(s, task) {
			return followupSource(task, m.ReadOnly, "")
		}
		if task.StartingSnapshot != "" || task.SourceRoot != "" {
			return task.StartingSnapshot, task.SourceRoot
		}
	}
	if e := s.Executions[m.Execution]; e != nil {
		return e.Base, e.SourceRoot
	}
	return "", ""
}

func workspaceMismatch(s *State, c *ExecutionContext, snapshot, source string) error {
	actual, root := workspaceSource(c)
	actual = snapshotCommit(s, actual)
	if actual == "" {
		actual = root
		if actual == "" {
			actual = "an unavailable captured commit"
		}
	}
	required := snapshotCommit(s, snapshot)
	if required == "" {
		required = source
		if required == "" {
			required = "an unavailable captured commit"
		}
	}
	return fail("workspace_mismatch", fmt.Sprintf("member's workspace uses %s; this follow-up requires %s; release the workspace and retry", actual, required))
}
