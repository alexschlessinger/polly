package sandbox

import (
	"fmt"
	"path/filepath"
)

// ReadAllowed reports whether an in-process read of path is consistent with
// the read policy this config applies to wrapped commands: reads are allowed
// except under the built-in DeniedPaths and cfg.DenyPaths, with ReadPaths
// exemptions. Tools that read files directly (rather than through a wrapped
// process) use this so they cannot see paths a sandboxed command could not.
// The check is best-effort against symlinks — both the lexical path and its
// resolved route are tested — matching the masking the OS backends apply.
func ReadAllowed(cfg Config, path string) error {
	path = filepath.Clean(expandTilde(path))
	candidates := []string{path}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		if resolved = filepath.Clean(resolved); resolved != path {
			candidates = append(candidates, resolved)
		}
	}
	for _, deny := range allDeniedPaths(cfg) {
		for _, candidate := range candidates {
			if !pathWithinPolicy(candidate, deny.Path) {
				continue
			}
			exempt := false
			for _, readPath := range cfg.ReadPaths {
				if pathWithinPolicy(candidate, readPath) {
					exempt = true
					break
				}
			}
			if !exempt {
				return fmt.Errorf("path %q is blocked from reads by the sandbox policy", path)
			}
		}
	}
	return nil
}
