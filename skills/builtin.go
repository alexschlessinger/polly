package skills

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// builtinFS holds the skills that ship inside the binary. They are synced to
// BuiltinDir at startup so discovery, activation, and sandbox read grants all
// work exactly as they do for user-installed skills.
//
//go:embed all:builtin
var builtinFS embed.FS

const builtinRoot = "builtin"

// BuiltinDir returns the directory builtin skills are materialized into
// (~/.pollytool/builtin-skills). It is polly-managed: anything not in the
// embedded tree is removed on sync.
func BuiltinDir() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(homeDir, ".pollytool", "builtin-skills"), nil
}

// MaterializeBuiltin syncs the embedded builtin skills into BuiltinDir —
// writing new and changed files and removing stale ones — and returns the
// directory path.
func MaterializeBuiltin() (string, error) {
	dir, err := BuiltinDir()
	if err != nil {
		return "", err
	}
	if err := fs.WalkDir(builtinFS, builtinRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(builtinRoot, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dir, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := builtinFS.ReadFile(path)
		if err != nil {
			return err
		}
		if existing, err := os.ReadFile(target); err == nil && bytes.Equal(existing, data) {
			return nil
		}
		return os.WriteFile(target, data, 0o644)
	}); err != nil {
		return "", fmt.Errorf("materialize builtin skills: %w", err)
	}
	if err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil || rel == "." {
			return err
		}
		if _, err := fs.Stat(builtinFS, filepath.Join(builtinRoot, rel)); err == nil {
			return nil
		}
		if d.IsDir() {
			// Remove the stale subtree whole and do not descend into it.
			if err := os.RemoveAll(path); err != nil {
				return err
			}
			return filepath.SkipDir
		}
		return os.Remove(path)
	}); err != nil {
		return "", fmt.Errorf("prune builtin skills: %w", err)
	}
	return dir, nil
}

// LoadBuiltinCatalog materializes the builtin skills and discovers them.
func LoadBuiltinCatalog() (*Catalog, error) {
	dir, err := MaterializeBuiltin()
	if err != nil {
		return nil, err
	}
	catalog, err := Discover([]string{dir})
	if err != nil {
		return nil, err
	}
	if catalog.IsEmpty() {
		return nil, nil
	}
	return catalog, nil
}

// Merge adds every skill from other that is not already present. Existing
// skills win, so user-installed skills shadow builtin skills of the same
// name instead of tripping the duplicate-name error.
func (c *Catalog) Merge(other *Catalog) {
	if c == nil || other == nil {
		return
	}
	for _, skill := range other.ordered {
		if _, ok := c.byName[skill.Name]; ok {
			continue
		}
		c.byName[skill.Name] = skill
		c.ordered = append(c.ordered, skill)
	}
	sort.Slice(c.ordered, func(i, j int) bool {
		return c.ordered[i].Name < c.ordered[j].Name
	})
}
