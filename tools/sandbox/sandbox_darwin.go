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
	profile  string
	allowEnv []string
}

// New creates a Sandbox for macOS using sandbox-exec with Seatbelt profiles.
func New(cfg Config) (Sandbox, error) {
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		return nil, fmt.Errorf("sandbox-exec not available: %w", err)
	}
	return &darwinSandbox{profile: buildProfile(cfg), allowEnv: cfg.AllowEnv}, nil
}

func (s *darwinSandbox) Wrap(cmd *exec.Cmd) error {
	env := cmd.Env
	if env == nil {
		env = os.Environ()
	}
	if len(s.allowEnv) > 0 {
		allowed := make(map[string]bool, len(s.allowEnv))
		for _, k := range s.allowEnv {
			allowed[k] = true
		}
		var filtered []string
		for _, e := range env {
			if k, _, _ := strings.Cut(e, "="); allowed[k] {
				filtered = append(filtered, e)
			}
		}
		cmd.Env = filtered
	} else {
		var filtered []string
		for _, e := range env {
			if k, _, _ := strings.Cut(e, "="); !strings.HasPrefix(k, "POLLYTOOL_") {
				filtered = append(filtered, e)
			}
		}
		cmd.Env = filtered
	}
	origArgs := cmd.Args
	cmd.Path = "/usr/bin/sandbox-exec"
	cmd.Args = append([]string{"sandbox-exec", "-p", s.profile}, origArgs...)
	return nil
}

func buildProfile(cfg Config) string {
	var sb strings.Builder
	sb.WriteString("(version 1)\n")
	sb.WriteString("(allow default)\n")
	sb.WriteString("(deny file-write*)\n")

	if !cfg.DenyWrite {
		// Allow writes to OS temp dir and configured paths.
		writePaths := []string{"/private/tmp"}
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
	for _, denied := range ExpandHome(DeniedPaths) {
		sb.WriteString(fmt.Sprintf("(deny file-read* (subpath %q))\n", denied.Path))
	}

	// Re-allow read access for exempted paths (last-match-wins in Seatbelt).
	for _, p := range cfg.ReadPaths {
		sb.WriteString(fmt.Sprintf("(allow file-read* (subpath %q))\n", expandTilde(p)))
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
