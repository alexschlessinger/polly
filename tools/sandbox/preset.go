package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PresetNames lists the valid components of a sandbox preset spec, for help
// text and error messages.
var PresetNames = []string{"base", "readonly", "workspace", "net"}

// ParsePreset builds a Config from a preset spec: one or more preset names
// joined with "+" (e.g. "workspace+net"). Components merge onto the base
// config, so every spec keeps temp-dir writes unless readonly denies them.
//
//	base      — the default sandbox: temp-dir writes only, no network
//	readonly  — deny all writes, including temp (analysis only)
//	workspace — the working directory is writable, with .git/hooks and
//	            .git/config carved back out as read-only so a sandboxed
//	            tool can't plant hooks that run unsandboxed later
//	net       — allow outbound network
//
// An empty spec is the base config. Unknown names error so a typo fails
// closed instead of silently running with a different policy.
func ParsePreset(spec string) (Config, error) {
	cfg := DefaultConfig()
	if strings.TrimSpace(spec) == "" {
		return cfg, nil
	}
	for _, part := range strings.Split(spec, "+") {
		switch strings.TrimSpace(part) {
		case "base":
			// the starting point; nothing to add
		case "readonly":
			cfg.DenyWrite = true
		case "net":
			cfg.AllowNetwork = true
		case "workspace":
			cwd, err := os.Getwd()
			if err != nil {
				return Config{}, fmt.Errorf("sandbox preset %q: resolve working directory: %w", part, err)
			}
			cfg.WritablePaths = append(cfg.WritablePaths, cwd)
			cfg.DenyWritePaths = append(cfg.DenyWritePaths, gitGuardrailPaths(cwd)...)
		default:
			return Config{}, fmt.Errorf("unknown sandbox preset %q (valid: %s, joined with +)",
				strings.TrimSpace(part), strings.Join(PresetNames, ", "))
		}
	}
	return cfg, nil
}

// gitGuardrailPaths returns the hooks dir and config file of the git
// repository at dir, if any. Making a repository writable re-opens two
// escalations out of the sandbox: writing .git/hooks/* (executed unsandboxed
// on the user's next git operation) and pointing core.hooksPath somewhere
// writable via .git/config. Both paths therefore go on DenyWritePaths.
//
// A .git *file* (linked worktree or submodule) is followed via its
// "gitdir:" pointer, and a worktree's commondir is included too — hooks and
// config live in the common dir, not the per-worktree one.
func gitGuardrailPaths(dir string) []string {
	gitPath := filepath.Join(dir, ".git")
	fi, err := os.Stat(gitPath)
	if err != nil {
		return nil
	}
	gitDir := gitPath
	if !fi.IsDir() {
		target, ok := readGitPointer(gitPath, "gitdir:")
		if !ok {
			return nil
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(dir, target)
		}
		gitDir = filepath.Clean(target)
	}
	gitDirs := []string{gitDir}
	if common, ok := readGitPointer(filepath.Join(gitDir, "commondir"), ""); ok {
		if !filepath.IsAbs(common) {
			common = filepath.Join(gitDir, common)
		}
		gitDirs = append(gitDirs, filepath.Clean(common))
	}
	paths := make([]string, 0, 2*len(gitDirs))
	for _, d := range gitDirs {
		paths = append(paths, filepath.Join(d, "hooks"), filepath.Join(d, "config"))
	}
	return paths
}

// readGitPointer reads a single-line git pointer file (a .git file's
// "gitdir: <path>" or a worktree's commondir) and returns the path after
// stripping prefix.
func readGitPointer(path, prefix string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	line, _, _ := strings.Cut(string(data), "\n")
	if prefix != "" {
		var found bool
		if line, found = strings.CutPrefix(line, prefix); !found {
			return "", false
		}
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return "", false
	}
	return line, true
}
