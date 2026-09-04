// Package safefile opens local files so that a policy decision made on a path
// cannot be redirected by a symlink swapped into that path afterwards. Callers
// resolve and approve a spelling of the path first; OpenRegular then
// guarantees the returned descriptor is the object at exactly that spelling,
// refusing symlinks in every component and refusing special files that could
// block the caller.
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
