//go:build darwin

package sandbox

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

type darwinSandbox struct {
	cfg Config
}

// New creates a Sandbox for macOS using sandbox-exec with Seatbelt profiles.
func New(cfg Config) (Sandbox, error) {
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		return nil, fmt.Errorf("sandbox-exec not available: %w", err)
	}
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	return &darwinSandbox{cfg: cfg}, nil
}

func (s *darwinSandbox) Wrap(cmd *exec.Cmd) error {
	env := cmd.Env
	if env == nil {
		env = os.Environ()
	}
	filtered, stripped := filterEnv(env, s.cfg.AllowEnv)
	cmd.Env = filtered

	origArgs := cmd.Args
	denied := allDeniedPaths(s.cfg)
	slog.Debug("sandbox_wrap",
		"command", commandSummary(origArgs),
		"network", s.cfg.AllowNetwork,
		"deny_write", s.cfg.DenyWrite,
		"writable_paths", s.cfg.WritablePaths,
		"env_stripped", stripped,
		"denied_paths", len(denied))
	cmd.Path = "/usr/bin/sandbox-exec"
	// The profile is rebuilt on every wrap, not frozen at construction, so the
	// symlink resolution below tracks the filesystem as it is now.
	cmd.Args = append([]string{"sandbox-exec", "-p", buildProfile(s.cfg, denied)}, origArgs...)

	// Run in a new session, detaching the controlling terminal. This is the
	// macOS counterpart to bwrap's --new-session on Linux: it closes terminal
	// injection vectors and, by giving the process its own process group, makes
	// the (allow signal (target pgrp)) rule in the profile mean "own children
	// only" — so a sandboxed tool can signal its own jobs but not the user's
	// other processes. The two must change together.
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
	return nil
}

// pathAndResolved returns path plus its symlink-resolved target when that
// differs. Seatbelt matches resolved vnode paths, so a rule on a
// dotfiles-managed symlink (~/.npmrc -> ~/dotfiles/npmrc) never fires for the
// real file; rules must name both.
func pathAndResolved(path string) []string {
	if real, err := filepath.EvalSymlinks(path); err == nil && real != path {
		return []string{path, real}
	}
	return []string{path}
}

func buildProfile(cfg Config, deniedLists ...[]DeniedPath) string {
	deniedPaths := allDeniedPaths(cfg)
	if len(deniedLists) > 0 {
		deniedPaths = deniedLists[0]
	}
	var sb strings.Builder
	sb.WriteString("(version 1)\n")
	sb.WriteString("(allow default)\n")
	sb.WriteString("(deny file-write*)\n")

	// Always allow writes to the standard character devices. /dev/null in
	// particular is a universal shell idiom (`>/dev/null 2>&1`) and blocking
	// it breaks otherwise-innocuous tools. The kernel's device drivers handle
	// the discard/zero/random semantics; there's no data at rest, so allowing
	// these writes does not weaken the sandbox. Apply even under DenyWrite —
	// matches bwrap's behavior on Linux (--dev /dev gives a fresh devtmpfs).
	for _, dev := range []string{
		"/dev/null",
		"/dev/zero",
		"/dev/random",
		"/dev/urandom",
		"/dev/stdout",
		"/dev/stderr",
	} {
		sb.WriteString(fmt.Sprintf("(allow file-write* (literal %q))\n", dev))
	}

	if !cfg.DenyWrite {
		// Allow writes to OS temp dir and configured paths.
		// Include both /private/tmp and the runtime TMPDIR (typically
		// /var/folders/...) so that programs using os.TempDir() work.
		writePaths := []string{"/private/tmp"}
		if tmpdir := os.TempDir(); tmpdir != "" && tmpdir != "/private/tmp" && tmpdir != "/tmp" {
			writePaths = append(writePaths, tmpdir)
		}
		for _, p := range cfg.WritablePaths {
			writePaths = append(writePaths, expandTilde(p))
		}

		for _, p := range writePaths {
			resolved, err := filepath.EvalSymlinks(p)
			if err != nil {
				resolved = p
			}
			sb.WriteString(fmt.Sprintf("(allow file-write* (subpath %q))\n", resolved))
		}
	}

	// Deny read access to sensitive credential paths.
	for _, denied := range deniedPaths {
		for _, p := range pathAndResolved(denied.Path) {
			sb.WriteString(fmt.Sprintf("(deny file-read* (subpath %q))\n", p))
		}
	}

	// Re-allow read access for exempted paths (last-match-wins in Seatbelt).
	for _, p := range cfg.ReadPaths {
		for _, rp := range pathAndResolved(expandTilde(p)) {
			sb.WriteString(fmt.Sprintf("(allow file-read* (subpath %q))\n", rp))
		}
	}

	// Deny signaling unrelated processes while still allowing a script to manage
	// its own descendants. (target same-sandbox) matches exactly the processes
	// running inside this sandbox instance — the script's own children,
	// including ones it detached into their own session/process group (job
	// control, `setsid` workers) — while the user's other processes, which are
	// not in the sandbox, stay unreachable. This is stricter and more robust
	// than a process-group scope: it doesn't depend on the Setsid in Wrap and
	// doesn't break a tool that re-groups its children. macOS approximation of
	// the isolation Linux gets for free from the PID namespace (where other
	// processes are simply invisible).
	sb.WriteString("(deny signal)\n")
	sb.WriteString("(allow signal (target self))\n")
	sb.WriteString("(allow signal (target same-sandbox))\n")

	if !cfg.AllowNetwork {
		sb.WriteString("(deny network*)\n")
	} else if cfg.DenyDNS {
		// Block macOS system resolver (mDNSResponder Unix domain socket).
		sb.WriteString("(deny network-outbound (to unix-socket (path-literal \"/private/var/run/mDNSResponder\")))\n")
		// Block direct DNS queries (port 53) as a fallback.
		sb.WriteString("(deny network-outbound (remote udp \"*:53\"))\n")
		sb.WriteString("(deny network-outbound (remote tcp \"*:53\"))\n")
	}

	return sb.String()
}
