package sandbox

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// GitCommonDir returns the canonical common Git directory of the repository
// whose working tree holds dir, found by walking up from dir without running
// Git: the .git directory of an ordinary checkout, the directory a .git file
// routes to for a submodule, and the main repository's for a linked
// worktree, so every worktree of one repository shares it. It returns ""
// without an error when no ancestor of dir has a .git entry.
func GitCommonDir(dir string) (string, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	for path := filepath.Clean(dir); ; path = filepath.Dir(path) {
		entry := filepath.Join(path, ".git")
		info, err := os.Stat(entry)
		if err == nil {
			return gitCommonDirOf(entry, info)
		}
		if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
			return "", fmt.Errorf("inspect %s: %w", entry, err)
		}
		if filepath.Dir(path) == path {
			return "", nil
		}
	}
}

// gitCommonDirOf follows a .git entry to its repository's common directory:
// through the "gitdir:" pointer of a .git file, then through the commondir
// pointer a linked worktree's Git directory holds.
func gitCommonDirOf(entry string, info fs.FileInfo) (string, error) {
	gitDir := entry
	switch {
	case info.IsDir():
	case info.Mode().IsRegular():
		target, err := readGitPointer(entry, "gitdir:")
		if err != nil {
			return "", fmt.Errorf("read Git routing entry %s: %w", entry, err)
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(entry), target)
		}
		gitDir = filepath.Clean(target)
	default:
		return "", fmt.Errorf("Git routing entry %s is neither a directory nor a regular file", entry)
	}
	common := gitDir
	pointer, err := readGitPointer(filepath.Join(gitDir, "commondir"), "")
	switch {
	case err == nil:
		if !filepath.IsAbs(pointer) {
			pointer = filepath.Join(gitDir, pointer)
		}
		common = filepath.Clean(pointer)
	case !errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("read the commondir pointer of %s: %w", gitDir, err)
	}
	real, err := filepath.EvalSymlinks(common)
	if err != nil {
		return "", fmt.Errorf("resolve Git directory %s: %w", common, err)
	}
	return filepath.Clean(real), nil
}
