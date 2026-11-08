package swarm

import (
	"context"
	"fmt"
)

// ensureWorkspace resolves provenance before looking at the current workspace.
// The caller holds launchMu; context access is tried, never awaited under it.
func (r *Runtime) ensureWorkspace(ctx context.Context, s *State, m *Member, task *Task, req AgentRequest) (observed string, fresh *ExecutionContext, unlock func(), err error) {
	var deps *taskDependencies
	if task != nil {
		deps = &taskDependencies{Task: task, Owner: s.Members[task.Owner], Execution: s.Executions[task.Execution]}
	}
	base, source := continuationSourceRecords(m, deps, s.Executions[m.Execution])
	c := s.Contexts[m.Context]
	actual := ""
	if c != nil {
		actual, _ = workspaceSource(c)
	}
	b := &memberWorkspace{Member: m, Context: c, Execution: s.Executions[m.Execution], Task: deps, Base: base, Source: source, Snapshot: s.Snapshots[base], ContextSnapshot: s.Snapshots[actual]}
	return r.ensureWorkspaceRecords(ctx, b, m, task, req)
}

func (r *Runtime) ensureWorkspaceRecords(ctx context.Context, b *memberWorkspace, m *Member, task *Task, req AgentRequest) (observed string, fresh *ExecutionContext, unlock func(), err error) {
	observed = m.Context
	if task != nil {
		if err = validateRequirement(task, m.ReadOnly); err != nil {
			return
		}
	}
	snapshot, source := b.Base, b.Source
	if err = r.validateWorkspaceSourceRecords(ctx, b.Snapshot, snapshot, source); err != nil {
		return
	}
	c := b.Context
	if c != nil && c.Release == WorkspaceRetained {
		err = fail("workspace_retained", "member's workspace was retained: "+c.Reason)
		return
	}
	if c == nil || c.Release == WorkspaceReleasing {
		presets := m.SandboxPresets
		if c != nil {
			presets = sandboxPresets(presets, c.SandboxPresets...)
		}
		presets = sandboxPresets(presets, r.config.AgentSandbox)
		fresh, err = r.makeContextFromSource(ctx, m.ID, AgentRequest{ReadOnly: m.ReadOnly, Snapshot: snapshot, Source: source, sandboxPresets: presets}, snapshot == "")
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
	actualSnapshot := b.ContextSnapshot
	if fresh != nil {
		actualSnapshot = b.Snapshot
	}
	if !sameWorkspaceBaseline(actual, snapshot, actualSnapshot, b.Snapshot) || root != source {
		err = workspaceMismatchRecords(c, snapshot, source, actualSnapshot, b.Snapshot)
		return
	}
	linked := task != nil && (task.Follows != "" && task.Status == "pending" || task.Status == "done" || b.Task.delivering())
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
		if b.Snapshot == nil || current.Tree != b.Snapshot.Tree {
			err = fail("workspace_mismatch", fmt.Sprintf("member's workspace contains edits beyond commit %s; release the workspace and retry", capturedCommit(b.Snapshot, snapshot)))
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
	err := r.updateRecords(context.WithoutCancel(r.ctx), func(tx *recordTx) error {
		current, err := tx.Context(c.ID)
		if err != nil || current == nil {
			return err
		}
		if current.Owner != "" {
			m, err := tx.Member(current.Owner)
			if err != nil {
				return err
			}
			if m != nil && m.Context == current.ID && m.Execution != "" {
				return nil
			}
		}
		if followup != "" {
			f, err := tx.Followup(followup)
			if err != nil {
				return err
			}
			if f != nil && f.HeldScratch != "" {
				held, _ := r.parkScratch(followup, current)
				if held == "" {
					// A failed restoration must not delete the only remaining copy.
					current.Release = WorkspaceRetained
					current.Reason = "refresh scratch could not be parked again after launch failed"
					return tx.PutContext(current)
				}
				f.HeldScratch = held
				if err := tx.PutFollowup(followup, f); err != nil {
					return err
				}
			}
		}
		current.Release = WorkspaceReleasing
		return tx.PutContext(current)
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
