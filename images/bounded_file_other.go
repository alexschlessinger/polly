//go:build plan9 || js || wasip1

package images

import "os"

func openFileForBoundedRead(path string) (*os.File, error) {
	return os.Open(path)
}
