package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/alexschlessinger/pollytool/internal/envstorage"
	"github.com/alexschlessinger/pollytool/swarm"
)

func sandboxStorageCommand(c *replCommandContext, args []string) error {
	show := len(args) == 1 && args[0] == "storage"
	reset := len(args) == 2 && args[0] == "reset" && args[1] == "environment"
	clean := len(args) == 2 && args[0] == "clean" && args[1] == "caches"
	if !show && !reset && !clean {
		return c.replyLine(sandboxCommandUsage)
	}
	s, why := sandboxProfileFor(c)
	if s == nil {
		return c.replyLine(why)
	}
	state := c.state
	release := func() {}
	if !show {
		var err error
		release, err = state.toolRegistry.BeginEnvironmentMaintenance()
		if err != nil {
			return err
		}
		if state.swarm != nil && state.swarm.HasActive() {
			release()
			return errors.New("environment is busy; stop members before cleanup")
		}
	}
	run := func(ctx context.Context) ([]string, error) {
		defer release()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !show && state.swarm != nil {
			members, err := state.swarm.State(ctx)
			if err != nil {
				return nil, err
			}
			for _, member := range members.Members {
				if member.Control != swarm.MemberControlStopped {
					return nil, errors.New("environment is busy; stop members before cleanup")
				}
			}
		}
		unlock, err := envstorage.LockContext(ctx, filepath.Join(filepath.Dir(s.ws.profile), "profile.lock"))
		if err != nil {
			return nil, err
		}
		defer unlock()
		file, err := readSandboxProfile(s.ws.profile)
		if err != nil {
			return nil, err
		}
		roots := s.ws.storageRoots()
		if show {
			return sandboxStorageLines(ctx, roots, file)
		}
		if err := s.ensureLease(); err != nil {
			return nil, err
		}
		if err := s.lease.Exclusive(func() error { return roots.Clean(ctx, file.Storage, reset) }); err != nil {
			return nil, fmt.Errorf("cleanup stopped (ownership records retained; retry is safe): %w", err)
		}
		if reset {
			return []string{"environment reset; configuration, declarations and explicit grants preserved", "restore dependencies with the recorded bootstrap commands, then verify the build and tests again; checkout dependencies and build outputs were left in place"}, nil
		}
		return []string{"tracked disposable caches cleared; empty directories recreated"}, nil
	}
	if c.storageWork != nil {
		if err := c.storageWork("sandbox storage", run); err != nil {
			release()
			return err
		}
		return nil
	}
	lines, err := run(c.operationContext())
	if err != nil {
		return err
	}
	return c.replyLines(lines)
}

func sandboxStorageLines(ctx context.Context, roots envstorage.Roots, file sandboxProfile) ([]string, error) {
	lines := []string{"managed sandbox storage"}
	for _, a := range file.Storage.Allocations {
		sharing := "this checkout"
		if a.Shared {
			sharing = "shared across writable worktrees"
		}
		if a.Disabled {
			sharing += " · forgotten (no automatic grant)"
		}
		size, err := roots.Size(ctx, a)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		amount := fmt.Sprintf("%d bytes", size)
		if err != nil {
			amount = "unavailable: " + err.Error()
		}
		lines = append(lines, fmt.Sprintf("  %s · %s · %s · %s\n    %s\n    %s · recipe %s", a.Key(), a.Kind, sharing, amount, roots.Path(a), a.Purpose, a.Recipe))
	}
	if len(file.Storage.Allocations) == 0 {
		lines = append(lines, "  no tracked allocations")
	}
	for _, item := range file.Items {
		if item.Kind == profileEnv && !item.managed() && usesProfileCache(item.Value) || item.Kind == profileWrite {
			lines = append(lines, "  legacy/unmanaged (excluded from cleanup): "+item.String())
		}
	}
	return append(lines, "project dependency directories and build outputs are unmanaged and excluded from cleanup"), nil
}

// Runs outside the model lock and stays attached to the originating tab.
func (r *managedREPL) startSandboxStorageWork(label string, run func(context.Context) ([]string, error)) error {
	m, tab := r.model, r.visibleTab()
	parent := context.Background()
	if r.state != nil && r.state.session != nil {
		parent = r.state.session.Context()
	}
	if !r.background(func() {
		ctx, cancel := context.WithCancel(parent)
		stop := context.AfterFunc(r.work.ctx, cancel)
		defer cancel()
		defer stop()
		lines, err := run(ctx)
		r.postUI(r.work.ctx, func() {
			if tab != nil && !slices.Contains(r.tabs, tab) {
				return
			}
			m.mu.Lock()
			defer m.mu.Unlock()
			if err != nil {
				m.appendErrorLine(label + ": " + err.Error())
				return
			}
			for _, line := range lines {
				m.appendNoticeLine(line)
			}
		})
	}) {
		return errors.New("workspace is closing")
	}
	m.appendNoticeLine(label + " started")
	return nil
}
