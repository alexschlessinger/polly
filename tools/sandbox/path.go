package sandbox

import (
	"path/filepath"
	"strings"
)

// PathWithin reports whether path lexically equals parent or lies beneath it.
// Both arguments must already be clean absolute paths; no symlinks are
// resolved. A relative result that is itself absolute (a different volume on
// Windows) is outside.
func PathWithin(path, parent string) bool {
	rel, err := filepath.Rel(parent, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// isWithinAny reports whether path lexically equals or lies beneath any root.
func isWithinAny(path string, roots []string) bool {
	for _, root := range roots {
		if PathWithin(path, root) {
			return true
		}
	}
	return false
}

// minimizePaths drops every path lexically covered by another entry, keeping
// input order. Entries in nonCovering may be kept but never cover others.
func minimizePaths(paths []string, nonCovering map[string]bool) []string {
	kept := make([]string, 0, len(paths))
	for _, path := range paths {
		covered := false
		for _, parent := range paths {
			if parent != path && !nonCovering[parent] && PathWithin(path, parent) {
				covered = true
				break
			}
		}
		if !covered {
			kept = append(kept, path)
		}
	}
	return kept
}
