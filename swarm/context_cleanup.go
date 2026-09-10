package swarm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/alexschlessinger/pollytool/tools/sandbox"
	"github.com/alexschlessinger/pollytool/workflow"
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
	// A copy still at its base needs no snapshot; the cheap check declines
	// whenever an edit could hide from it, and the capture then decides.
	if unchanged, err := m.Unchanged(ctx, *c.Checkout); err != nil {
		return "", err
	} else if unchanged {
		return c.Checkout.Base.Tree, nil
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
	if err := r.markRetiring(ctx, []*ExecutionContext{c}); err != nil {
		return err
	}
	_, err := r.finishRetirement(ctx, []*ExecutionContext{c}, map[string]string{c.ID: tree})
	return err
}

// markRetiring records provenance and retirement for every context in one
// transaction, before any file changes. Callers hold launchMu and parentTools
// and exclude active context operations.
func (r *Runtime) markRetiring(ctx context.Context, contexts []*ExecutionContext) error {
	return r.update(ctx, func(s *State) error {
		for _, c := range contexts {
			stored := s.Contexts[c.ID]
			if stored == nil {
				return errors.New("unknown execution context")
			}
			for _, task := range s.Tasks {
				if task.Owner == c.Owner && task.StartingSnapshot == "" && c.Checkout != nil {
					task.StartingSnapshot = c.Checkout.Base.ID
				}
			}
			stored.Release = WorkspaceReleasing
			if member := s.Members[c.Owner]; member != nil {
				member.Control = MemberControlRetired
			}
		}
		return nil
	})
}

// finishRetirement closes the workflow tool bindings of contexts whose
// retirement is recorded, removes their files, and deletes their records in
// one transaction. A context whose removal fails stays Retiring for a later
// cleanup while the others still finish. Callers hold the context locks and
// must have constructed the worktree manager when any context has a checkout.
func (r *Runtime) finishRetirement(ctx context.Context, contexts []*ExecutionContext, trees map[string]string) (removed []string, err error) {
	// A canceled caller cannot strand cleanup halfway through retirement.
	// Parent lease loss still cancels this bounded finishing phase.
	finishCtx, cancel := context.WithTimeout(r.config.Parent.Context(), 2*time.Minute+10*time.Second*time.Duration(len(contexts)))
	defer cancel()
	var errs []error
	for _, c := range contexts {
		// Bound tools and MCP servers hold grants on the directory; they go
		// before the directory can be reused by the next checkout.
		r.unbindContext(c.ID)
		if e := r.removeContextFiles(finishCtx, c, trees[c.ID]); e != nil {
			errs = append(errs, fmt.Errorf("context %s: %w", c.ID, e))
			continue
		}
		removed = append(removed, c.ID)
	}
	if len(removed) > 0 {
		if e := r.update(finishCtx, func(s *State) error {
			for _, id := range removed {
				delete(s.Contexts, id)
			}
			return nil
		}); e != nil {
			return nil, errors.Join(append(errs, e)...)
		}
	}
	return removed, errors.Join(errs...)
}

func (r *Runtime) removeContextFiles(ctx context.Context, c *ExecutionContext, tree string) error {
	if c.Checkout != nil {
		return r.worktrees.Cleanup(ctx, *c.Checkout, tree)
	}
	if c.Scratch == "" {
		return nil
	}
	// A live-tree scratch is runtime-owned only inside the runtime directory.
	if dir, err := filepath.EvalSymlinks(r.config.Directory); err == nil && sandbox.PathWithin(c.Scratch, dir) {
		return os.RemoveAll(c.Scratch)
	}
	return nil
}
