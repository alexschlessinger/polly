package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteAllowed reports whether an in-process write of path is consistent with
// the write policy this config applies to wrapped commands: writes are allowed
// only under the OS temp directories and cfg.WritablePaths, excluding the
// cfg.DenyWritePaths islands and the credential deny list (which ReadPaths
// exempts from reads but deliberately never from writes), and DenyWrite denies
// everything. Tools that write files directly (rather than through a wrapped
// process) use this so they cannot change what a sandboxed command could not.
// The check is best-effort against symlinks — the lexical route and its
// resolved route are both tested, and a target that does not exist yet is
// resolved through its deepest existing ancestor — matching the masking the OS
// backends apply.
func WriteAllowed(cfg Config, path string) error {
	if cfg.DenyWrite {
		return fmt.Errorf("path %q is blocked: the sandbox policy denies all file writes", path)
	}
	path = filepath.Clean(expandTilde(path))
	if !filepath.IsAbs(path) {
		return fmt.Errorf("path %q is not absolute", path)
	}
	candidates := []string{path}
	if resolved, err := resolveExistingPathPrefix(path); err == nil {
		if resolved = filepath.Clean(resolved); resolved != path {
			candidates = append(candidates, resolved)
		}
	}
	denies := writePolicyRoutes(cfg.DenyWritePaths...)
	for _, denied := range allDeniedPaths(cfg) {
		denies = append(denies, writePolicyRoutes(denied.Path)...)
	}
	for _, deny := range denies {
		for _, candidate := range candidates {
			if pathWithinPolicy(candidate, deny) {
				return fmt.Errorf("path %q is blocked from writes by the sandbox policy", path)
			}
		}
	}
	// The write lands on the resolved route, so containment is judged there;
	// requiring the lexical route too would reject writable grants the OS
	// backends honor when reached through a symlinked spelling.
	target := candidates[len(candidates)-1]
	for _, root := range writableRootRoutes(cfg) {
		if pathWithinPolicy(target, root) {
			return nil
		}
	}
	return fmt.Errorf("path %q is outside the sandbox policy's writable paths", path)
}

// writableRootRoutes mirrors the write grants the OS backends give wrapped
// commands: the OS temp directories plus cfg.WritablePaths, each in lexical
// and resolved form.
func writableRootRoutes(cfg Config) []string {
	roots := []string{"/tmp", os.TempDir()}
	for _, p := range cfg.WritablePaths {
		roots = append(roots, p)
	}
	return writePolicyRoutes(roots...)
}

// writePolicyRoutes expands each policy path to its lexical and, when it
// resolves differently, symlink-resolved spellings, deduplicated in order.
func writePolicyRoutes(paths ...string) []string {
	var out []string
	seen := make(map[string]bool, len(paths))
	add := func(route string) {
		if !seen[route] {
			seen[route] = true
			out = append(out, route)
		}
	}
	for _, path := range paths {
		path = filepath.Clean(expandTilde(path))
		add(path)
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			add(filepath.Clean(resolved))
		}
	}
	return out
}
