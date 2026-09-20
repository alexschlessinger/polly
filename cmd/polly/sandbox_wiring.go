package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/alexschlessinger/pollytool/internal/envstorage"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

var newSandbox = sandbox.New

// resolveConfigAddDirs strictly validates the --add-dir entries against the
// working directory, returning the canonical real paths in flag order with
// duplicates dropped (a repeated or subsumed flag grants nothing new). The
// filesystem checks belong here at sandbox startup rather than at flag
// parsing (see validateSandboxPresetSpec): management commands and
// --nosandbox never construct a sandbox, and the open is what fails, naming
// the offending path and the rejection reason.
func resolveConfigAddDirs(config *Config) ([]string, error) {
	if len(config.AddDirs) == 0 {
		return nil, nil
	}
	workspace, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("resolve working directory: %w", err)
	}
	dirs := make([]string, 0, len(config.AddDirs))
	for _, path := range config.AddDirs {
		canonical, err := sandbox.ValidateExtraReadDir(workspace, path)
		if err != nil {
			return nil, fmt.Errorf("--add-dir: %w", err)
		}
		dirs = append(dirs, canonical)
	}
	return sandbox.MergeExtraReadDirs(nil, dirs), nil
}

// sandboxRegistryOptionsWithWarnings builds the base sandbox policy: the
// preset, the CLI grants and denies, the session's private paths, the read
// grants that keep skills and attachments visible inside the private home,
// the extra read-only directories from --add-dir, the working directory
// when nothing else exposes it, and a linked worktree's Git metadata. The
// workspace's sandbox profile, whatever of it applies, is layered over the
// base; the returned state is nil under --nosandbox.
func sandboxRegistryOptionsWithWarnings(config *Config, warnings *broadWritablePathWarner, skillRoots, extraReadDirs []string, privatePaths ...string) ([]tools.RegistryOption, *sandboxProbe, *sandboxProfileState, error) {
	if config.NoSandbox {
		return []tools.RegistryOption{tools.WithUnsafeNoSandbox()}, nil, nil, nil
	}
	// Establish the runtime mount point before any Linux sandbox is created.
	// Otherwise a command started before the first profile save could see
	// ~/.pollytool appear later through its readable home mount.
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("resolve runtime storage: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(home, userConfigDirName), 0700); err != nil {
		return nil, nil, nil, fmt.Errorf("prepare private runtime root: %w", err)
	}
	if err := envstorage.EnsurePrivateRoots(); err != nil {
		return nil, nil, nil, fmt.Errorf("prepare private storage roots: %w", err)
	}
	if warnings == nil {
		warnings = newBroadWritablePathWarner()
	}

	baseCfg, err := sandbox.ParsePreset(config.SandboxPreset)
	if err != nil {
		return nil, nil, nil, err
	}
	baseCfg = baseCfg.Merge(sandbox.Config{
		WritablePaths: config.WritePaths,
		// Extra read dirs are appended after the home grants: they are
		// ordinary read-only paths, not home-interior candidates, and the
		// validator has already rejected the roots homeReadGrants filters
		// for (home itself and filesystem roots).
		ReadPaths:    append(homeReadGrants(config, skillRoots), extraReadDirs...),
		DenyPaths:    append(append([]string(nil), config.DenyPaths...), privatePaths...),
		AllowNetwork: config.AllowNet,
	})
	baseCfg, err = sandbox.PrepareConfig(baseCfg)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("prepare sandbox config: %w", err)
	}
	baseCfg, err = exposeWorkingDirectory(baseCfg, warnings, config.Quiet)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("expose working directory: %w", err)
	}
	baseCfg = exposeCheckoutGit(baseCfg, warnings, config.Quiet)
	if err := refuseConfigWriteGrant(baseCfg); err != nil {
		return nil, nil, nil, err
	}

	// The same warning-aware factory handles the startup probe and every final
	// per-tool config produced later by the registry. One shared state suppresses
	// repeats when the base grant appears in several effective configs. --quiet
	// silences the warnings at their source, like the sandbox notice.
	warningFactory := func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		if err := refuseConfigWriteGrant(cfg); err != nil {
			return nil, err
		}
		sb, err := newSandbox(cfg)
		if err == nil && sb != nil && !config.Quiet {
			warnings.Warn(cfg)
		}
		return sb, err
	}

	// Validate that the backend constructs (e.g. the binary exists)...
	sb, err := warningFactory(baseCfg)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("sandbox requested but unavailable: %w", err)
	}
	// ...and that it can actually start a command. Construction alone misses
	// environments where the backend is present but fails at runtime; without
	// this probe every bash call would silently return a refusal while the run
	// still exits 0/ok. The spawn costs tens of milliseconds, so it runs off
	// the open; the first turn waits on it before any tool can run, and the
	// open itself consults it only when a tool that spawns while loading
	// fails (see conversationOpener.open).
	probe := startSandboxProbe(sb)

	opts := []tools.RegistryOption{tools.WithSandboxFactory(warningFactory, baseCfg)}
	profile := openSandboxProfile(config)
	if layer, ok := profile.apply(baseCfg, warningFactory); ok {
		opts = append(opts, tools.WithSandboxLayer(sandboxProfileLayer, layer))
	}
	if !config.Quiet {
		for _, notice := range profile.notices() {
			warnings.Note(notice)
		}
	}
	return opts, probe, profile, nil
}

