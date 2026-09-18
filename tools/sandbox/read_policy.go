package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
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
//
// Loops that check many paths against one config compile it once with
// CompileReadPolicy instead.
func ReadAllowed(cfg Config, path string) error {
	policy, err := CompileReadPolicy(cfg)
	if err != nil {
		return err
	}
	return policy.Allowed(path)
}

// ReadMasked is ReadAllowed without the private-root rule: it reports only
// whether a denied path masks the read. Runtime plumbing that itself creates
// visibility inside private roots (ExposeReadOnlyPaths) uses it, as do
// callers that must tell an operator's explicit denial apart from the
// structural privacy of the home directory.
func ReadMasked(cfg Config, path string) error {
	policy, err := compileReadPolicy(cfg, nil)
	if err != nil {
		return err
	}
	return policy.Allowed(path)
}

// DeniedBy reports whether one of denyPaths covers path, matched on the
// lexical and canonical routes ReadMasked matches, but without the credential
// deny list. Callers that must tell a denial someone made (an operator's
// denyPaths, a context's private roots) apart from the default credential
// masks use it: an explicit grant at or inside a credential mask is the
// operator's choice. A path that is not absolute counts as denied.
func DeniedBy(denyPaths []string, path string) bool {
	path = filepath.Clean(expandTilde(path))
	if !filepath.IsAbs(path) {
		return true
	}
	var paths []string
	for _, denied := range denyPaths {
		if denied != "" {
			paths = append(paths, denied)
		}
	}
	if len(paths) == 0 {
		return false
	}
	routes := compileRoutes(policyRoutes(paths...))
	for _, query := range newRouteQueries(path) {
		if routes.deepestContaining(query) >= 0 {
			return true
		}
	}
	return false
}

// ReadPolicy is a read policy compiled from one Config for a bounded batch of
// queries. Route tables, route identities, the private roots and the frozen
// authority identities are captured at compile time; each query then costs
// one resolution of the queried path. Compile one at the start of a loop and
// discard it afterwards: never cache a ReadPolicy across captures or configs,
// since a route created or replaced after compilation is judged by its
// compile-time identity. A frozen grant replaced after compilation still
// fails every query closed. The zero value denies everything.
type ReadPolicy struct {
	compiled             bool
	authority            []authorityPathIdentity
	roots, grants, masks compiledRoutes
}

// CompileReadPolicy compiles cfg's read policy, including the platform's
// private roots, for repeated Allowed queries. It fails when a frozen grant
// of a prepared config has been rerouted or replaced since preparation.
func CompileReadPolicy(cfg Config) (ReadPolicy, error) {
	return compileReadPolicy(cfg, policyPrivateRoots())
}

func compileReadPolicy(cfg Config, privateRoots []string) (ReadPolicy, error) {
	if err := validateAuthorityPathIdentities(cfg.authorityPaths); err != nil {
		return ReadPolicy{}, err
	}
	return ReadPolicy{
		compiled:  true,
		authority: cfg.authorityPaths,
		roots:     compileRoutes(policyRoutes(privateRoots...)),
		grants:    compileRoutes(readGrantRoutes(cfg, privateRoots)),
		masks:     compileRoutes(maskRoutes(cfg)),
	}, nil
}

// Allowed reports whether an in-process read of path is consistent with the
// compiled policy, with ReadAllowed's rules and messages.
func (p ReadPolicy) Allowed(path string) error {
	if !p.compiled {
		return fmt.Errorf("path %q is blocked: the sandbox read policy was not compiled", path)
	}
	if err := checkAuthorityPathsInPlace(p.authority); err != nil {
		return err
	}
	path = filepath.Clean(expandTilde(path))
	if !filepath.IsAbs(path) {
		return fmt.Errorf("path %q is not absolute", path)
	}
	for _, query := range newRouteQueries(path) {
		grant := p.grants.deepestContaining(query)
		if p.roots.deepestContaining(query) > grant {
			return fmt.Errorf("path %q is inside a private directory the sandbox policy does not grant", path)
		}
		if p.masks.deepestContaining(query) > grant {
			return fmt.Errorf("path %q is blocked from reads by the sandbox policy", path)
		}
	}
	return nil
}

