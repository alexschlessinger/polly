package main

import (
	"os"
	"path/filepath"

	"github.com/alexschlessinger/pollytool/sessions"
)

// Determine storage exclusions before building a registry: stdio MCP and
// shell schema loading can start processes before the swarm is registered.
// Include the disk-promotion destination even for a currently in-memory store.
func sessionPrivatePaths(store sessions.SessionStore) ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	databases := []string{filepath.Join(home, ".pollytool", "polly.db")}
	if durable, ok := store.(sessions.DurableStore); ok {
		mode, path := durable.Location()
		if mode == sessions.ModeDisk && path != "" {
			databases = append(databases, path)
		}
	}
	paths := []string{}
	seen := map[string]bool{}
	for _, database := range databases {
		absolute, err := filepath.Abs(database)
		if err != nil {
			return nil, err
		}
		for _, suffix := range []string{"", "-wal", "-shm"} {
			canonical, err := canonicalStoragePath(absolute + suffix)
			if err != nil {
				return nil, err
			}
			for _, path := range []string{absolute + suffix, canonical} {
				if !seen[path] {
					paths = append(paths, path)
					seen[path] = true
				}
			}
		}
	}
	return paths, nil
}

// Sidecars and a future promotion directory may not exist yet. Freeze their
// existing ancestor's route so the database cannot be reached via its real path
// when the configured storage path used a symlink.
func canonicalStoragePath(path string) (string, error) {
	real, err := filepath.EvalSymlinks(path)
	if err == nil {
		return real, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return "", err
	}
	real, err = canonicalStoragePath(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(real, filepath.Base(path)), nil
}
