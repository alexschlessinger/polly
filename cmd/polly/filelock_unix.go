//go:build unix

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// lockFile takes an exclusive advisory lock on f, released by unlockFile.
func lockFile(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_EX)
}

func unlockFile(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_UN)
}
