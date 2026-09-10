package swarm

import (
	"context"
	"fmt"
	"sort"

	"errors"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/workflow"
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
		if owner.Control != MemberControlRetired || task.StartingSnapshot == "" {
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

// acceptTask records the parent's acceptance of the current submitted
// revision inside the caller's transaction. Snapshot-less research and an
// unchanged candidate finish immediately; a changed candidate stays awaiting
// its integration receipt. Accepting always reactivates retained work first.
func acceptTask(s *State, t *Task) error {
	if err := reactivateTask(s, t); err != nil {
		return err
	}
	t.AcceptedRevision = t.Revision
	if t.Snapshot == "" || unchangedTask(s, t) {
		t.Status = "done"
	}
	return nil
}

// workflowResearchTasks lists the read-only, snapshot-less submissions a
// workflow's completed executions left awaiting review: research the script
// consumed without reviewing. Results the script already reviewed and editing
// candidates are never included. Sorted by task ID.
func workflowResearchTasks(s *State, w *workflow.Report) []*Task {
	var tasks []*Task
	for _, t := range s.Tasks {
		if t.Run != w.Run || t.Status != "awaiting_review" || t.Snapshot != "" {
			continue
		}
		e, owner := s.Executions[t.Execution], s.Members[t.Owner]
		if e == nil || e.Workflow != w.ID || e.Run != w.Run || e.Member != t.Owner || e.Status != "completed" || owner == nil || !owner.ReadOnly {
			continue
		}
		tasks = append(tasks, t)
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].ID < tasks[j].ID })
	return tasks
}

// countNoun formats "1 research result" or "40 research results".
func countNoun(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
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

// unsettledTasks lists, by ID, the current run's open tasks plus retained
// tasks whose deferral no longer holds.
func unsettledTasks(s *State, run string) []*Task {
	var tasks []*Task
	for _, task := range s.Tasks {
		if (task.Run == run || task.Deferral != nil) && task.Status != "done" && task.Status != "canceled" && !TaskDeferred(s, task) {
			tasks = append(tasks, task)
		}
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].ID < tasks[j].ID })
	return tasks
}

// unsettledTasksError names the first task's blocker and, when several tasks
// are open, how many. The single-task text is unchanged, and the typed
// blocker (its code, or the iteration limit) survives the count prefix.
func unsettledTasksError(s *State, tasks []*Task) error {
	first := taskSettlementError(s, tasks[0])
	if len(tasks) == 1 {
		return first
	}
	count := fmt.Sprintf("%d tasks unsettled; first: ", len(tasks))
	var blocker *workflow.Error
	if errors.As(first, &blocker) {
		return fail(blocker.Code, count+blocker.Message)
	}
	return fmt.Errorf("%s%w", count, first)
}

// unacknowledgedResearch returns the first (by ID) completed, unacknowledged
// workflow of the run that still owns consumed research, with that research.
func unacknowledgedResearch(s *State, run string) (*workflow.Report, []*Task) {
	for _, id := range sortedInspectionIDs(s.Workflows) {
		w := s.Workflows[id]
		if w.Run != run || w.Status != "completed" || w.Acknowledged {
			continue
		}
		if research := workflowResearchTasks(s, w); len(research) > 0 {
			return w, research
		}
	}
	return nil, nil
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
