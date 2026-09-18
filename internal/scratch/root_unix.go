//go:build unix

package scratch

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// exclusiveToCurrentUser refuses a scratch root that other users can reach
// or that another user planted in the shared temp area before this process
// could create it.
func exclusiveToCurrentUser(info fs.FileInfo) error {
	if perm := info.Mode().Perm(); perm&0077 != 0 {
		return fmt.Errorf("is reachable by other users (mode %04o)", perm)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("owner could not be determined")
	}
	if int(stat.Uid) != os.Getuid() {
		return errors.New("belongs to another user")
	}
	return nil
}
