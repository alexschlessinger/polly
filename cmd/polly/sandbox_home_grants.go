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
	grants := existingHomeGrants(candidates)
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

// existingHomeGrants keeps the candidates that exist strictly inside the home
// directory, canonical and deduplicated.
func existingHomeGrants(candidates []string) []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	home = canonicalWarningPath(home)
	seen := make(map[string]bool, len(candidates))
	var grants []string
	for _, candidate := range candidates {
		real, err := filepath.EvalSymlinks(filepath.Clean(candidate))
		if err != nil {
			continue
		}
		real = filepath.Clean(real)
		if real == home || !sandbox.PathWithin(real, home) || seen[real] {
			continue
		}
		seen[real] = true
		grants = append(grants, real)
	}
	return grants
}

// exposeWorkingDirectory keeps the working directory readable when no
// writable grant covers it, so read-only and base presets still see the
// project inside a private home. A working directory at or above the home
// directory is left alone: exposing it would re-open the whole home.
func exposeWorkingDirectory(cfg sandbox.Config, warnings *broadWritablePathWarner, quiet bool) (sandbox.Config, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return cfg, nil
	}
	cwd, err = filepath.EvalSymlinks(cwd)
	if err != nil {
		return cfg, nil
	}
	cwd = filepath.Clean(cwd)
	if home, err := os.UserHomeDir(); err == nil {
		if home = canonicalWarningPath(home); home != "" && sandbox.PathWithin(home, cwd) {
			if warnings != nil && !quiet {
				warnings.emit(cwd, "working directory "+cwd+" is your home directory or above it; sandboxed tools only see granted paths there, so run polly from a project directory or grant paths with --readpath")
			}
			return cfg, nil
		}
	}
	if !cfg.DenyWrite {
		for _, writable := range cfg.WritablePaths {
			if sandbox.PathWithin(cwd, writable) {
				return cfg, nil
			}
		}
	}
	return sandbox.ExposeReadOnlyPaths(cfg, cwd)
}