// refuseConfigWriteGrant fails a policy whose writable paths cover polly's
// configuration file. A POLLYTOOL_NOSANDBOX or POLLYTOOL_WRITEPATHS line
// planted there takes effect at the next start, so such a grant would let a
// sandboxed command turn the sandbox off for later sessions without anyone
// choosing --nosandbox, which stays the open way to do that. Only explicit
// writable paths count: the implicit host temp grant covers a home directory
// only where polly refuses to start at all.
func refuseConfigWriteGrant(cfg sandbox.Config) error {
	path, err := userConfigPath()
	if err != nil {
		return nil
	}
	cfg.DenyHostTemp = true
	covered := sandbox.WriteAllowed(cfg, path) == nil
	// Reject explicit broad grants even when the private runtime root would
	// mask them; the caller must not believe it granted configuration writes.
	if !cfg.DenyWrite {
		for _, grant := range cfg.WritablePaths {
			if grant == filepath.Dir(path) || grant == path {
				covered = true
			}
		}
	}
	if !covered {
		return nil
	}
	return fmt.Errorf("sandbox writable paths cover polly's configuration %s, which a sandboxed command could use to turn the sandbox off for later sessions; remove the originating --writepath/POLLYTOOL_WRITEPATHS or tool writablePaths entry, or run with --nosandbox to disable the sandbox openly", userConfigDisplayPath)
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

// Warn reports explicit writable or read grants for the whole home directory
// or a filesystem root. The workspace preset rejects those roots before
// discovery, but --writepath, --readpath and per-tool overlays can still add
// them. The credential deny list still applies; this is a user-visible
// heads-up, not a refusal.
func (w *broadWritablePathWarner) Warn(cfg sandbox.Config) {
	if w == nil {
		return
	}
	if !cfg.DenyWrite {
		for _, path := range cfg.WritablePaths {
			path = filepath.Clean(path)
			if broadWritablePathDenied(path, cfg.DenyWritePaths) {
				continue
			}
			scope := w.broadScope(path)
			if scope == "" {
				continue
			}
			body := fmt.Sprintf("sandbox writable path %q grants write access to %s; remove or narrow the originating --writepath/POLLYTOOL_WRITEPATHS or tool writablePaths setting unless this broad access is intentional", path, scope)
			w.emit(path, body)
		}
	}
	for _, path := range cfg.ReadPaths {
		path = filepath.Clean(path)
		scope := w.broadScope(path)
		if scope == "" {
			continue
		}
		body := fmt.Sprintf("sandbox read path %q exposes %s; remove or narrow the originating --readpath/POLLYTOOL_READPATHS or tool readPaths setting unless this broad access is intentional", path, scope)
		w.emit("read:"+path, body)
	}
}

// Note queues one more sandbox warning, such as a profile item that no
// longer applies, shown once like the rest.
func (w *broadWritablePathWarner) Note(body string) {
	if w == nil {
		return
	}
	w.emit("note:"+body, body)
}

func (w *broadWritablePathWarner) broadScope(path string) string {
	switch {
	case path != "" && filepath.IsAbs(path) && filepath.Dir(path) == path:
		return "a filesystem root"
	case w.home != "" && path == w.home:
		return "the whole home directory"
	}
	return ""
}

func (w *broadWritablePathWarner) emit(path, body string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.seen[path] {
		return
	}
	w.seen[path] = true
	w.pending = append(w.pending, body)
	nudge(w.notify)
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
