//go:build linux

package sandbox

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type linuxSandbox struct {
	cfg Config
}

// New creates a Sandbox for Linux using bubblewrap (bwrap).
func New(cfg Config) (Sandbox, error) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		return nil, fmt.Errorf("bwrap not available: %w", err)
	}
	// Without a home directory the credential deny list expands to nothing;
	// refuse to construct a sandbox that silently masks zero paths.
	if _, err := os.UserHomeDir(); err != nil {
		return nil, fmt.Errorf("cannot resolve home directory for credential masking: %w", err)
	}
	// bwrap aborts on a bind whose source or mountpoint is missing, which
	// would fail every command run through this sandbox. Catch it here so the
	// tool fails at construction with the offending path named.
	if !cfg.DenyWrite {
		for _, p := range cfg.WritablePaths {
			if _, err := os.Stat(expandTilde(p)); err != nil {
				return nil, fmt.Errorf("writable path %q: %w", p, err)
			}
		}
	}
	return &linuxSandbox{cfg: cfg}, nil
}

// existingDeniedPaths resolves each deny-path to its real location and drops
// the ones that don't exist. Two failure modes motivate this:
//
//   - Missing paths: masking one forces bwrap to mkdir a mountpoint under the
//     read-only root bind, which fails ("Can't mkdir <path>: Read-only file
//     system") and aborts the whole sandbox — so one absent credential dir
//     (e.g. ~/.gnupg) would break every command.
//   - Symlinks (e.g. WSL's ~/.aws -> /mnt/c/Users/.../.aws): bwrap can't mount a
//     tmpfs on the link itself ("Can't mount tmpfs ...: No such file or
//     directory"), and masking the link wouldn't cover the real target anyway.
//
// Only a confirmed not-exist drops a mask. Any other resolution failure
// (permissions, I/O) keeps the original path — if bwrap then can't mask it,
// the command errors instead of running with the path readable.
// Resolved paths are de-duplicated so two links to the same target don't
// double-mask.
func existingDeniedPaths(paths []DeniedPath) []DeniedPath {
	kept := make([]DeniedPath, 0, len(paths))
	seen := make(map[string]bool, len(paths))
	for _, p := range paths {
		real, err := filepath.EvalSymlinks(p.Path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			real = p.Path
		}
		if seen[real] {
			continue
		}
		seen[real] = true
		kept = append(kept, DeniedPath{Path: real, Kind: p.Kind})
	}
	return kept
}

func (s *linuxSandbox) Wrap(cmd *exec.Cmd) error {
	env := cmd.Env
	if env == nil {
		env = os.Environ()
	}
	filtered, stripped := filterEnv(env, s.cfg.AllowEnv)
	cmd.Env = filtered

	// The deny list is re-evaluated on every wrap, not frozen at construction:
	// a credential dir created mid-session (e.g. `aws configure` in another
	// terminal) must be masked by the next command, not left readable because
	// it didn't exist at startup.
	denied := existingDeniedPaths(allDeniedPaths(s.cfg))

	origArgs := cmd.Args
	slog.Debug("sandbox_wrap",
		"command", commandSummary(origArgs),
		"network", s.cfg.AllowNetwork,
		"deny_write", s.cfg.DenyWrite,
		"writable_paths", s.cfg.WritablePaths,
		"env_stripped", stripped,
		"denied_paths", len(denied))
	bwrapPath, _ := exec.LookPath("bwrap")
	cmd.Path = bwrapPath
	args := buildBwrapArgs(s.cfg, denied)
	cmd.Args = make([]string, 0, len(args)+1+len(origArgs))
	cmd.Args = append(cmd.Args, args...)
	cmd.Args = append(cmd.Args, "--")
	cmd.Args = append(cmd.Args, origArgs...)
	return nil
}

func buildBwrapArgs(cfg Config, deniedPaths []DeniedPath) []string {
	args := []string{"bwrap"}

	// Read-only root filesystem.
	args = append(args, "--ro-bind", "/", "/")

	// Writable bind mounts.
	if !cfg.DenyWrite {
		args = append(args, "--bind", "/tmp", "/tmp")
		for _, p := range cfg.WritablePaths {
			expanded := expandTilde(p)
			args = append(args, "--bind", expanded, expanded)
		}
	}

	// Expand ReadPaths (resolve ~) for comparison with denied paths.
	readSet := make(map[string]bool, len(cfg.ReadPaths))
	for _, rp := range cfg.ReadPaths {
		readSet[expandTilde(rp)] = true
	}

	// Overlay sensitive credential paths, skipping those exempted by ReadPaths.
	// Denied files are masked with /dev/null (reads return empty): unlike a
	// shared placeholder file in /tmp, it can't be written through the
	// writable /tmp bind to plant content at the masked paths.
	for _, denied := range deniedPaths {
		if readSet[denied.Path] || isUnderAny(denied.Path, readSet) {
			continue
		}
		switch denied.Kind {
		case DeniedPathFile:
			args = append(args, "--ro-bind", "/dev/null", denied.Path)
		default:
			args = append(args, "--tmpfs", denied.Path)
		}
	}

	// Basic device and proc mounts.
	args = append(args, "--dev", "/dev")
	args = append(args, "--proc", "/proc")

	// Without a PID namespace the sandbox shares the host's, and /proc lets it
	// read /proc/<pid>/environ of any same-UID process — including polly's own
	// POLLYTOOL_* API keys, defeating the env filtering above.
	args = append(args, "--unshare-pid")

	if !cfg.AllowNetwork {
		args = append(args, "--unshare-net")
	} else if cfg.DenyDNS {
		args = append(args, "--ro-bind", "/dev/null", "/etc/resolv.conf")
	}

	args = append(args, "--die-with-parent")

	// Detach from the controlling terminal: a sandboxed process holding the
	// tty can otherwise inject keystrokes into the parent shell via the
	// TIOCSTI ioctl (see bwrap(1) on --new-session).
	args = append(args, "--new-session")

	return args
}

// isUnderAny reports whether path is a child of any key in the set.
func isUnderAny(path string, set map[string]bool) bool {
	for parent := range set {
		if strings.HasPrefix(path, parent+"/") {
			return true
		}
	}
	return false
}
