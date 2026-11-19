package main

import (
	"log/slog"
	"os"
	"path/filepath"

	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/worktree"
)

// installChangeTracker builds the session's workspace observer: Git snapshots
// kept under ~/.pollytool/changes. The tracker is deliberately not attached to
// the registry, so no tool takes a before/after snapshot of its own; the
// conversation reports the net workspace state once per turn instead. A
// tracker that cannot be built only costs the report, so failures are logged
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
	return tracker
}
