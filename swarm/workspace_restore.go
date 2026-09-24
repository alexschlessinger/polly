package swarm

import (
	"context"
	"fmt"
)

// ensureWorkspace resolves provenance before looking at the current workspace.
// The caller holds launchMu; context access is tried, never awaited under it.
func (r *Runtime) ensureWorkspace(ctx context.Context, s *State, m *Member, task *Task, req AgentRequest) (observed string, fresh *ExecutionContext, unlock func(), err error) {
	observed = m.Context
	if task != nil {
		if err = validateRequirement(task, m.ReadOnly); err != nil {
			return
		}
	}
	snapshot, source := continuationSource(s, m, task)
	if err = r.validateWorkspaceSource(ctx, s, snapshot, source); err != nil {
		return
	}
	c := s.Contexts[observed]
	if c != nil && c.Release == WorkspaceRetained {
		err = fail("workspace_retained", "member's workspace was retained: "+c.Reason)
		return
	}
	if c == nil || c.Release == WorkspaceReleasing {
		fresh, err = r.makeContextFromSource(ctx, m.ID, AgentRequest{ReadOnly: m.ReadOnly, Snapshot: snapshot, Source: source}, snapshot == "")
		if err != nil {
			return
		}
		c = fresh
	}
	lock := r.contextMutex(c.ID)
	if !lock.TryLock() {
		err = fail("context_busy", "workspace is being used or released; retry")
		return
	}
	unlock = lock.Unlock
	actual, root := workspaceSource(c)
	if !sameBaseline(s, actual, snapshot) || root != source {
		err = workspaceMismatch(s, c, snapshot, source)
		return
	}
	linked := task != nil && (task.Follows != "" && task.Status == "pending" || task.Status == "done" || deliveringTask(s, task))
	if linked && !m.ReadOnly && fresh == nil && c.Checkout != nil {
		manager, e := r.manager(ctx)
		if e != nil {
			err = e
			return
		}
		current, e := manager.Capture(ctx, c.Root)
		if e != nil {
			err = e
			return
		}
		if current.Tree != s.Snapshots[snapshot].Tree {
			err = fail("workspace_mismatch", fmt.Sprintf("member's workspace contains edits beyond commit %s; release the workspace and retry", snapshotCommit(s, snapshot)))
			return
		}
	}
	return
}

// rollbackWorkspace leaves a durable cleanup obligation. The coalesced worker
// obtains global locks only after the launch wrapper has returned and unlocked.
func (r *Runtime) rollbackWorkspace(c *ExecutionContext) {
	r.rollbackWorkspaceWithScratch(c, "")
}

// A refresh may have adopted its parked scratch before its launch failed.
// Restore the hold only after proving the new workspace was not assigned:
// an error confirming a committed launch must leave that workspace intact.
func (r *Runtime) rollbackWorkspaceWithScratch(c *ExecutionContext, followup string) {
	if c == nil {
		return
	}
	err := r.update(context.WithoutCancel(r.ctx), func(s *State) error {
		current := s.Contexts[c.ID]
		if current == nil {
			return nil
		}
		if m := s.Members[current.Owner]; m != nil && m.Context == current.ID && m.Execution != "" {
			return nil
		}
		if f := s.Followups[followup]; f != nil && f.HeldScratch != "" {
			held, _ := r.parkScratch(followup, current)
			if held == "" {
				// A failed restoration must not delete the only remaining copy.
				current.Release = WorkspaceRetained
				current.Reason = "refresh scratch could not be parked again after launch failed"
				return nil
			}
			f.HeldScratch = held
		}
		current.Release = WorkspaceReleasing
		return nil
	})
	if err != nil {
		r.event("release_incomplete", c.Owner, err.Error())
		return
	}
	r.scheduleRelease()
}

func workspaceBrief(c *ExecutionContext) string {
	snapshot, source := workspaceSource(c)
	from := "observing the same live root " + source
	if snapshot != "" {
		from = "from commit " + c.Checkout.Base.Commit
	}
	return "Your workspace was recreated at " + c.Root + " " + from + "; scratch is " + c.Scratch + ". Use these current paths.\n\n"
}
