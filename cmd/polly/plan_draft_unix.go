//go:build unix

package main

import (
	"os"
	"path/filepath"
	"syscall"

	"github.com/alexschlessinger/pollytool/internal/safefile"
)

func openRegularDraft(root *os.Root, name string) (*os.File, error) {
	return safefile.OpenRegular(filepath.Join(root.Name(), name), os.O_RDONLY, 0)
}

func singleLinkDraft(_ *os.File, info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink == 1
}
