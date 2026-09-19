//go:build unix

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// ownedByUserAlone refuses a profile file or directory another user owns or
// may write, which could hand this user's sandbox grants it never chose.
func ownedByUserAlone(info fs.FileInfo) error {
	if perm := info.Mode().Perm(); perm&0o022 != 0 {
		return fmt.Errorf("is writable by other users (mode %04o)", perm)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("has an owner that could not be determined")
	}
	if int(stat.Uid) != os.Getuid() {
		return errors.New("belongs to another user")
	}
	return nil
}
