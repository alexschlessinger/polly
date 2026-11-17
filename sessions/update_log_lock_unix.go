//go:build unix

package sessions

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// lockLogFile takes the exclusive lock on an owner's lock file without
// waiting, and reports whether it got it. The lock goes when the file is
// closed or the process ends.
func lockLogFile(f *os.File) (bool, error) {
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, unix.EINTR):
		case errors.Is(err, unix.EWOULDBLOCK):
			return false, nil
		default:
			return false, err
		}
	}
}
