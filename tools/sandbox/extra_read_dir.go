package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ValidateExtraReadDir canonicalizes one candidate extra read directory
// (the --add-dir / /add-dir surface) against the workspace it would be
// added to and returns the canonical real path. The path must exist and be
// a directory; relative paths resolve against the process working
// directory and a leading ~ expands to the home directory. Symlinks resolve
// to the real path, the same treatment the workspace itself gets. Rejected
// with distinct errors are: the empty path; paths that do not exist or are
// not directories; the filesystem root; the home directory or any ancestor
// of it (granting home unmasks every private path, including the
// credential masks under it); paths inside the workspace (already
// readable, so the grant is redundant); and paths equal to or inside the
// OS temp-family roots. An ancestor of the workspace is accepted: that is
// the multi-repo case, documented as also exposing the workspace's
// siblings. Credential paths are not rejected: a directory that contains
// one keeps it masked (the deeper mask wins), and a directory at or inside
// one is the operator's explicit choice to expose it, which callers name
// through ExposedCredentials. workspace is resolved like the candidate but
// does not have to exist.
func ValidateExtraReadDir(workspace, path string) (string, error) {
	return validateExtraReadDir(workspace, path, true)
}

// CanonicalizeExtraReadDir is the lenient form of ValidateExtraReadDir for
// re-validating a persisted entry on resume. The canonicalization and the
// root and home rejections are the same, but existence is not
// required: a directory deleted between sessions stays on the session
// record, and the sandbox's own construction freeze (PrepareConfig drops
// missing read grants) keeps it unreadable until it is recreated and the
// session resumed again.
func CanonicalizeExtraReadDir(path string) (string, error) {
	return validateExtraReadDir("", path, false)
}

func validateExtraReadDir(workspace, path string, requireExists bool) (string, error) {
	canonical, err := canonicalizeExtraReadDir(path, requireExists)
	if err != nil {
		return "", err
	}
	if requireExists {
		info, err := os.Stat(canonical)
		if err != nil {
			return "", fmt.Errorf("inspect extra read directory %q: %w", canonical, err)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("extra read directory %q is not a directory", canonical)
		}
	}
	if filepath.Dir(canonical) == canonical {
		return "", fmt.Errorf("extra read directory %q is the filesystem root", canonical)
	}
	if home, err := os.UserHomeDir(); err == nil {
		if home := canonicalExtraReadDirPath(home); PathWithin(home, canonical) {
			return "", fmt.Errorf("extra read directory %q is the home directory or an ancestor of it", canonical)
		}
	}
	if requireExists {
		if resolved := canonicalExtraReadDirWorkspace(workspace); resolved != "" && PathWithin(canonical, resolved) {
			return "", fmt.Errorf("extra read directory %q is inside the workspace %q, which is already readable", canonical, resolved)
		}
	}
	if requireExists {
		for _, root := range extraReadDirTempRoots() {
			if PathWithin(canonical, root) {
				return "", fmt.Errorf("extra read directory %q is inside the OS temp directory %q", canonical, root)
			}
		}
	}
	return canonical, nil
}

// canonicalizeExtraReadDir expands ~, makes the path absolute against the
// process working directory, and resolves symlinks. The lenient form
// (requireExists false) keeps the spelling a recreated path would resolve
// to, so a persisted entry survives deletion; the strict form fails with
// the existence error itself.
func canonicalizeExtraReadDir(path string, requireExists bool) (string, error) {
	if path == "" {
		return "", errors.New("extra read directory path is empty")
	}
	path = filepath.Clean(expandTilde(path))
	if !filepath.IsAbs(path) {
		abs, err := filepath.Abs(path)
		if err != nil {
			return "", fmt.Errorf("resolve extra read directory %q: %w", path, err)
		}
		path = abs
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return filepath.Clean(resolved), nil
	}
	if !requireExists {
		if prefix, prefixErr := resolveExistingPathPrefix(path); prefixErr == nil {
			return filepath.Clean(prefix), nil
		}
		return "", fmt.Errorf("resolve extra read directory %q: %w", path, err)
	}
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("extra read directory %q does not exist", path)
	}
	if errors.Is(err, syscall.ENOTDIR) {
		return "", fmt.Errorf("extra read directory %q is not a directory", path)
	}
	return "", fmt.Errorf("resolve extra read directory %q: %w", path, err)
}

// canonicalExtraReadDirWorkspace resolves the workspace the same way the
// candidate is resolved — ~ expanded, absolute, symlinks in the existing
// prefix resolved — without requiring it to exist.
func canonicalExtraReadDirWorkspace(workspace string) string {
	if workspace == "" {
		return ""
	}
	return canonicalExtraReadDirPath(canonicalizeExtraReadDirRelative(workspace))
}

// canonicalizeExtraReadDirRelative expands ~ and makes the path absolute
// against the process working directory, keeping the spelling on failure.
func canonicalizeExtraReadDirRelative(path string) string {
	path = filepath.Clean(expandTilde(path))
	if !filepath.IsAbs(path) {
		if abs, err := filepath.Abs(path); err == nil {
			return abs
		}
	}
	return path
}

// canonicalExtraReadDirPath cleans a path and resolves its symlinks,
// resolving only the existing prefix so a path that does not exist yet
// keeps the spelling a recreated path would resolve to.
func canonicalExtraReadDirPath(path string) string {
	path = filepath.Clean(expandTilde(path))
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(resolved)
	}
	if resolved, err := resolveExistingPathPrefix(path); err == nil {
		return resolved
	}
	return path
}

// extraReadDirTempRoots lists the canonical OS temp-family roots an extra
// read directory may not sit in: the configured temp directory and /tmp,
// which coincide on some platforms.
func extraReadDirTempRoots() []string {
	roots := make([]string, 0, 2)
	seen := make(map[string]bool, 2)
	for _, root := range []string{os.TempDir(), "/tmp"} {
		if real, err := filepath.EvalSymlinks(root); err == nil {
			root = real
		}
		root = filepath.Clean(root)
		if root != "" && !seen[root] {
			seen[root] = true
			roots = append(roots, root)
		}
	}
	return roots
}

// MergeExtraReadDirs appends added to existing in order, dropping exact
// duplicates and entries subsumed by an entry that is already present: an
// existing ancestor covers a new descendant. A new ancestor does not
// remove the descendants already listed; both are kept, and repeated adds
// converge. Entries must be canonical absolute paths as returned by
// ValidateExtraReadDir or CanonicalizeExtraReadDir. The result is a fresh
// slice, nil when nothing remains.
func MergeExtraReadDirs(existing, added []string) []string {
	merged := make([]string, 0, len(existing)+len(added))
	subsumed := func(path string) bool {
		for _, kept := range merged {
			if PathWithin(path, kept) {
				return true
			}
		}
		return false
	}
	for _, path := range existing {
		if path == "" {
			continue
		}
		path = filepath.Clean(path)
		if !subsumed(path) {
			merged = append(merged, path)
		}
	}
	for _, path := range added {
		if path == "" {
			continue
		}
		path = filepath.Clean(path)
		if !subsumed(path) {
			merged = append(merged, path)
		}
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}
