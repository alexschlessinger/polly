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
// are tested, and depth is judged on canonical spellings — matching the
// masking the OS backends apply. A prepared config whose frozen grant has
// been rerouted or replaced since preparation fails closed, as the backends
// do before wrapping a command.
func ReadAllowed(cfg Config, path string) error {
	return checkReadPolicy(cfg, path, policyPrivateRoots())
}

// ReadMasked is ReadAllowed without the private-root rule: it reports only
// whether a denied path masks the read. Runtime plumbing that itself creates
// visibility inside private roots (ExposeReadOnlyPaths) uses it, as do
// callers that must tell an operator's explicit denial apart from the
// structural privacy of the home directory.
func ReadMasked(cfg Config, path string) error {
	return checkReadPolicy(cfg, path, nil)
}

func checkReadPolicy(cfg Config, path string, privateRoots []string) error {
	if err := validateAuthorityPathIdentities(cfg.authorityPaths); err != nil {
		return err
	}
	path = filepath.Clean(expandTilde(path))
	if !filepath.IsAbs(path) {
		return fmt.Errorf("path %q is not absolute", path)
	}
	roots := policyRoutes(privateRoots...)
	grants := readGrantRoutes(cfg, privateRoots)
	masks := maskRoutes(cfg)
	for _, candidate := range readPolicyCandidates(path) {
		grant := deepestContaining(candidate, grants)
		if deepestContaining(candidate, roots) > grant {
			return fmt.Errorf("path %q is inside a private directory the sandbox policy does not grant", path)
		}
		if deepestContaining(candidate, masks) > grant {
			return fmt.Errorf("path %q is blocked from reads by the sandbox policy", path)
		}
	}
	return nil
}

// readPolicyCandidates is the lexical path plus its canonical route when that
// differs. A path that does not exist yet resolves through its deepest
// existing ancestor, so a query spelled through a symlink alias still meets
// the canonical rules that normalization produced.
func readPolicyCandidates(path string) []string {
	candidates := []string{path}
	if canonical := canonicalPolicyPath(path); canonical != path {
		candidates = append(candidates, canonical)
	}
	return candidates
}

// policyPrivateRoots lists the directories the in-process policy hides:
// nothing inside them is readable without a grant. Only directories the
// platform backend hides from wrapped commands qualify; host temp stays
// readable in-process because DenyHostTemp governs its write grant.
func policyPrivateRoots() []string {
	return platformPrivatePolicyRoots()
}

// readGrantRoutes lists every path a wrapped command may read inside a private
// root: read and visible grants, writable grants, and host temp unless
// DenyHostTemp withholds it, each in lexical and canonical spellings. A grant
// equal to a private root is dropped; the root wins that tie.
func readGrantRoutes(cfg Config, privateRoots []string) []policyRoute {
	paths := []string{}
	if !cfg.DenyHostTemp {
		paths = append(paths, "/tmp", os.TempDir())
	}
	paths = append(paths, cfg.ReadPaths...)
	paths = append(paths, cfg.visiblePaths...)
	if !cfg.DenyWrite {
		paths = append(paths, cfg.WritablePaths...)
	}
	return grantRoutesOutsideRoots(paths, privateRoots)
}

// maskRoutes lists the denied paths in lexical and canonical spellings. Entries
// are kept whether or not they exist, so a historical path stays masked.
func maskRoutes(cfg Config) []policyRoute {
	var paths []string
	for _, denied := range allDeniedPaths(cfg) {
		paths = append(paths, denied.Path)
	}
	return policyRoutes(paths...)
}

// policyRoute is one spelling of a policy path together with the depth of its
// canonical route. Containment is judged per spelling (lexically and by
// filesystem identity) while depth compares canonical routes only, so an
// alias one segment shorter than the real path (/var against /private/var)
// never wins or loses a tie by its spelling.
type policyRoute struct {
	path  string
	depth int
}

// policyRoutes expands each policy path to its lexical and, when it resolves
// differently, canonical spellings, deduplicated in order. Both carry the
// canonical depth.
func policyRoutes(paths ...string) []policyRoute {
	var out []policyRoute
	seen := make(map[string]bool, len(paths))
	add := func(path string, depth int) {
		if !seen[path] {
			seen[path] = true
			out = append(out, policyRoute{path: path, depth: depth})
		}
	}
	for _, path := range paths {
		path = filepath.Clean(expandTilde(path))
		canonical := canonicalPolicyPath(path)
		depth := pathDepth(canonical)
		add(path, depth)
		add(canonical, depth)
	}
	return out
}

// canonicalPolicyPath is the spelling the kernel would resolve path to: every
// existing symlink prefix resolved and the missing remainder appended
// lexically. A path that cannot be resolved keeps its cleaned spelling.
func canonicalPolicyPath(path string) string {
	path = filepath.Clean(path)
	if resolved, err := resolveExistingPathPrefix(path); err == nil {
		return filepath.Clean(resolved)
	}
	return path
}

// grantRoutesOutsideRoots expands grants to policy routes, dropping any grant
// whose canonical route is itself a private root: the root wins that tie.
func grantRoutesOutsideRoots(paths, privateRoots []string) []policyRoute {
	rootRoutes := make(map[string]bool, len(privateRoots))
	for _, root := range privateRoots {
		rootRoutes[canonicalPolicyPath(expandTilde(root))] = true
	}
	routes := policyRoutes(paths...)
	kept := routes[:0]
	for _, route := range routes {
		if !rootRoutes[canonicalPolicyPath(route.path)] {
			kept = append(kept, route)
		}
	}
	return kept
}

// deepestContaining returns the canonical depth of the deepest route
// containing path, or -1 when none does.
func deepestContaining(path string, routes []policyRoute) int {
	deepest := -1
	for _, route := range routes {
		if route.depth > deepest && pathWithinPolicy(path, route.path) {
			deepest = route.depth
		}
	}
	return deepest
}
