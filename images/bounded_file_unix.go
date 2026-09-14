//go:build unix

package images

import (
	"os"
	"syscall"
)

func openFileForBoundedRead(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
