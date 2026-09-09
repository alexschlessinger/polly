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

func (m *Manager) privateSourcePath(name string) bool {
	for _, path := range m.privatePaths {
		if name == path || strings.HasPrefix(name, path+"/") {
			return true
		}
	}
	return false
}
