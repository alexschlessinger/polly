package scratch

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// ReleaseOwned removes a recorded scratch only if its name and protected
// ownership record identify slot. The recorded root may predate Root's current
// location. This is cleanup authority only, never a sandbox grant. Callers must
// still prove ownership of slot and stop its users before releasing it.
func ReleaseOwned(dir, slot string) error {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir || !filepath.IsAbs(slot) || filepath.Clean(slot) != slot || filepath.Base(dir) != filepath.Base(DirFor(slot)) {
		return fmt.Errorf("not scratch for slot %s: %s", slot, dir)
	}
	root, err := openExclusiveRoot(filepath.Dir(dir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	name := filepath.Base(dir)
	owner, err := readOwner(root, name)
	if errors.Is(err, fs.ErrNotExist) {
		if _, statErr := root.Lstat(name); errors.Is(statErr, fs.ErrNotExist) {
			return nil // A release already removed both directory and record.
		}
	}
	if err != nil {
		return fmt.Errorf("scratch %s ownership: %w", dir, err)
	}
	if owner != slot {
		return fmt.Errorf("scratch %s belongs to %s, not %s", dir, owner, slot)
	}
	return releaseAt(root, name)
}

// ReleaseSlot removes slot's scratches in the known private roots and at an
// optional retained location. Discovery never scans arbitrary directories;
// every discovered scratch must have an ownership record matching slot. A
// nested runtime may not access the host roots: only its active and explicitly
// retained paths are obligations when no other root's record can be inspected.
func ReleaseSlot(slot, retained string) error {
	dirs := map[string]bool{DirFor(slot): true}
	if retained != "" {
		dirs[retained] = true
	}
	for _, root := range PrivateRoots() {
		dir := filepath.Join(root, filepath.Base(DirFor(slot)))
		if _, err := os.Lstat(dir + ownerSuffix); err == nil {
			dirs[dir] = true
		}
	}
	var errs []error
	for dir := range dirs {
		errs = append(errs, ReleaseOwned(dir, slot))
	}
	return errors.Join(errs...)
}

func openExclusiveRoot(path string) (*os.Root, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("scratch root %s is not a directory", path)
	}
	if err := exclusiveToCurrentUser(info); err != nil {
		return nil, fmt.Errorf("scratch root %s: %w", path, err)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		root.Close()
		return nil, fmt.Errorf("scratch root changed during cleanup: %s", path)
	}
	return root, nil
}

func readOwner(root *os.Root, name string) (string, error) {
	info, err := root.Lstat(name + ownerSuffix)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("scratch owner record is not a regular file")
	}
	if err := exclusiveToCurrentUser(info); err != nil {
		return "", err
	}
	file, err := root.Open(name + ownerSuffix)
	if err != nil {
		return "", err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return "", errors.New("scratch owner record changed during cleanup")
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return "", err
	}
	owner := string(data)
	if !filepath.IsAbs(owner) || filepath.Clean(owner) != owner {
		return "", errors.New("scratch owner record is not an absolute slot path")
	}
	return owner, nil
}

func releaseAt(root *os.Root, name string) error {
	if err := removeAllAt(root, name); err != nil {
		return err
	}
	if err := root.Remove(name + ownerSuffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
