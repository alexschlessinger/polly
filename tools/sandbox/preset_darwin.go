//go:build darwin

package sandbox

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func rejectPlatformBroadWorkspace(dir string) error {
	var stat unix.Statfs_t
	if err := unix.Statfs(dir, &stat); err != nil {
		return fmt.Errorf("inspect Darwin mount for workspace %q: %w", dir, err)
	}
	mountPoint := filepath.Clean(unix.ByteSliceToString(stat.Mntonname[:]))
	if mountPoint == "." || !filepath.IsAbs(mountPoint) {
		return fmt.Errorf("inspect Darwin mount for workspace %q: invalid mount point %q", dir, mountPoint)
	}
	if filepath.Clean(dir) == mountPoint {
		return broadWorkspaceError(dir, "mounted volume root")
	}
	dirInfo, dirErr := os.Stat(dir)
	mountInfo, mountErr := os.Stat(mountPoint)
	if dirErr != nil {
		return fmt.Errorf("inspect Darwin workspace %q: %w", dir, dirErr)
	}
	if mountErr != nil {
		return fmt.Errorf("inspect Darwin mount point %q for workspace %q: %w", mountPoint, dir, mountErr)
	}
	if os.SameFile(dirInfo, mountInfo) {
		return broadWorkspaceError(dir, "mounted volume root")
	}
	return nil
}

// Seatbelt writable path rules always refer to the host filesystem, including
// the default temp grants, so every final configured root is relevant to the
// Git persistence audit.
func gitHostWritablePath(string) bool {
	return true
}
