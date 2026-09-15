package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
)

// ReadAllowed reports whether an in-process read of path is consistent with
// the read policy this config applies to wrapped commands. Rules are
// path-scoped and the deepest rule containing a path decides: a private root
// (a directory the platform backend hides, such as the home directory) denies
// everything inside it unless a deeper read or write grant covers the path,
// and a denied path (the built-in credential list plus cfg.DenyPaths) is
// masked unless a grant at the same or a deeper path covers it. Tools that
// read files directly (rather than through a wrapped process) use this so
// they cannot see paths a sandboxed command could not. The check is
// best-effort against symlinks — both the lexical path and its resolved route
// are tested — matching the masking the OS backends apply.
func ReadAllowed(cfg Config, path string) error {
	return checkReadPolicy(cfg, path, policyPrivateRoots(cfg))
}

// readMasked is ReadAllowed without the private-root rule: it reports only
// whether a denied path masks the read. Runtime plumbing that itself creates
// visibility inside private roots (ExposeReadOnlyPaths) uses it.
func readMasked(cfg Config, path string) error {
	return checkReadPolicy(cfg, path, nil)
}

func checkReadPolicy(cfg Config, path string, privateRoots []string) error {
	path = filepath.Clean(expandTilde(path))
	grants := readGrantRoutes(cfg, privateRoots)
	masks := maskRoutes(cfg)
	for _, candidate := range readPolicyCandidates(path) {
		grant := deepestContaining(candidate, grants)
		if deepestContaining(candidate, privateRoots) > grant {
			return fmt.Errorf("path %q is inside a private directory the sandbox policy does not grant", path)
		}
		if deepestContaining(candidate, masks) > grant {
			return fmt.Errorf("path %q is blocked from reads by the sandbox policy", path)
		}
	}
	return nil
}

// readPolicyCandidates is the lexical path plus its fully resolved route when
// that differs.
func readPolicyCandidates(path string) []string {
	candidates := []string{path}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		if resolved = filepath.Clean(resolved); resolved != path {
			candidates = append(candidates, resolved)
		}
	}
	return candidates
}

// policyPrivateRoots lists the directories the in-process policy hides:
// nothing inside them is readable without a grant. Only directories the
// platform backend hides from wrapped commands qualify; host temp stays
// readable in-process because DenyHostTemp governs its write grant.
func policyPrivateRoots(cfg Config) []string {
	return platformPrivatePolicyRoots()
}

// readGrantRoutes lists every path a wrapped command may read inside a private
// root: read and visible grants, writable grants, and host temp unless
// DenyHostTemp withholds it, each in lexical and resolved spellings. A grant
// equal to a private root is dropped; the root wins that tie.
func readGrantRoutes(cfg Config, privateRoots []string) []string {
	paths := []string{}
	if !cfg.DenyHostTemp {
		paths = append(paths, "/tmp", os.TempDir())
	}
	paths = append(paths, cfg.ReadPaths...)
	paths = append(paths, cfg.visiblePaths...)
	if !cfg.DenyWrite {
		paths = append(paths, cfg.WritablePaths...)
	}
	routes := writePolicyRoutes(paths...)
	kept := routes[:0]
	for _, route := range routes {
		if !pathEqualsAny(route, privateRoots) {
			kept = append(kept, route)
		}
	}
	return kept
}

// maskRoutes lists the denied paths in lexical and resolved spellings. Entries
// are kept whether or not they exist, so a historical path stays masked.
func maskRoutes(cfg Config) []string {
	var paths []string
	for _, denied := range allDeniedPaths(cfg) {
		paths = append(paths, denied.Path)
	}
	return writePolicyRoutes(paths...)
}

// deepestContaining returns the depth of the deepest route containing path,
// or -1 when none does.
func deepestContaining(path string, routes []string) int {
	deepest := -1
	for _, route := range routes {
		if depth := pathDepth(route); depth > deepest && pathWithinPolicy(path, route) {
			deepest = depth
		}
	}
	return deepest
}
