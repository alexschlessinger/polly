// Package safefile opens local files and verifies that the object opened is a
// regular file, refusing special files that could block the caller.
// OpenRegular additionally pins a policy decision made on a path: callers
// resolve and approve a spelling first, and the returned descriptor is the
// object at exactly that spelling, with symlinks refused in every component.
// OpenRegularFollow is the form for paths the user or Polly chose directly,
// where symlinks are followed as os.Open would.
package safefile

import (
	"fmt"
	"os"
)

// NotRegularError reports that the object at Path is not a regular file.
type NotRegularError struct {
	Path string
	Mode os.FileMode
}

func (e *NotRegularError) Error() string {
	switch {
	case e.Mode.IsDir():
		return e.Path + " is a directory, not a file"
	case e.Mode&os.ModeSymlink != 0:
		return e.Path + " is a symbolic link, not a regular file"
	case e.Mode&os.ModeNamedPipe != 0:
		return e.Path + " is a named pipe, not a regular file"
	case e.Mode&os.ModeSocket != 0:
		return e.Path + " is a socket, not a regular file"
	case e.Mode&os.ModeDevice != 0:
		return e.Path + " is a device, not a regular file"
	default:
		return fmt.Sprintf("%s is not a regular file (mode %s)", e.Path, e.Mode)
	}
}

// OpenRegular opens the absolute path with flag and perm as for os.OpenFile,
// without following a symbolic link in any path component, and verifies that
// the opened object is a regular file. A path that legitimately routes through
// a symlink must be resolved by the caller (after checking policy on the
// resolved spelling) before it is opened here. FIFOs and devices are rejected
// without blocking on them.
func OpenRegular(path string, flag int, perm os.FileMode) (*os.File, error) {
	return openRegular(path, flag, perm)
}

// OpenDirectory opens the absolute path for reading its entries, without
// following a symbolic link in any path component, so the directory listed
// is the one at exactly that spelling. As for OpenRegular, a path that
// legitimately routes through a symlink must be resolved by the caller
// first.
func OpenDirectory(path string) (*os.File, error) {
	return openDirectory(path)
}

// OpenRegularFollow opens path read-only as os.Open would, following symbolic
// links, and verifies that the opened object is a regular file without
// blocking on a FIFO or device. It serves paths the user or Polly chose
// directly, where no policy decision needs pinning; a policy-checked route
// opens through OpenRegular.
func OpenRegularFollow(path string) (*os.File, error) {
	return openRegularFollow(path)
}
