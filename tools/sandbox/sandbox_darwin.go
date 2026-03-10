//go:build darwin

package sandbox

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

type darwinSandbox struct {
	profile string
}

// New creates a Sandbox for macOS using sandbox-exec with Seatbelt profiles.
func New(cfg Config) (Sandbox, error) {
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		return nil, fmt.Errorf("sandbox-exec not available: %w", err)
	}
	return &darwinSandbox{profile: buildProfile(cfg)}, nil
}

func (s *darwinSandbox) Wrap(cmd *exec.Cmd) error {
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

	// Always allow writes to OS temp dir.
	writePaths := []string{"/private/tmp"}
	writePaths = append(writePaths, cfg.WritablePaths...)

	for _, p := range writePaths {
		resolved, err := filepath.EvalSymlinks(p)
		if err != nil {
			resolved = p
		}
		sb.WriteString(fmt.Sprintf("(allow file-write* (subpath %q))\n", resolved))
	}

	// Deny read access to sensitive credential paths.
	for _, denied := range ExpandHome(DeniedPaths) {
		sb.WriteString(fmt.Sprintf("(deny file-read* (subpath %q))\n", denied.Path))
	}

	if !cfg.AllowNetwork {
		sb.WriteString("(deny network*)\n")
	}

	return sb.String()
}
