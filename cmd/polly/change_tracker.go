package main

import (
	"log/slog"
	"os"
	"path/filepath"

	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/worktree"
)

// installChangeTracker gives the registry's bash tool a way to report what
// its commands changed: Git snapshots kept under ~/.pollytool/changes. A
// tracker that cannot be built only costs the diffs, so failures are logged
// and the conversation opens without one.
func installChangeTracker(registry *tools.ToolRegistry, privatePaths []string) *worktree.ChangeTracker {
	home, err := os.UserHomeDir()
	if err != nil {
		slog.Debug("change tracking disabled", "error", err)
		return nil
	}
	tracker, err := worktree.NewChangeTracker(registry, filepath.Join(home, ".pollytool", "changes"), privatePaths, worktree.ChangeLimits{})
	if err != nil {
		slog.Debug("change tracking disabled", "error", err)
		return nil
	}
	registry.SetChangeTracker(tracker)
	return tracker
}
