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

// GitRouting is a checkout's Git directory routing: GitDir holds its
// metadata (the .git directory itself, or the linked-worktree entry a .git
// file points to) and CommonDir is the shared repository directory a linked
// worktree's commondir names, empty for an ordinary checkout. Both are
// canonical paths.
type GitRouting struct {
	GitDir    string
	CommonDir string
}

// Linked reports whether the checkout is a linked worktree.
func (r GitRouting) Linked() bool { return r.CommonDir != "" }

// DiscoverGitRouting reads a checkout's routing files: .git as a directory
// or a single-link pointer file, then the commondir pointer. Symlinked
// routing fails closed, as it does for the sandbox policies. A root without
// a .git entry reports os.ErrNotExist.
func DiscoverGitRouting(root string) (GitRouting, error) {
	return discoverGitRouting(nil, root)
}

// discoverGitRouting is DiscoverGitRouting with each routing file checked
// against masks before it is read, when masks is set.
func discoverGitRouting(masks *Config, root string) (GitRouting, error) {
	check := func(path string) error {
		if masks == nil {
			return nil
		}
		return ReadMasked(*masks, path)
	}
	entry := filepath.Join(root, ".git")
	if err := check(entry); err != nil {
		return GitRouting{}, err
	}
	info, err := os.Lstat(entry)
	if err != nil {
		return GitRouting{}, err
	}
	gitDir := entry
	switch {
	case info.IsDir():
	case info.Mode().IsRegular() && !hasMultipleLinks(info):
		gitDir, err = readGitPointer(entry, "gitdir:")
		if err != nil {
			return GitRouting{}, err
		}
		if !filepath.IsAbs(gitDir) {
			gitDir = filepath.Join(root, gitDir)
		}
	default:
		return GitRouting{}, fmt.Errorf("unsupported Git routing entry: %s", entry)
	}
	if err := check(gitDir); err != nil {
		return GitRouting{}, err
	}
	gitDir, err = resolveGitDir(gitDir)
	if err != nil {
		return GitRouting{}, err
	}
	routing := GitRouting{GitDir: gitDir}
	pointer := filepath.Join(gitDir, "commondir")
	if err := check(pointer); err != nil {
		return GitRouting{}, err
	}
	info, err = os.Lstat(pointer)
	if err == nil {
		if !info.Mode().IsRegular() || hasMultipleLinks(info) {
			return GitRouting{}, fmt.Errorf("unsupported Git common-directory pointer: %s", pointer)
		}
		common, err := readGitPointer(pointer, "")
		if err != nil {
			return GitRouting{}, err
		}
		if !filepath.IsAbs(common) {
			common = filepath.Join(gitDir, common)
		}
		if err := check(common); err != nil {
			return GitRouting{}, err
		}
		common, err = resolveGitDir(common)
		if err != nil {
			return GitRouting{}, err
		}
		routing.CommonDir = common
	} else if !os.IsNotExist(err) {
		return GitRouting{}, err
	}
	return routing, nil
}

// RuntimeGitReadConfig exposes a checkout and its Git routing directories for
// fixed runtime plumbing, including when the backend hides their host temp or
// home directory. Discovery reads only routing files and never grants an
// exemption from denied paths; it is itself what makes the routing visible
// inside private roots.
func RuntimeGitReadConfig(base Config, root string) (Config, error) {
	routing, err := discoverGitRouting(&base, root)
	if err != nil {
		return Config{}, err
	}
	paths := []string{root, routing.GitDir}
	if routing.CommonDir != "" {
		paths = append(paths, routing.CommonDir)
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
	// The runtime grants itself these roots below; an operator's explicit read
	// denial of either stays authoritative rather than losing the tie.
	for _, path := range []string{directory, common} {
		if err := ReadMasked(base, path); err != nil {
			return Config{}, fmt.Errorf("runtime Git administration is blocked by an explicit sandbox restriction: %w", err)
		}
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
