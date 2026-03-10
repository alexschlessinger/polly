//go:build linux

package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

type linuxSandbox struct {
	args            []string // pre-built bwrap args (everything before --)
	placeholderFile string
}

// New creates a Sandbox for Linux using bubblewrap (bwrap).
func New(cfg Config) (Sandbox, error) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		return nil, fmt.Errorf("bwrap not available: %w", err)
	}
	placeholderFile, err := ensurePlaceholderFile()
	if err != nil {
		return nil, fmt.Errorf("prepare placeholder file: %w", err)
	}
	deniedPaths := ExpandHome(DeniedPaths)
	return &linuxSandbox{
		args:            buildBwrapArgs(cfg, deniedPaths, placeholderFile),
		placeholderFile: placeholderFile,
	}, nil
}

func (s *linuxSandbox) Wrap(cmd *exec.Cmd) error {
	origArgs := cmd.Args
	bwrapPath, _ := exec.LookPath("bwrap")
	cmd.Path = bwrapPath
	cmd.Args = make([]string, 0, len(s.args)+1+len(origArgs))
	cmd.Args = append(cmd.Args, s.args...)
	cmd.Args = append(cmd.Args, "--")
	cmd.Args = append(cmd.Args, origArgs...)
	return nil
}

func buildBwrapArgs(cfg Config, deniedPaths []DeniedPath, placeholderFile string) []string {
	args := []string{"bwrap"}

	// Read-only root filesystem.
	args = append(args, "--ro-bind", "/", "/")

	// Writable bind mounts.
	args = append(args, "--bind", "/tmp", "/tmp")
	for _, p := range cfg.WritablePaths {
		args = append(args, "--bind", p, p)
	}

	// Overlay sensitive credential paths based on whether they are directories or files.
	for _, denied := range deniedPaths {
		switch denied.Kind {
		case DeniedPathFile:
			args = append(args, "--ro-bind", placeholderFile, denied.Path)
		default:
			args = append(args, "--tmpfs", denied.Path)
		}
	}

	// Basic device and proc mounts.
	args = append(args, "--dev", "/dev")
	args = append(args, "--proc", "/proc")

	if !cfg.AllowNetwork {
		args = append(args, "--unshare-net")
	}

	args = append(args, "--die-with-parent")

	return args
}

func ensurePlaceholderFile() (string, error) {
	path := filepath.Join(os.TempDir(), "pollytool-sandbox-empty")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return path, nil
}
