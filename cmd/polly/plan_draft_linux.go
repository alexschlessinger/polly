package main

import (
	"os"

	"golang.org/x/sys/unix"
)

func renameDraftNoReplace(dir *os.File, from, to string) error {
	fd := int(dir.Fd())
	return unix.Renameat2(fd, from, fd, to, unix.RENAME_NOREPLACE)
}
