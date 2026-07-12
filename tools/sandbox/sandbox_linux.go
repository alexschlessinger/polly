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
	"strconv"
	"strings"
	"syscall"
)

type linuxSandbox struct {
	cfg Config
}

// New creates a Sandbox for Linux using bubblewrap (bwrap).
func New(cfg Config) (Sandbox, error) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		return nil, fmt.Errorf("bwrap not available: %w", err)
	}
	var err error
	cfg, err = normalizeConfigPaths(cfg)
	if err != nil {
		return nil, err
	}
	if err := validateConfig(cfg); err != nil {
		return nil, err
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
// Only a confirmed not-exist drops a mask. ENOTDIR counts as not-exist: a deny
// entry like ~/.docker/config.json resolves with ENOTDIR (not ENOENT) when
// ~/.docker is a regular file, and the credential file cannot exist under a
// non-directory — keeping it would make bwrap try to mkdir a mountpoint under a
// file and abort every command. Any OTHER resolution failure (permissions, I/O)
// keeps the original path — if bwrap then can't mask it, the command errors
// instead of running with the path readable.
// Resolved paths are de-duplicated so two links to the same target don't
// double-mask.
func existingDeniedPaths(paths []DeniedPath) []DeniedPath {
	kept := make([]DeniedPath, 0, len(paths))
	seen := make(map[string]bool, len(paths))
	for _, p := range paths {
		real, err := filepath.EvalSymlinks(p.Path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
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
	for i, entry := range filtered {
		name, _, _ := strings.Cut(entry, "=")
		switch name {
		case "TMPDIR", "TMP", "TEMP":
			filtered[i] = name + "=/tmp"
		}
	}
	cmd.Env = filtered

	// The deny list is re-evaluated on every wrap, not frozen at construction:
	// a credential dir created mid-session (e.g. `aws configure` in another
	// terminal) must be masked by the next command, not left readable because
	// it didn't exist at startup.
	denied := existingDeniedPaths(allDeniedPaths(s.cfg))

	origArgs := cmd.Args
	origPath, err := resolvedExecutablePath(cmd)
	if err != nil {
		return err
	}
	slog.Debug("sandbox_wrap",
		"command", commandSummary(origArgs),
		"network", s.cfg.AllowNetwork,
		"deny_write", s.cfg.DenyWrite,
		"writable_paths", s.cfg.WritablePaths,
		"env_stripped", stripped,
		"denied_paths", len(denied))
	bwrapPath, _ := exec.LookPath("bwrap")
	cmd.Path = bwrapPath
	args := buildBwrapArgs(s.cfg, denied, origPath)
	seccompFD, err := attachUnixSocketFilter(cmd)
	if err != nil {
		return fmt.Errorf("prepare seccomp filter: %w", err)
	}
	args = append(args, "--seccomp", strconv.Itoa(seccompFD))
	cmd.Args = make([]string, 0, len(args)+1+len(origArgs))
	cmd.Args = append(cmd.Args, args...)
	cmd.Args = append(cmd.Args, "--")
	// Preserve the executable path that os/exec already resolved. In particular,
	// a strict AllowEnv may omit PATH; re-executing origArgs[0] would then fail or
	// select a different program than the caller requested.
	cmd.Args = append(cmd.Args, origPath)
	cmd.Args = append(cmd.Args, origArgs[1:]...)
	return nil
}

func buildBwrapArgs(cfg Config, deniedPaths []DeniedPath, commandPaths ...string) []string {
	args := []string{"bwrap"}

	// Read-only root filesystem. Runtime state is covered with private mounts
	// below so host Unix sockets are not visible through the broad root bind.
	args = append(args, "--ro-bind", "/", "/")
	privateTmp := "/tmp"
	if real, err := filepath.EvalSymlinks(privateTmp); err == nil {
		privateTmp = real
	}
	privateRun := "/run"
	if real, err := filepath.EvalSymlinks(privateRun); err == nil {
		privateRun = real
	}
	args = append(args, "--tmpfs", privateTmp)
	args = append(args, "--tmpfs", privateRun)
	runtimeReadOnlyPaths := []string{privateRun}
	resolvInPrivateRun := false
	if cfg.AllowNetwork {
		// systemd-resolved commonly makes /etc/resolv.conf a symlink into /run.
		// Recreate only that file inside the otherwise private /run, either from
		// the host resolver config or from /dev/null when DNS is denied.
		if real, err := filepath.EvalSymlinks("/etc/resolv.conf"); err == nil && strings.HasPrefix(real, privateRun+"/") {
			source := "/etc/resolv.conf"
			if cfg.DenyDNS {
				source = "/dev/null"
			}
			args = append(args, "--ro-bind", source, real)
			resolvInPrivateRun = true
		}
	}
	if real, err := filepath.EvalSymlinks("/var/run"); err == nil && real != privateRun {
		args = append(args, "--tmpfs", real)
		runtimeReadOnlyPaths = append(runtimeReadOnlyPaths, real)
	}

	// A selected tool may itself live under host temp/runtime state. Re-expose
	// only its containing directory read-only; the rest of the private mount
	// remains blank, and the seccomp policy still blocks any socket nodes found
	// in that explicitly selected directory.
	seenCommandBinds := make(map[string]bool)
	for _, commandPath := range commandPaths {
		for _, privateRoot := range []string{privateTmp, privateRun} {
			if !isPathWithin(commandPath, privateRoot) {
				continue
			}
			bindPath := filepath.Dir(commandPath)
			if bindPath == privateRoot {
				bindPath = commandPath
			}
			if !seenCommandBinds[bindPath] {
				args = append(args, "--ro-bind", bindPath, bindPath)
				seenCommandBinds[bindPath] = true
			}
		}
	}

	// Writable bind mounts.
	if !cfg.DenyWrite {
		for _, p := range cfg.WritablePaths {
			expanded := expandTilde(p)
			// /tmp is always writable, but is deliberately a private tmpfs. The
			// default config historically listed it as a writable path; do not
			// accidentally replace the private mount with the host's shared /tmp.
			cleaned := filepath.Clean(expanded)
			if cleaned == "/tmp" || cleaned == privateTmp || cleaned == filepath.Clean(os.TempDir()) {
				continue
			}
			// bwrap aborts the whole command if a bind source is missing, so
			// skip a writable path that doesn't exist (yet) rather than failing
			// every command. Re-evaluated per wrap, so a path created later
			// mid-session becomes writable on the next command. Writes to a
			// skipped path fail at runtime — fail-closed.
			if _, err := os.Stat(expanded); err != nil {
				slog.Warn("sandbox_skip_writable_path", "path", expanded, "reason", err)
				continue
			}
			args = append(args, "--bind", expanded, expanded)
		}
	}

	// Expand ReadPaths (resolve ~) for comparison with denied paths. Denied
	// paths arrive already symlink-resolved (existingDeniedPaths), so resolve
	// the exemptions the same way: a readPaths entry naming a symlinked
	// credential dir (e.g. WSL's ~/.aws -> /mnt/c/...) would otherwise never
	// match the resolved denied path and the exemption would silently fail,
	// leaving the path masked despite the user opting to read it.
	readSet := make(map[string]bool, len(cfg.ReadPaths))
	type readBind struct {
		source      string
		destination string
		stage       string
	}
	var readBinds []readBind
	seenReadBinds := make(map[string]bool)
	readStageRoot := filepath.Join(privateRun, ".pollytool-readpaths")
	for _, rp := range cfg.ReadPaths {
		expanded := expandTilde(rp)
		readSet[expanded] = true
		real := expanded
		if resolved, err := filepath.EvalSymlinks(expanded); err == nil {
			real = resolved
			readSet[real] = true
		}

		// A child exemption needs to be restored after its denied parent is
		// covered with tmpfs. Stage it first under the private /run mount so
		// the host source remains reachable after the parent mask is applied.
		for _, destination := range []string{expanded, real} {
			if seenReadBinds[destination] {
				continue
			}
			for _, denied := range deniedPaths {
				if denied.Kind != DeniedPathDir || destination == denied.Path || !isPathWithin(destination, denied.Path) {
					continue
				}
				if _, err := os.Stat(real); err != nil {
					break
				}
				stage := filepath.Join(readStageRoot, strconv.Itoa(len(readBinds)))
				readBinds = append(readBinds, readBind{source: real, destination: destination, stage: stage})
				seenReadBinds[destination] = true
				break
			}
		}
	}
	if len(readBinds) > 0 {
		args = append(args, "--dir", readStageRoot)
	}
	for _, bind := range readBinds {
		args = append(args, "--ro-bind", bind.source, bind.stage)
	}

	// Overlay sensitive credential paths, skipping those exempted by ReadPaths.
	// Denied files are masked with /dev/null (reads return empty): unlike a
	// shared placeholder file in /tmp, it can't be written through the
	// writable /tmp bind to plant content at the masked paths.
	var deniedReadOnlyPaths []string
	for _, denied := range deniedPaths {
		if readSet[denied.Path] || isUnderAny(denied.Path, readSet) {
			continue
		}
		switch denied.Kind {
		case DeniedPathFile:
			args = append(args, "--ro-bind", "/dev/null", denied.Path)
		default:
			args = append(args, "--tmpfs", denied.Path)
			if cfg.DenyWrite {
				deniedReadOnlyPaths = append(deniedReadOnlyPaths, denied.Path)
			}
		}
	}
	for _, bind := range readBinds {
		args = append(args, "--ro-bind", bind.stage, bind.destination)
	}
	for _, p := range deniedReadOnlyPaths {
		args = append(args, "--remount-ro", p)
	}
	for _, p := range runtimeReadOnlyPaths {
		args = append(args, "--remount-ro", p)
	}
	if cfg.DenyWrite {
		args = append(args, "--remount-ro", privateTmp)
	}

	// Basic device and proc mounts.
	args = append(args, "--dev", "/dev")
	args = append(args, "--proc", "/proc")

	// Without a PID namespace the sandbox shares the host's, and /proc lets it
	// read /proc/<pid>/environ of any same-UID process — including polly's own
	// POLLYTOOL_* API keys, defeating the env filtering above.
	args = append(args, "--unshare-pid")
	args = append(args, "--unshare-ipc")

	if !cfg.AllowNetwork {
		args = append(args, "--unshare-net")
	} else if cfg.DenyDNS {
		if !resolvInPrivateRun {
			args = append(args, "--ro-bind", "/dev/null", "/etc/resolv.conf")
		}
	}

	args = append(args, "--die-with-parent")

	// Detach from the controlling terminal: a sandboxed process holding the
	// tty can otherwise inject keystrokes into the parent shell via the
	// TIOCSTI ioctl (see bwrap(1) on --new-session).
	args = append(args, "--new-session")

	return args
}

func isPathWithin(path, parent string) bool {
	rel, err := filepath.Rel(parent, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
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
