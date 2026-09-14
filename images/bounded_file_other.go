//go:build !unix

package images

import "os"

func openFileForBoundedRead(path string) (*os.File, error) {
	// No portable O_NONBLOCK equivalent here. The descriptor is still
	// verified immediately after opening.
	return os.Open(path)
}
