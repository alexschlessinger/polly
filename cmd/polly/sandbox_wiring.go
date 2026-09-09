package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

var newSandbox = sandbox.New

func sandboxRegistryOptionsWithWarnings(config *Config, warnings *broadWritablePathWarner, privatePaths ...string) ([]tools.RegistryOption, *sandboxProbe, error) {
	if config.NoSandbox {
		return []tools.RegistryOption{tools.WithUnsafeNoSandbox()}, nil, nil
	}
	if warnings == nil {
		warnings = newBroadWritablePathWarner()
	}

	baseCfg, err := sandbox.ParsePreset(config.SandboxPreset)
	if err != nil {
		return nil, nil, err
	}
	baseCfg = baseCfg.Merge(sandbox.Config{
		WritablePaths: config.WritePaths,
		DenyPaths:     append(append([]string(nil), config.DenyPaths...), privatePaths...),
		AllowNetwork:  config.AllowNet,
	})
	baseCfg, err = sandbox.PrepareConfig(baseCfg)
	if err != nil {
		return nil, nil, fmt.Errorf("prepare sandbox config: %w", err)
	}

	// The same warning-aware factory handles the startup probe and every final
	// per-tool config produced later by the registry. One shared state suppresses
	// repeats when the base grant appears in several effective configs. --quiet
	// silences the warnings at their source, like the sandbox notice.
	warningFactory := func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		sb, err := newSandbox(cfg)
		if err == nil && sb != nil && !config.Quiet {
			warnings.Warn(cfg)
		}
		return sb, err
	}

	// Validate that the backend constructs (e.g. the binary exists)...
	sb, err := warningFactory(baseCfg)
	if err != nil {
		return nil, nil, fmt.Errorf("sandbox requested but unavailable: %w", err)
	}
	// ...and that it can actually start a command. Construction alone misses
	// environments where the backend is present but fails at runtime; without
	// this probe every bash call would silently return a refusal while the run
	// still exits 0/ok. The spawn costs tens of milliseconds, so it runs off
	// the open; the first turn waits on it before any tool can run, and the
	// open itself consults it only when a tool that spawns while loading
	// fails (see conversationOpener.open).
	return []tools.RegistryOption{tools.WithSandboxFactory(warningFactory, baseCfg)}, startSandboxProbe(sb), nil
}

// sandboxProbe is one asynchronous sandbox.Probe. wait blocks until the
// spawned command has reported, returning the startup failure with its
// escape hatch, or the caller's cancellation.
type sandboxProbe struct {
	done chan struct{}
	err  error
}

func startSandboxProbe(sb sandbox.Sandbox) *sandboxProbe {
	p := &sandboxProbe{done: make(chan struct{})}
	go func() {
		defer close(p.done)
		if err := sandbox.Probe(sb); err != nil {
			p.err = fmt.Errorf("sandbox requested but failed to start: %w\n"+
				"Set POLLYTOOL_NOSANDBOX=1 (or pass --nosandbox) to run without the sandbox", err)
		}
	}()
	return p
}

// wait is safe on a nil probe, which is what --nosandbox produces.
func (p *sandboxProbe) wait(ctx context.Context) error {
	if p == nil {
		return nil
	}
	select {
	case <-p.done:
		return p.err
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

type broadWritablePathWarner struct {
	mu      sync.Mutex
	seen    map[string]bool
	pending []string
	notify  chan struct{}
	home    string
}

func newBroadWritablePathWarner() *broadWritablePathWarner {
	home, _ := os.UserHomeDir()
	home = canonicalWarningPath(home)
	return &broadWritablePathWarner{
		seen:   make(map[string]bool),
		notify: make(chan struct{}, 1),
		home:   home,
	}
}

func (w *broadWritablePathWarner) Notify() <-chan struct{} {
	if w == nil {
		return nil
	}
	return w.notify
}

// Drain atomically takes all pending warning bodies. Consuming the coalesced
// notification under the same lock as the queue prevents a concurrent enqueue
// from losing its wakeup.
func (w *broadWritablePathWarner) Drain() []string {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	select {
	case <-w.notify:
	default:
	}
	pending := append([]string(nil), w.pending...)
	w.pending = nil
	w.mu.Unlock()
	return pending
}

// Warn reports explicit writable grants for the whole home directory or a
// filesystem root. The workspace preset rejects those roots before discovery,
// but --writepath and per-tool overlays can still add them. The credential deny
// list still applies; this is a user-visible heads-up, not a refusal.
func (w *broadWritablePathWarner) Warn(cfg sandbox.Config) {
	if w == nil || cfg.DenyWrite {
		return
	}
	for _, path := range cfg.WritablePaths {
		path = filepath.Clean(path)
		if broadWritablePathDenied(path, cfg.DenyWritePaths) {
			continue
		}
		scope := ""
		switch {
		case path != "" && filepath.IsAbs(path) && filepath.Dir(path) == path:
			scope = "a filesystem root"
		case w.home != "" && path == w.home:
			scope = "the whole home directory"
		default:
			continue
		}

		body := fmt.Sprintf("sandbox writable path %q grants write access to %s; remove or narrow the originating --writepath/POLLYTOOL_WRITEPATHS or tool writablePaths setting unless this broad access is intentional", path, scope)
		w.emit(path, body)
	}
}

func (w *broadWritablePathWarner) emit(path, body string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.seen[path] {
		return
	}
	w.seen[path] = true
	w.pending = append(w.pending, body)
	select {
	case w.notify <- struct{}{}:
	default:
	}
}

func canonicalWarningPath(path string) string {
	if path == "" {
		return ""
	}
	path = filepath.Clean(path)
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(real)
	}
	return path
}

func broadWritablePathDenied(path string, denyWritePaths []string) bool {
	if path == "" {
		return false
	}
	for _, denied := range denyWritePaths {
		if denied != "" && sandbox.PathWithin(path, filepath.Clean(denied)) {
			return true
		}
	}
	return false
}
