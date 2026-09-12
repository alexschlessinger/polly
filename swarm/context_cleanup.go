package swarm

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/alexschlessinger/pollytool/internal/scratch"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
	"github.com/alexschlessinger/pollytool/workflow"
)

// contextCleanupTree proves the copy is unchanged or exactly matches an
// integrated task. Callers enforce their own activity and ownership rules and
// hold the context lock while checking and releasing it.
func (r *Runtime) contextCleanupTree(ctx context.Context, s *State, c *ExecutionContext) (string, error) {
	if c.Checkout == nil {
		return "", nil
	}
	m, err := r.manager(ctx)
	if err != nil {
		return "", err
	}
	if c.Release == WorkspaceReleasing {
		present, err := m.CheckoutPresent(*c.Checkout)
		if err != nil {
			return "", err
		}
		if !present {
			return "", nil
		}
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
		if !c.ReadOnly && task.Status == "done" && snapshot != nil && snapshot.Source == c.Root && snapshot.Tree == current.Tree {
			return current.Tree, nil
		}
	}
	return "", &workflow.Error{Code: "unintegrated_changes", Message: "context contains unintegrated tree " + current.Tree + "; retained at " + c.Root, Result: map[string]any{"context": c.ID, "root": c.Root}}
}

// markReleasing records provenance and release for every context in one
// transaction, before any file changes. Callers hold launchMu and parentTools
// and exclude active context operations.
func (r *Runtime) markReleasing(ctx context.Context, contexts []*ExecutionContext) error {
	return r.update(ctx, func(s *State) error {
		for _, c := range contexts {
			stored := s.Contexts[c.ID]
			if stored == nil {
				return errors.New("unknown execution context")
			}
			stored.Release = WorkspaceReleasing
		}
		return nil
	})
}

// finishRelease closes the workflow tool bindings of contexts whose
// release is recorded, removes their files, and deletes their records in
// one transaction. A context whose removal fails stays Releasing for a later
// cleanup while the others still finish. Callers hold the context locks and
// must have constructed the worktree manager when any context has a checkout.
func (r *Runtime) finishRelease(ctx context.Context, contexts []*ExecutionContext, trees map[string]string) (removed []string, err error) {
	defer func() {
		for _, c := range contexts {
			r.notifyRelease(c.ID)
		}
	}()
	// A canceled caller cannot strand cleanup halfway through release.
	// Parent lease loss still cancels this bounded finishing phase.
	finishCtx, cancel := context.WithTimeout(r.config.Parent.Context(), 2*time.Minute+10*time.Second*time.Duration(len(contexts)))
	defer cancel()
	var errs []error
	var wake []string
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
				for _, member := range s.Members {
					if member.Context == id {
						member.Context = ""
						wake = append(wake, member.ID)
					}
				}
				delete(s.Contexts, id)
			}
			return nil
		}); e != nil {
			return nil, errors.Join(append(errs, e)...)
		}
	}
	for _, id := range wake {
		r.wake(id)
	}
	return removed, errors.Join(errs...)
}

func (r *Runtime) removeContextFiles(ctx context.Context, c *ExecutionContext, tree string) error {
	if c.Checkout != nil {
		return r.worktrees.FinishCleanup(ctx, *c.Checkout, tree)
	}
	if c.Scratch == "" {
		return nil
	}
	// A live-tree scratch is runtime-owned only inside the runtime directory.
	if dir, err := filepath.EvalSymlinks(r.config.Directory); err == nil && sandbox.PathWithin(c.Scratch, dir) {
		return scratch.RemoveAll(c.Scratch)
	}
	return nil
}
