//go:build darwin

package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type darwinSandbox struct {
	cfg Config
}

// New creates a Sandbox for macOS using sandbox-exec with Seatbelt profiles.
func New(cfg Config) (Sandbox, error) {
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		return nil, fmt.Errorf("sandbox-exec not available: %w", err)
	}
	// Without a home directory the credential deny list expands to nothing;
	// refuse to construct a sandbox that silently masks zero paths.
	if _, err := os.UserHomeDir(); err != nil {
		return nil, fmt.Errorf("cannot resolve home directory for credential masking: %w", err)
	}
	return &darwinSandbox{cfg: cfg}, nil
}

func (s *darwinSandbox) Wrap(cmd *exec.Cmd) error {
	env := cmd.Env
	if env == nil {
		env = os.Environ()
	}
	cmd.Env = filterEnv(env, s.cfg.AllowEnv)

	origArgs := cmd.Args
	cmd.Path = "/usr/bin/sandbox-exec"
	// The profile is rebuilt on every wrap, not frozen at construction, so the
	// symlink resolution below tracks the filesystem as it is now.
	cmd.Args = append([]string{"sandbox-exec", "-p", buildProfile(s.cfg)}, origArgs...)
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

func buildProfile(cfg Config) string {
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
	for _, denied := range allDeniedPaths(cfg) {
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
