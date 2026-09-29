package swarm

import (
	"context"
	"errors"
	"fmt"

	"github.com/alexschlessinger/pollytool/worktree"
)

// ErrWorktreesUnavailable means the host did not configure Git workspace
// construction. It is distinct from worktree.ErrNotRepository: a missing
// constructor cannot silently turn an isolated checkout into a live workspace.
var ErrWorktreesUnavailable = errors.New("swarm requires OpenWorktrees for Git workspaces")

func needsWorktrees(s *State) bool {
	if len(s.Snapshots) != 0 || len(s.Applies) != 0 {
		return true
	}
	for _, c := range s.Contexts {
		if c.Checkout != nil {
			return true
		}
	}
	return false
}

func (r *Runtime) manager(ctx context.Context) (*worktree.Manager, error) {
	r.worktreeMu.Lock()
	defer r.worktreeMu.Unlock()
	if r.worktrees != nil {
		return r.worktrees, nil
	}
	if r.config.OpenWorktrees == nil {
		return nil, ErrWorktreesUnavailable
	}
	c := worktree.Config{
		Root: r.config.Root, Directory: r.config.Directory,
		MaxWorktrees: r.config.MaxWorktrees,
		PrivatePaths: append([]string(nil), r.config.PrivatePaths...),
	}
	m, err := r.config.OpenWorktrees(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("open swarm worktrees: %w", err)
	}
	if m == nil {
		return nil, errors.New("OpenWorktrees returned a nil manager")
	}
	r.worktrees = m
	return m, nil
}
