// Package scratch removes runtime-owned temporary directories.
package scratch

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// RemoveAll removes a scratch directory, repairing owner permissions when
// read-only directories (such as Go's module cache) prevent ordinary removal.
// The caller must establish ownership and stop users of the scratch first.
func RemoveAll(path string) error {
	if path == "" {
		return nil
	}
	parent, err := os.OpenRoot(filepath.Dir(path))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer parent.Close()
	return removeAllAt(parent, filepath.Base(path))
}

// removeAllAt keeps both removal attempts and permission repairs inside the
// opened parent, even if its original path is replaced during cleanup.
func removeAllAt(parent *os.Root, path string) error {
	err := parent.RemoveAll(path)
	if !errors.Is(err, fs.ErrPermission) {
		return err
	}
	info, err := parent.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return parent.Remove(path)
	}
	root, err := parent.OpenRoot(path)
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
	return parent.RemoveAll(path)
}
