package skills

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// builtinFS holds the skills that ship inside the binary. They are synced to
// BuiltinDir at startup so discovery, activation, and sandbox read grants all
// work exactly as they do for user-installed skills.
//
//go:embed all:builtin
var builtinFS embed.FS

// builtinSkills is the embedded tree rooted at the skill directories
// themselves, so a path inside it is the same path under BuiltinDir.
var builtinSkills = func() fs.FS {
	sub, err := fs.Sub(builtinFS, "builtin")
	if err != nil {
		panic(err)
	}
	return sub
}()

// BuiltinDir returns the directory builtin skills are materialized into
// (~/.pollytool/builtin-skills). It is polly-managed: anything not in the
// embedded tree is removed on sync.
func BuiltinDir() (string, error) {
	return pollytoolDir("builtin-skills")
}

// MaterializeBuiltin syncs the embedded builtin skills into BuiltinDir —
// writing new and changed files and removing stale ones — and returns the
// directory path.
func MaterializeBuiltin() (string, error) {
	dir, err := BuiltinDir()
	if err != nil {
		return "", err
	}
	if err := fs.WalkDir(builtinSkills, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(dir, filepath.FromSlash(path))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := fs.ReadFile(builtinSkills, path)
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
		if _, err := fs.Stat(builtinSkills, filepath.ToSlash(rel)); err == nil {
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
	return LoadCatalog([]string{dir})
}
