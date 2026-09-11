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
	if c == nil || c.Release != "" || c.Owner != m.ID || c.Root == "" {
		return errors.New("member's execution context is unavailable")
	}
	if err := validateRequirement(t, m.ReadOnly); err != nil {
		return err
	}
	base, source := workspaceSource(c)
	if t.StartingSnapshot != "" && t.StartingSnapshot != base || t.SourceRoot != "" && t.SourceRoot != source {
		return fail("workspace_mismatch", "task's required source does not match the member's workspace; release it and retry")
	}
	t.Owner, t.Status, t.Execution = m.ID, "running", executionID
	t.StartingSnapshot, t.SourceRoot = base, source
	t.Revision++
	t.AcceptedRevision, t.Delivery = 0, nil
	m.Task = t.ID
	return nil
}
