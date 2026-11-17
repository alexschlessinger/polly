package sessions

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// lockLogFile takes the exclusive lock on an owner's lock file without
// waiting, and reports whether it got it. The lock goes when the file is
// closed or the process ends.
func lockLogFile(f *os.File) (bool, error) {
	var overlapped windows.Overlapped
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
		return false, nil
	}
	return err == nil, err
}
