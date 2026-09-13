package swarm

import "errors"

func workspaceSource(c *ExecutionContext) (snapshot, liveRoot string) {
	if c.Checkout != nil {
		return c.Checkout.Base.ID, ""
	}
	return "", c.Root
}

// assignTask binds provenance before work can start. A preset source is an
// obligation, not a hint that can be replaced by whichever workspace is live.
func assignTask(s *State, t *Task, m *Member, c *ExecutionContext, executionID string) error {
	for _, f := range s.Followups {
		if f.Refresh && f.Phase == "preparing" && f.Member == m.ID && f.Task != t.ID {
			return fail("session_busy", "member has a refresh in preparation")
		}
	}
	if err := unresolvedTaskApply(s, t.ID); err != nil {
		return err
	}
	if c == nil || c.Release != "" || c.Owner != m.ID || c.Root == "" {
		return errors.New("member's execution context is unavailable")
	}
	if err := validateRequirement(t, m.ReadOnly); err != nil {
		return err
	}
	base, source := workspaceSource(c)
	if t.StartingSnapshot != "" && !sameBaseline(s, t.StartingSnapshot, base) || t.SourceRoot != "" && t.SourceRoot != source {
		return fail("workspace_mismatch", "task's required source does not match the member's workspace; release it and retry")
	}
	if t.Execution != executionID {
		// Prior submissions remain in their execution and snapshot records.
		// A new revision must capture its own result before it is reviewable.
		t.Result, t.Snapshot = nil, ""
	}
	t.Owner, t.Status, t.Execution = m.ID, "running", executionID
	t.StartingSnapshot, t.SourceRoot = base, source
	t.Revision++
	t.AcceptedRevision, t.Delivery = 0, nil
	m.Task = t.ID
	return nil
}

func unresolvedTaskApply(s *State, taskID string) error {
	for _, apply := range s.Applies {
		if apply.Status != "applying" && apply.Status != "recovery_required" {
			continue
		}
		for _, ref := range apply.Tasks {
			if ref.Task == taskID {
				return fail("blocked", "task has an unresolved integration; inspect and reconcile it through polly.integration.reconcile in workflow_run before continuing")
			}
		}
	}
	return nil
}
