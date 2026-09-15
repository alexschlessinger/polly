package docker

import (
	"context"
	"io"
)

// GitAccess is what copy mode needs from the host's Git: a base commit for a
// checkout, a bundle of it, the checkout's divergence from a tree, and the
// admission of a copy's changes back into the checkout. *worktree.Manager
// implements it and keeps every policy decision (private paths, size caps,
// content filters, path admission); this package only moves bytes.
type GitAccess interface {
	// Base is the commit a copy starts from and the tree divergence is
	// measured against. It is parentless: the copy never receives history.
	Base(ctx context.Context, root string) (commit, tree string, err error)
	// WriteBundle streams a bundle holding commit.
	WriteBundle(ctx context.Context, root, commit string, w io.Writer) error
	// Divergence lists the paths root's working tree changed or added
	// relative to tree, and the tracked paths it deleted.
	Divergence(ctx context.Context, root, tree string) (changed, deleted []string, err error)
	// ImportChanges applies a copy's changed files (a tar of paths relative
	// to root) and deletions to root.
	ImportChanges(ctx context.Context, root string, changes io.Reader, deleted []string) error
}
