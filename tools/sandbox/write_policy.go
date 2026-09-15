package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteAllowed reports whether an in-process write of path is consistent with
// the write policy this config applies to wrapped commands: writes are allowed
// only under the OS temp directories (withheld by DenyHostTemp) and
// cfg.WritablePaths, excluding the cfg.DenyWritePaths islands and any denied
// path or private root at or below the covering writable grant, and DenyWrite
// denies everything. The credential deny list and cfg.DenyPaths are never
// exempted from writes by a grant at the same path: a grant wins that tie for
// reads only. Tools that write files directly (rather than through a wrapped
// process) use this so they cannot change what a sandboxed command could not.
// The check is best-effort against symlinks — the lexical route and its
// resolved route are both tested, a target that does not exist yet is
// resolved through its deepest existing ancestor, and depth is judged on
// canonical spellings — matching the masking the OS backends apply. A
// prepared config whose frozen grant has been rerouted or replaced since
// preparation fails closed, as the backends do before wrapping a command.
func WriteAllowed(cfg Config, path string) error {
	if cfg.DenyWrite {
		return fmt.Errorf("path %q is blocked: the sandbox policy denies all file writes", path)
	}
	if err := validateAuthorityPathIdentities(cfg.authorityPaths); err != nil {
		return err
	}
	path = filepath.Clean(expandTilde(path))
	if !filepath.IsAbs(path) {
		return fmt.Errorf("path %q is not absolute", path)
	}
	candidates := readPolicyCandidates(path)
	for _, deny := range policyRoutes(cfg.DenyWritePaths...) {
		for _, candidate := range candidates {
			if pathWithinPolicy(candidate, deny.path) {
				return fmt.Errorf("path %q is blocked from writes by the sandbox policy", path)
			}
		}
	}
	// The write lands on the resolved route, so containment is judged there;
	// requiring the lexical route too would reject writable grants the OS
	// backends honor when reached through a symlinked spelling.
	target := candidates[len(candidates)-1]
	writable := deepestContaining(target, writableRootRoutes(cfg))
	if writable < 0 {
		return fmt.Errorf("path %q is outside the sandbox policy's writable paths", path)
	}
	masks := maskRoutes(cfg)
	privateRoots := policyRoutes(policyPrivateRoots(cfg)...)
	for _, candidate := range candidates {
		if deepestContaining(candidate, privateRoots) > writable {
			return fmt.Errorf("path %q is inside a private directory the sandbox policy does not grant", path)
		}
		if mask := deepestContaining(candidate, masks); mask >= 0 && mask >= writable {
			return fmt.Errorf("path %q is blocked from writes by the sandbox policy", path)
		}
	}
	return nil
}

// writableRootRoutes mirrors the write grants the OS backends give wrapped
// commands: the OS temp directories, unless DenyHostTemp withholds them, plus
// cfg.WritablePaths, each in lexical and canonical form. A grant equal to a
// private root is dropped; the root wins that tie.
func writableRootRoutes(cfg Config) []policyRoute {
	roots := []string{}
	if !cfg.DenyHostTemp {
		roots = append(roots, "/tmp", os.TempDir())
	}
	roots = append(roots, cfg.WritablePaths...)
	return grantRoutesOutsideRoots(roots, policyPrivateRoots(cfg))
}
