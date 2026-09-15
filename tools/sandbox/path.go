package sandbox

import (
	"cmp"
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

// pathEqualsAny reports whether path lexically equals one of roots.
func pathEqualsAny(path string, roots []string) bool {
	path = filepath.Clean(path)
	for _, root := range roots {
		if path == filepath.Clean(root) {
			return true
		}
	}
	return false
}

// pathDepth counts the separators in the cleaned path, so a parent always
// sorts before its descendants.
func pathDepth(path string) int {
	return strings.Count(filepath.Clean(path), string(filepath.Separator))
}

// comparePathDepth orders shallower paths first and equal depths lexically,
// so parent mounts are installed before their descendants deterministically.
func comparePathDepth(a, b string) int {
	if c := cmp.Compare(pathDepth(a), pathDepth(b)); c != 0 {
		return c
	}
	return strings.Compare(a, b)
}

// minimizeGrants is minimizePaths for grants: a grant covered by another is
// kept when a boundary lies strictly inside the covering grant and at or
// above the covered one, since the covered grant is what re-opens the tree
// the boundary hides.
func minimizeGrants(grants, boundaries []string, nonCovering map[string]bool) []string {
	kept := make([]string, 0, len(grants))
	for _, path := range grants {
		covered := false
		for _, parent := range grants {
			if parent == path || nonCovering[parent] || !PathWithin(path, parent) {
				continue
			}
			if boundaryBetween(path, parent, boundaries) {
				continue
			}
			covered = true
			break
		}
		if !covered {
			kept = append(kept, path)
		}
	}
	return kept
}

// boundaryBetween reports whether a boundary lies strictly inside parent and
// at or above path. A boundary equal to parent ties with it, which a grant
// wins for reads, so it hides nothing the inner grant would have to re-open.
func boundaryBetween(path, parent string, boundaries []string) bool {
	for _, boundary := range boundaries {
		if boundary != parent && PathWithin(boundary, parent) && PathWithin(path, boundary) {
			return true
		}
	}
	return false
}
