package envstorage

import (
	"os"
	"path/filepath"
	"runtime"
)

// DataRoot and CacheRoot locate storage, not permission records. Canonicalize
// only the platform base: a replaced directory beneath it must be rejected.
func DataRoot() (string, error) {
	base := os.Getenv("XDG_DATA_HOME")
	if !filepath.IsAbs(base) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "share")
		if runtime.GOOS == "darwin" {
			base = filepath.Join(home, "Library", "Application Support")
		}
	}
	return filepath.Join(canonicalBase(base), "pollytool", "environments"), nil
}

func CacheRoot() (string, error) {
	base := os.Getenv("XDG_CACHE_HOME")
	if !filepath.IsAbs(base) {
		var err error
		base, err = os.UserCacheDir()
		if err != nil {
			return "", err
		}
	}
	return filepath.Join(canonicalBase(base), "pollytool", "ws"), nil
}

func canonicalBase(path string) string {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	parent := filepath.Dir(path)
	if parent == path {
		return path
	}
	return filepath.Join(canonicalBase(parent), filepath.Base(path))
}

// PrivateRoots also covers XDG locations outside the home directory.
func PrivateRoots() []string {
	var roots []string
	if p, err := DataRoot(); err == nil {
		roots = append(roots, p)
	}
	if p, err := CacheRoot(); err == nil {
		roots = append(roots, p)
	}
	return roots
}
