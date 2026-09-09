package swarm

import (
	"context"
	"fmt"
	"sort"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/worktree"
)

// taskSnapshots uses the task's original copy, never the current parent tree.
// After cleanup, the runtime-authored snapshot links preserve that provenance.
func taskSnapshots(s *State, task *Task) (base, submitted *worktree.Snapshot) {
	if task == nil || task.Snapshot == "" {
		return nil, nil
	}
	owner := s.Members[task.Owner]
	if owner == nil || owner.ID != task.Owner || owner.Context == "" || owner.ReadOnly {
		return nil, nil
	}
	submitted = s.Snapshots[task.Snapshot]
	if !validTaskSnapshot(submitted, task.Snapshot) {
		return nil, nil
	}
	if c := s.Contexts[owner.Context]; c != nil {
		if c.Owner != task.Owner || c.Checkout == nil || c.ReadOnly || c.Root == "" || c.Root != c.Checkout.Path || submitted.Source != c.Root {
			return nil, nil
		}
		base = s.Snapshots[c.Checkout.Base.ID]
		if base == nil || *base != c.Checkout.Base || task.StartingSnapshot != "" && task.StartingSnapshot != base.ID {
			return nil, nil
		}
	} else {
		// Cleanup pins these two exact snapshot IDs before deleting the copy.
		// Requiring new fields here would strand previously saved retirements.
		if owner.Status != "retired" || task.StartingSnapshot == "" {
			return nil, nil
		}
		base = s.Snapshots[task.StartingSnapshot]
		if !validTaskSnapshot(base, task.StartingSnapshot) {
			return nil, nil
		}
	}
	if base == nil || !validTaskSnapshot(base, base.ID) {
		return nil, nil
	}
	return base, submitted
}

func validTaskSnapshot(snapshot *worktree.Snapshot, id string) bool {
	return snapshot != nil && id != "" && snapshot.ID == id && snapshot.Tree != "" && snapshot.Commit != "" && snapshot.Source != ""
}

func unchangedTask(s *State, task *Task) bool {
	base, submitted := taskSnapshots(s, task)
	return base != nil && submitted != nil && base.Tree == submitted.Tree
}

// Explicit integration remains available for accepted unchanged inputs, even
// after review or recovery has completed them without a filesystem apply.
func integrationTask(s *State, task *Task) bool {
	return task != nil && (task.Status == "awaiting_review" || task.Status == "done" && acceptedTaskRevision(task) && unchangedTask(s, task))
}

// settlementState repairs saved, explicitly accepted no-op submissions through
// the same lease-fenced transaction and task lock as review and integration.
func (r *Runtime) settlementState(ctx context.Context) (*State, error) {
	r.parentTools.Lock()
	defer r.parentTools.Unlock()
	s, err := r.read(ctx)
	if err != nil {
		return nil, err
	}
	// An uncertain filesystem outcome must be reconciled before task recovery.
	for _, receipt := range s.Applies {
		if receipt.Status == "applying" || receipt.Status == "recovery_required" {
			return s, nil
		}
	}
	eligible := func(s *State, task *Task) bool {
		return task.Status == "awaiting_review" && !TaskDeferred(s, task) && acceptedTaskRevision(task) && unchangedTask(s, task)
	}
	for _, task := range s.Tasks {
		if eligible(s, task) {
			err = r.update(ctx, func(current *State) error {
				for _, task := range current.Tasks {
					if eligible(current, task) {
						task.Status = "done"
					}
				}
				s = current
				return nil
			})
			break
		}
	}
	return s, err
}

func unsettledTask(s *State, run string) *Task {
	ids := make([]string, 0, len(s.Tasks))
	for id, task := range s.Tasks {
		if (task.Run == run || task.Deferral != nil) && task.Status != "done" && task.Status != "canceled" && !TaskDeferred(s, task) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return nil
	}
	return s.Tasks[ids[0]]
}

func taskSettlementError(s *State, task *Task) error {
	prefix := fmt.Sprintf("task %s revision %d: ", task.ID, task.Revision)
	if e := s.Executions[task.Execution]; e != nil && e.Status == "paused" && e.StopReason == messages.StopReasonMaxIterations {
		return fmt.Errorf("%s%w", prefix, e.iterationLimitError())
	}
	operation := "inspect the task and explicitly resume its member or cancel the task"
	switch task.Status {
	case "awaiting_review":
		if acceptedTaskRevision(task) && task.Snapshot != "" {
			if base, _ := taskSnapshots(s, task); base == nil {
				operation = "accepted, but snapshot provenance is unavailable; restore the original task snapshots or cancel the task"
			} else {
				operation = "accepted; prepare, accept, and apply an integration candidate for this revision"
			}
		} else {
			operation = "awaiting parent review; accept this revision or request changes"
		}
	case "pending":
		operation = "pending; resolve its dependencies, then assign and run the task or cancel it"
	case "blocked":
		operation = "blocked; update the task to resolve its blocker or cancel it"
	case "changes_requested":
		operation = "changes requested; deliver the feedback and resume the member or cancel the task"
	default:
		if e := s.Executions[task.Execution]; e != nil {
			operation = fmt.Sprintf("execution %s is %s; inspect its saved result and explicitly resume member %s or cancel the task", e.ID, e.Status, e.Member)
		}
	}
	return fail("blocked", prefix+operation)
}