// checkAuthorityPathsInPlace is the per-query form of
// validateAuthorityPathIdentities: one Stat per frozen authority path,
// compared by identity, so a grant replaced after compilation fails closed
// without resolving every component again.
func checkAuthorityPathsInPlace(identities []authorityPathIdentity) error {
	for _, identity := range identities {
		info, err := os.Stat(identity.path)
		if err != nil {
			return fmt.Errorf("inspect frozen sandbox authority path %q: %w", identity.path, err)
		}
		if !os.SameFile(identity.info, info) {
			return fmt.Errorf("frozen sandbox authority path %q was replaced", identity.path)
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

// routeQuery is one candidate spelling of a queried path together with the
// identities of its existing ancestors, computed once per query and compared
// against every route.
type routeQuery struct {
	path  string
	chain []os.FileInfo
	// alt is the canonical spelling's chain, carried by the lexical
	// candidate: pathWithinPolicy also judges a lexical spelling by the
	// ancestors of its resolved route.
	alt []os.FileInfo
}

// newRouteQueries is readPolicyCandidates with each candidate's ancestor
// identities attached.
func newRouteQueries(path string) []routeQuery {
	candidates := readPolicyCandidates(path)
	queries := make([]routeQuery, len(candidates))
	for i, candidate := range candidates {
		queries[i] = routeQuery{path: candidate, chain: ancestorIdentityChain(candidate)}
	}
	if len(queries) == 2 {
		queries[0].alt = queries[1].chain
	}
	return queries
}

// ancestorIdentityChain lists the identities of path and its existing
// ancestors from the deepest up, as existingAncestorHasIdentity walks them:
// components that do not exist, or that sit below a file, are skipped, and
// any other error ends the walk there.
func ancestorIdentityChain(path string) []os.FileInfo {
	var chain []os.FileInfo
	current := filepath.Clean(path)
	for {
		info, err := os.Stat(current)
		if err == nil {
			chain = append(chain, info)
		} else if !os.IsNotExist(err) && !errors.Is(err, syscall.ENOTDIR) {
			return chain
		}
		parent := filepath.Dir(current)
		if parent == current {
			return chain
		}
		current = parent
	}
}

// compiledRoute is one spelling of a policy path together with the depth of
// its canonical route and, when it exists, its filesystem identity.
// Containment is judged per spelling (lexically and by filesystem identity)
// while depth compares canonical routes only, so an alias one segment shorter
// than the real path (/var against /private/var) never wins or loses a tie by
// its spelling. A route whose Stat fails matches lexically only, as a route
// pathWithinPolicy cannot stat does.
type compiledRoute struct {
	path  string
	depth int
	info  os.FileInfo
}

type compiledRoutes []compiledRoute

// compileRoutes attaches each route's identity, captured once here.
func compileRoutes(routes []policyRoute) compiledRoutes {
	out := make(compiledRoutes, len(routes))
	for i, route := range routes {
		out[i] = compiledRoute{path: route.path, depth: route.depth}
		if info, err := os.Stat(route.path); err == nil {
			out[i].info = info
		}
	}
	return out
}

// contains is pathWithinPolicy(query.path, r.path) with the route's identity
// and the query's ancestor walks hoisted out of the per-route loop.
func (r compiledRoute) contains(q routeQuery) bool {
	if PathWithin(q.path, r.path) {
		return true
	}
	if r.info == nil {
		return false
	}
	return sameFileAny(r.info, q.chain) || sameFileAny(r.info, q.alt)
}

func sameFileAny(info os.FileInfo, chain []os.FileInfo) bool {
	for _, candidate := range chain {
		if os.SameFile(info, candidate) {
			return true
		}
	}
	return false
}

// deepestContaining returns the canonical depth of the deepest route
// containing the query, or -1 when none does.
func (routes compiledRoutes) deepestContaining(q routeQuery) int {
	deepest := -1
	for _, route := range routes {
		if route.depth > deepest && route.contains(q) {
			deepest = route.depth
		}
	}
	return deepest
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
// canonical route; compileRoutes attaches its identity.
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
