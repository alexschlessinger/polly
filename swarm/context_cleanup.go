package swarm

import (
	"context"
	"errors"
	"time"

	"github.com/alexschlessinger/pollytool/tools/sandbox"
	"github.com/alexschlessinger/pollytool/workflow"
	"os"
	"path/filepath"
)

// contextCleanupTree proves the copy is unchanged or exactly matches an
// integrated task. Callers enforce their own activity and ownership rules and
// hold parentTools while checking and retiring the context.
func (r *Runtime) contextCleanupTree(ctx context.Context, s *State, c *ExecutionContext) (string, error) {
	if c.Checkout == nil {
		return "", nil
	}
	m, err := r.manager(ctx)
	if err != nil {
		return "", err
	}
	current, err := m.Capture(ctx, c.Root)
	if err != nil {
		return "", err
	}
	if current.Tree == c.Checkout.Base.Tree {
		return current.Tree, nil
	}
	for _, task := range s.Tasks {
		snapshot := s.Snapshots[task.Snapshot]
		if task.Status == "done" && snapshot != nil && snapshot.Source == c.Root && snapshot.Tree == current.Tree {
			return current.Tree, nil
		}
	}
	return "", &workflow.Error{Code: "unintegrated_changes", Message: "context contains unintegrated edits; retained at " + c.Root, Result: map[string]any{"context": c.ID, "root": c.Root}}
}

// retireContext records provenance and retirement before removing any files.
// Callers hold launchMu and parentTools and exclude active context operations.
// Worktree cleanup rechecks the approved tree. Snapshots and publications stay
// pinned; only explicit whole-family cleanup may retire their Git references.
func (r *Runtime) retireContext(ctx context.Context, c *ExecutionContext, tree string) error {
	if err := r.update(ctx, func(s *State) error {
		stored := s.Contexts[c.ID]
		if stored == nil {
			return errors.New("unknown execution context")
		}
		for _, task := range s.Tasks {
			if task.Owner == c.Owner && task.StartingSnapshot == "" && c.Checkout != nil {
				task.StartingSnapshot = c.Checkout.Base.ID
			}
		}
		stored.Retiring = true
		if member := s.Members[c.Owner]; member != nil {
			member.Control = MemberControlRetired
		}
		return nil
	}); err != nil {
		return err
	}
	// A canceled caller cannot strand cleanup halfway through retirement.
	// Parent lease loss still cancels this bounded finishing phase.
	finishCtx, cancel := context.WithTimeout(r.config.Parent.Context(), 2*time.Minute)
	defer cancel()
	if c.Checkout != nil {
		if err := r.worktrees.Cleanup(finishCtx, *c.Checkout, tree); err != nil {
			return err
		}
	} else if c.Scratch != "" {
		// A live-tree scratch is runtime-owned only inside the runtime directory.
		if dir, err := filepath.EvalSymlinks(r.config.Directory); err == nil && sandbox.PathWithin(c.Scratch, dir) {
			if err := os.RemoveAll(c.Scratch); err != nil {
				return err
			}
		}
	}
	return r.update(finishCtx, func(s *State) error { delete(s.Contexts, c.ID); return nil })
}
