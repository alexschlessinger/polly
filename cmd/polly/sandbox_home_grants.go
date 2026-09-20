package main

import (
	"os"
	"path/filepath"

	"github.com/alexschlessinger/pollytool/skills"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

// homeReadGrants lists the read-only grants the CLI adds on top of the
// preset's toolchain grants: the skill directories in use, the default skill
// directory, the remote skill cache, the attachment cache, and every
// --readpath. Automatic entries are kept only when they exist inside the home
// directory, the only place a grant is needed; explicit --readpath entries
// pass through verbatim so PrepareConfig reports them as the user spelled them.
func homeReadGrants(config *Config, skillRoots []string) []string {
	candidates := append([]string(nil), skillRoots...)
	if dir, found, err := skills.DefaultDir(); err == nil && found {
		candidates = append(candidates, dir)
	}
	if dir, err := skills.CacheDir(); err == nil {
		candidates = append(candidates, dir)
	}
	if dir, err := attachmentCachePath(); err == nil {
		candidates = append(candidates, dir)
	}
	grants := sandbox.ExistingHomeGrants(candidates)
	if config != nil {
		grants = append(grants, config.ReadPaths...)
	}
	return grants
}

// skillCatalogRoots names the root directory of every discovered skill.
func skillCatalogRoots(result *skillCatalogResult) []string {
	if result == nil || result.catalog == nil {
		return nil
	}
	var roots []string
	for _, skill := range result.catalog.List() {
		if skill != nil && skill.RootDir != "" {
			roots = append(roots, skill.RootDir)
		}
	}
	return roots
}

// exposeWorkingDirectory keeps the working directory readable when no grant
// covers it, so read-only and base presets still see the project inside a
// private home. A working directory at or above the home directory is left
// alone: exposing it would re-open the whole home. A working directory that
// an explicit denial covers (--denypath, the session's private paths) is
// left alone too, with a warning: the operator's mask wins over the
// convenience grant. A working directory that cannot be resolved (deleted
// under polly, say) gets no grant and a warning rather than silence.
func exposeWorkingDirectory(cfg sandbox.Config, warnings *broadWritablePathWarner, quiet bool) (sandbox.Config, error) {
	cwd, err := os.Getwd()
	if err == nil {
		cwd, err = filepath.EvalSymlinks(cwd)
	}
	if err != nil {
		if warnings != nil && !quiet {
			warnings.emit("unresolved-cwd", "working directory cannot be resolved ("+err.Error()+"), so sandboxed tools may not see the project; run polly from an existing directory or grant paths with --readpath")
		}
		return cfg, nil
	}
	cwd = filepath.Clean(cwd)
	if home, err := os.UserHomeDir(); err == nil {
		if home = canonicalWarningPath(home); home != "" && sandbox.PathWithin(home, cwd) {
			if warnings != nil && !quiet && cfg.PrivateHome {
				warnings.emit(cwd, "working directory "+cwd+" is your home directory or above it; sandboxed tools only see granted paths there, so run polly from a project directory or grant paths with --readpath")
			}
			return cfg, nil
		}
	}
	// Ask the policy itself: a cwd already readable through a grant, or outside
	// every private root, needs nothing. A temp-only write grant does not count,
	// since the temp root is private and a home under it stays hidden.
	if sandbox.ReadAllowed(cfg, cwd) == nil {
		return cfg, nil
	}
	if err := sandbox.ReadMasked(cfg, cwd); err != nil {
		if warnings != nil && !quiet {
			warnings.emit("denied-cwd:"+cwd, "working directory "+cwd+" is inside a denied path, so sandboxed tools cannot read it; run polly elsewhere or drop the --denypath/POLLYTOOL_DENYPATHS entry that covers it")
		}
		return cfg, nil
	}
	return sandbox.ExposeReadOnlyPaths(cfg, cwd)
}

// exposeCheckoutGit keeps the Git metadata of a linked worktree or
// submodule readable when the working directory is its checkout: the .git
// file there routes to a gitdir, and for a linked worktree a common
// directory, in another checkout, often inside the private home, and no Git
// command works without them. A main checkout reads its own .git as part of
// the working directory, and swarm members are granted theirs, so this only
// gives the parent's tools the same. An empty .git file routes nowhere and
// is left alone; routing a denied path masks, or that cannot be pinned,
// gets a warning and no grant.
func exposeCheckoutGit(cfg sandbox.Config, warnings *broadWritablePathWarner, quiet bool) sandbox.Config {
	cwd, err := os.Getwd()
	if err == nil {
		cwd, err = filepath.EvalSymlinks(cwd)
	}
	if err != nil {
		return cfg
	}
	info, err := os.Lstat(filepath.Join(cwd, ".git"))
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return cfg
	}
	exposed, err := sandbox.ExposeCheckoutGit(cfg, cwd)
	if err != nil {
		if warnings != nil && !quiet {
			warnings.emit("checkout-git:"+cwd, "the Git metadata of "+cwd+" is not readable to sandboxed tools, so Git fails there: "+err.Error())
		}
		return cfg
	}
	return exposed
}
