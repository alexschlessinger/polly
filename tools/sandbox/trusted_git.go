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
