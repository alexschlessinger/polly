// Package scratch removes runtime-owned temporary directories.
package scratch

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// RemoveAll removes a scratch directory, repairing owner permissions when
// read-only directories (such as Go's module cache) prevent ordinary removal.
// The caller must establish ownership and stop users of the scratch first.
func RemoveAll(path string) error {
	err := os.RemoveAll(path)
	if !errors.Is(err, fs.ErrPermission) {
		return err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return os.Remove(path)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return err
	}
	defer root.Close()
	opened, err := root.Stat(".")
	if err != nil {
		return err
	}
	if !os.SameFile(info, opened) {
		return fmt.Errorf("scratch changed during cleanup: %s", path)
	}
	// WalkDir does not descend through symlinks. Root also confines chmod
	// if an entry is replaced during the walk; external targets stay untouched.
	err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode().Perm()&0700 != 0700 {
			return root.Chmod(name, info.Mode().Perm()|0700)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("prepare scratch %s for removal: %w", path, err)
	}
	return os.RemoveAll(path)
}
