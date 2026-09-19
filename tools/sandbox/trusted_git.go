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
// fixed runtime plumbing, including when the backend hides their host temp or
// home directory. Discovery reads only routing files and never grants an
// exemption from denied paths; it is itself what makes the routing visible
// inside private roots.
func RuntimeGitReadConfig(base Config, root string) (Config, error) {
	dirs, err := checkoutGitDirs(base, root)
	if err != nil {
		return Config{}, err
	}
	return ExposeReadOnlyPaths(base, append([]string{root}, dirs...)...)
}

// ExposeCheckoutGit keeps the Git directories a checkout's .git entry routes
// to readable inside private roots: its gitdir and, for a linked worktree,
// the repository's common directory. A linked worktree's metadata lives in
// the main checkout, often inside the private home, and no Git command
// works in the worktree without it. The checkout itself is not exposed, and
// directories the policy already lets tools read are left alone. Like
// RuntimeGitReadConfig it never overrides a denied path, and it grants no
// writes: the workspace preset pins a gitdir outside the workspace
// read-only.
func ExposeCheckoutGit(base Config, root string) (Config, error) {
	dirs, err := checkoutGitDirs(base, root)
	if err != nil {
		return Config{}, err
	}
	var hidden []string
	for _, dir := range dirs {
		if ReadAllowed(base, dir) != nil {
			hidden = append(hidden, dir)
		}
	}
	if len(hidden) == 0 {
		return base, nil
	}
	return ExposeReadOnlyPaths(base, hidden...)
}

// checkoutGitDirs follows root's .git entry to the canonical Git directories
// it routes to: the gitdir, then the common directory when the gitdir has a
// commondir pointer. It refuses a routing file or directory a denied path
// masks under base, and routing it cannot pin safely.
func checkoutGitDirs(base Config, root string) ([]string, error) {
	entry := filepath.Join(root, ".git")
	if err := ReadMasked(base, entry); err != nil {
		return nil, err
	}
	info, err := os.Lstat(entry)
	if err != nil {
		return nil, err
	}
	gitDir := entry
	switch {
	case info.IsDir():
	case info.Mode().IsRegular() && !hasMultipleLinks(info):
		gitDir, err = readGitPointer(entry, "gitdir:")
		if err != nil {
			return nil, err
		}
		if !filepath.IsAbs(gitDir) {
			gitDir = filepath.Join(root, gitDir)
		}
	default:
		return nil, fmt.Errorf("unsupported Git routing entry: %s", entry)
	}
	if err := ReadMasked(base, gitDir); err != nil {
		return nil, err
	}
	gitDir, err = resolveGitDir(gitDir)
	if err != nil {
		return nil, err
	}
	dirs := []string{gitDir}
	pointer := filepath.Join(gitDir, "commondir")
	if err := ReadMasked(base, pointer); err != nil {
		return nil, err
	}
	info, err = os.Lstat(pointer)
	if err == nil {
		if !info.Mode().IsRegular() || hasMultipleLinks(info) {
			return nil, fmt.Errorf("unsupported Git common-directory pointer: %s", pointer)
		}
		common, err := readGitPointer(pointer, "")
		if err != nil {
			return nil, err
		}
		if !filepath.IsAbs(common) {
			common = filepath.Join(gitDir, common)
		}
		if err := ReadMasked(base, common); err != nil {
			return nil, err
		}
		common, err = resolveGitDir(common)
		if err != nil {
			return nil, err
		}
		dirs = append(dirs, common)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return dirs, nil
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
