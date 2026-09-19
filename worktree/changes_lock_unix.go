//go:build unix

package worktree

import (
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// Observers share a lease; pruning requires the exclusive lease. Closing the
// descriptor releases it, including when a process exits unexpectedly.
func lockChangeStore(path string, exclusive bool) (io.Closer, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	mode := unix.LOCK_SH | unix.LOCK_NB
	if exclusive {
		mode = unix.LOCK_EX | unix.LOCK_NB
	}
	if err = unix.Flock(int(f.Fd()), mode); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}
