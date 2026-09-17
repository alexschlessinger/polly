package worktree

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

func snapshotPrivatePaths(root string, paths []string) ([]string, error) {
	var excluded []string
	seen := map[string]bool{}
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			path = filepath.Join(root, path)
		}
		routes := []string{filepath.Clean(path)}
		if resolved, err := sandbox.ResolveExistingPathPrefix(path); err == nil {
			routes = append(routes, resolved)
		}
		for _, route := range routes {
			if sandbox.PathWithin(root, route) {
				return nil, fmt.Errorf("snapshot source is inside private path %s", path)
			}
			if !sandbox.PathWithin(route, root) {
				continue
			}
			rel, err := filepath.Rel(root, route)
			if err != nil {
				return nil, err
			}
			rel = filepath.ToSlash(rel)
			if !seen[rel] {
				seen[rel] = true
				excluded = append(excluded, rel)
			}
		}
	}
	return excluded, nil
}

// privatePath reports whether the repository-relative name is one of the
// private paths or inside one.
func privatePath(private []string, name string) bool {
	for _, path := range private {
		if name == path || strings.HasPrefix(name, path+"/") {
			return true
		}
	}
	return false
}

func (m *Manager) privateSourcePath(name string) bool { return privatePath(m.privatePaths, name) }
