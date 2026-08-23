//go:build linux

package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type linuxSandbox struct {
	args            []string // pre-built bwrap args (everything before --)
	placeholderFile string
	allowEnv        []string
}

// New creates a Sandbox for Linux using bubblewrap (bwrap).
func New(cfg Config) (Sandbox, error) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		return nil, fmt.Errorf("bwrap not available: %w", err)
	}
	cfg, err := PrepareConfig(cfg)
	if err != nil {
		return nil, err
	}
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	placeholderFile, err := ensurePlaceholderFile()
	if err != nil {
		return nil, fmt.Errorf("prepare placeholder file: %w", err)
	}
	deniedPaths := existingDeniedPaths(ExpandHome(DeniedPaths))
	return &linuxSandbox{
		args:            buildBwrapArgs(cfg, deniedPaths, placeholderFile),
		placeholderFile: placeholderFile,
		allowEnv:        cfg.AllowEnv,
	}, nil
}

// existingDeniedPaths resolves each deny-path to its real location and drops the
// ones that don't exist. Two failure modes motivate this:
//
//   - Missing paths: masking one forces bwrap to mkdir a mountpoint under the
//     read-only root bind, which fails ("Can't mkdir <path>: Read-only file
//     system") and aborts the whole sandbox — so one absent credential dir
//     (e.g. ~/.gnupg) would break every command.
//   - Symlinks (e.g. WSL's ~/.aws -> /mnt/c/Users/.../.aws): bwrap can't mount a
//     tmpfs on the link itself ("Can't mount tmpfs ...: No such file or
//     directory"), and masking the link wouldn't cover the real target anyway.
//
// EvalSymlinks handles both: it returns an error for missing paths (skip them)
// and the resolved real path for symlinks (mask that instead). Resolved paths
// are de-duplicated so two links to the same target don't double-mask.
func existingDeniedPaths(paths []DeniedPath) []DeniedPath {
	kept := make([]DeniedPath, 0, len(paths))
	seen := make(map[string]bool, len(paths))
	for _, p := range paths {
		real, err := filepath.EvalSymlinks(p.Path)
		if err != nil || seen[real] {
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
	for _, denied := range deniedPaths {
		if readSet[denied.Path] || isUnderAny(denied.Path, readSet) {
			continue
		}
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
	} else if cfg.DenyDNS {
		args = append(args, "--ro-bind", placeholderFile, "/etc/resolv.conf")
	}

	args = append(args, "--die-with-parent")

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

func freezeAuthorityPathsForPlatform(cfg Config) (Config, error) {
	return freezeAuthorityPaths(cfg)
}
