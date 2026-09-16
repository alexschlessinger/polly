//go:build unix

package sandbox

import (
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"sync/atomic"
	"syscall"
)

// helperMode gates construction of the container sandbox: only polly's
// in-container helper may build a sandbox that applies no filesystem or
// network rules, because inside a container those rules are the container's.
// EnterHelperMode is called from exactly one place, the hidden helper
// subcommand; tests call it from TestMain. Audit: grep -rn EnterHelperMode.
var helperMode atomic.Bool

// EnterHelperMode marks this process as polly's in-container helper. It is
// idempotent and cannot be undone.
func EnterHelperMode() { helperMode.Store(true) }

// ErrNotHelper reports a container sandbox requested outside helper mode.
var ErrNotHelper = errors.New("container sandbox is only constructible in helper mode")

// containerSandbox runs commands directly inside the container. It keeps the
// environment discipline of the OS backends (sealed host values, ambient
// filtering, explicit per-tool values, scratch variables) and starts every
// command in its own session, but applies no filesystem or network rules:
// the container's mounts, read-only root, dropped capabilities and network
// mode are the boundary. This is the one sanctioned path that runs a child
// outside bubblewrap or Seatbelt; SANDBOX.md documents it.
type containerSandbox struct {
	cfg    Config
	sealed map[string]string
}

// NewContainerSandboxFactory returns the sandbox factory the helper's
// registry uses. sealed is the host-selected environment as NAME=VALUE
// entries; its values override the image's before ambient filtering and
// never reach a wrapper argv.
func NewContainerSandboxFactory(sealed []string) (func(Config) (Sandbox, error), error) {
	if !helperMode.Load() {
		return nil, ErrNotHelper
	}
	values := make(map[string]string, len(sealed))
	for _, entry := range sealed {
		name, err := validateEnvEntry(entry)
		if err != nil {
			return nil, err
		}
		values[name] = entry[len(name)+1:]
	}
	return func(cfg Config) (Sandbox, error) {
		prepared, err := PrepareConfig(cfg)
		if err != nil {
			return nil, err
		}
		if err := validateConfig(prepared); err != nil {
			return nil, err
		}
		return &containerSandbox{cfg: prepared, sealed: values}, nil
	}, nil
}

func (s *containerSandbox) Wrap(*exec.Cmd) error { return ErrManagedWrapRequired }

func (s *containerSandbox) WrapWithEnv(_ *exec.Cmd, explicitEnv map[string]string) error {
	if err := validateExplicitEnv(explicitEnv); err != nil {
		return err
	}
	return ErrManagedWrapRequired
}

func (s *containerSandbox) wrapManaged(cmd *exec.Cmd, explicitEnv map[string]string) error {
	if err := validateExplicitEnv(explicitEnv); err != nil {
		return err
	}
	env := cmd.Env
	if env == nil {
		env = os.Environ()
	}
	// Host-selected values win over the image's, then ambient filtering
	// applies to the merged set, then the tool's explicit values, then the
	// policy's own (the scratch variables).
	env = mergeExplicitEnv(env, s.sealed)
	filtered, stripped := filterEnv(env, s.cfg.AllowEnv, s.cfg.PassEnv)
	if len(explicitEnv) > 0 {
		filtered = mergeExplicitEnv(filtered, explicitEnv)
	}
	if len(s.cfg.Env) > 0 {
		filtered = mergeExplicitEnv(filtered, s.cfg.Env)
	}
	cmd.Env = filtered
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// A private session makes the command its own process-group leader, so
	// finite cancellation kills the whole group.
	cmd.SysProcAttr.Setsid = true
	slog.Debug("container_sandbox_wrap", "command", commandSummary(cmd.Args), "env_stripped", stripped)
	return nil
}
