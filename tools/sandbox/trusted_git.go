package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
)

// TrustedGitExecutable selects the same audited Git used by workspace policy
// preparation. Runtime Git plumbing must still execute through a sandbox.
func TrustedGitExecutable(writableRoots []string) (string, error) {
	return trustedGitExecutable(writableRoots)
}

// RuntimeGitReadConfig exposes a checkout and its Git routing directories for
// fixed runtime plumbing, including when Linux hides their host temp directory.
// Discovery reads only routing files and never grants an exemption from denies.
func RuntimeGitReadConfig(base Config, root string) (Config, error) {
	entry := filepath.Join(root, ".git")
	if err := ReadAllowed(base, entry); err != nil {
		return Config{}, err
	}
	info, err := os.Lstat(entry)
	if err != nil {
		return Config{}, err
	}
	gitDir := entry
	switch {
	case info.IsDir():
	case info.Mode().IsRegular() && !hasMultipleLinks(info):
		gitDir, err = readGitPointer(entry, "gitdir:")
		if err != nil {
			return Config{}, err
		}
		if !filepath.IsAbs(gitDir) {
			gitDir = filepath.Join(root, gitDir)
		}
	default:
		return Config{}, fmt.Errorf("unsupported Git routing entry: %s", entry)
	}
	if err := ReadAllowed(base, gitDir); err != nil {
		return Config{}, err
	}
	gitDir, err = resolveGitDir(gitDir)
	if err != nil {
		return Config{}, err
	}
	paths := []string{root, gitDir}
	pointer := filepath.Join(gitDir, "commondir")
	if err := ReadAllowed(base, pointer); err != nil {
		return Config{}, err
	}
	info, err = os.Lstat(pointer)
	if err == nil {
		if !info.Mode().IsRegular() || hasMultipleLinks(info) {
			return Config{}, fmt.Errorf("unsupported Git common-directory pointer: %s", pointer)
		}
		common, err := readGitPointer(pointer, "")
		if err != nil {
			return Config{}, err
		}
		if !filepath.IsAbs(common) {
			common = filepath.Join(gitDir, common)
		}
		if err := ReadAllowed(base, common); err != nil {
			return Config{}, err
		}
		common, err = resolveGitDir(common)
		if err != nil {
			return Config{}, err
		}
		paths = append(paths, common)
	} else if !os.IsNotExist(err) {
		return Config{}, err
	}
	return ExposeReadOnlyPaths(base, paths...)
}

// RuntimeGitConfig is only for the host's fixed Git plumbing operations, never
// an agent's bash, shell, MCP, or custom tool. It replaces preset-generated
// whole-tree pins for the selected repository with metadata leaf protections.
// Explicit deny rules (including an identical path added via Merge), unrelated
// repository pins, credential restrictions, and DenyWrite remain authoritative.
// The returned config must not replace a registry's base policy.
func RuntimeGitConfig(base Config, commonDir, directory string) (Config, error) {
	common, err := resolveGitDir(commonDir)
	if err != nil {
		return Config{}, err
	}
	if common != commonDir {
		return Config{}, fmt.Errorf("runtime Git directory must be canonical: %s", commonDir)
	}
	if !filepath.IsAbs(directory) || pathWithinPolicy(directory, common) {
		return Config{}, fmt.Errorf("runtime directory must be absolute and outside Git metadata")
	}
	cfg := base.Merge(Config{})
	cfg.WritablePaths = []string{directory, common}
	cfg.AllowNetwork = false
	cfg.AllowUnixSockets = nil
	// Preset provenance is private and survives preparation and Merge. Remove
	// exactly the generated occurrences; a caller's duplicate deny still wins.
	generated := map[string]int{}
	for i := range cfg.gitPolicies {
		policy := &cfg.gitPolicies[i]
		var kept []string
		for _, path := range policy.protected {
			if path == common || path == filepath.Join(common, "worktrees") {
				generated[path]++
			} else {
				kept = append(kept, path)
			}
		}
		policy.protected = kept
	}
	var denied []string
	for _, path := range cfg.DenyWritePaths {
		if generated[path] > 0 {
			generated[path]--
		} else {
			denied = append(denied, path)
		}
	}
	cfg.DenyWritePaths = denied
	// Git's executable configuration and the parent's own index/HEAD/branches
	// remain read-only even to runtime plumbing. Linked parent metadata retains
	// its original pin; new runtime worktree entries are administered by Git.
	for _, name := range []string{"config", "config.worktree", "hooks", "HEAD", "index", "refs/heads", "refs/tags", "logs/refs/heads", "modules"} {
		path := filepath.Join(common, filepath.FromSlash(name))
		if _, err := os.Lstat(path); err == nil {
			cfg.DenyWritePaths = append(cfg.DenyWritePaths, path)
		} else if !os.IsNotExist(err) {
			return Config{}, err
		}
	}
	for _, policy := range cfg.gitPolicies {
		for _, repository := range policy.repositories {
			if repository.gitDir != common && pathWithinPolicy(repository.gitDir, common) {
				cfg.DenyWritePaths = append(cfg.DenyWritePaths, repository.gitDir)
			}
		}
	}
	for _, name := range []string{"objects", "refs/polly/snapshots", "worktrees"} {
		if err := WriteAllowed(cfg, filepath.Join(common, filepath.FromSlash(name))); err != nil {
			return Config{}, fmt.Errorf("runtime Git administration is blocked by an explicit sandbox restriction: %w", err)
		}
	}
	return PrepareConfig(cfg)
}
