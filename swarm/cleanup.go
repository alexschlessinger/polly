package swarm

import (
	"context"
	"errors"
	"fmt"
)

// Cleanup retires inactive execution contexts after proving their current
// files contain no unintegrated changes. An empty ID means the whole family.
// Whole-family cleanup also explicitly retires published Git snapshots;
// findings, transcript history and published artifact bytes stay in SQLite.
func (r *Runtime) Cleanup(ctx context.Context, contextID string) error {
	r.launchMu.Lock()
	defer r.launchMu.Unlock()
	r.parentTools.Lock()
	defer r.parentTools.Unlock()
	if r.HasActive() {
		return errors.New("stop active members and workflows before cleanup")
	}
	s, err := r.read(ctx)
	if err != nil {
		return err
	}
	for _, receipt := range s.Applies {
		if receipt.Status == "applying" || receipt.Status == "recovery_required" {
			return errors.New("reconcile or complete interrupted integrations before cleanup")
		}
	}

	if contextID != "" && s.Contexts[contextID] == nil {
		return fmt.Errorf("unknown execution context %q; cleanup requires the context field from list_agents, not an execution ID", contextID)
	}
	contexts := []*ExecutionContext{}
	acceptedTrees := map[string]string{}
	for _, c := range s.Contexts {
		if contextID != "" && c.ID != contextID {
			continue
		}
		contexts = append(contexts, c)
	}
	// Validate every requested context before removing any checkout.
	for _, c := range contexts {
		tree, err := r.contextCleanupTree(ctx, s, c)
		if err != nil {
			return err
		}
		acceptedTrees[c.ID] = tree
	}
	if contextID == "" && (len(s.Previews) > 0 || len(s.Snapshots) > 0) {
		manager, err := r.manager(ctx)
		if err != nil {
			return err
		}
		for _, preview := range s.Previews {
			current, err := manager.Capture(ctx, preview.Checkout.Path)
			if err != nil {
				return err
			}
			if current.Tree != preview.Checkout.Base.Tree {
				return errors.New("cleanup refuses unintegrated preview edits in " + preview.ID)
			}
		}
	}
	// One transaction records every retirement before any file changes, and
	// one deletes the records after the files are gone.
	if len(contexts) > 0 {
		if err := r.markRetiring(ctx, contexts); err != nil {
			return err
		}
		if _, err := r.finishRetirement(ctx, contexts, acceptedTrees); err != nil {
			return err
		}
	}
	if contextID == "" && r.worktrees != nil {
		// Preview checkouts contain the immutable merged result; they may have
		// deliberate conflict-resolution edits, which must be retained.
		for _, p := range s.Previews {
			if err := r.worktrees.Cleanup(ctx, p.Checkout, ""); err != nil {
				return err
			}
			if err := r.update(ctx, func(s *State) error { delete(s.Previews, p.ID); return nil }); err != nil {
				return err
			}
		}
		if err := r.worktrees.CleanupSnapshotRefs(ctx); err != nil {
			return err
		}
		return r.update(ctx, func(s *State) error { clear(s.Previews); clear(s.Snapshots); return nil })
	}
	return nil
}
