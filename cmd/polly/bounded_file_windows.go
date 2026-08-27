//go:build windows

package main

import "os"

func openFileForBoundedRead(path string) (*os.File, error) {
	// Windows has no os.OpenFile flag equivalent to O_NONBLOCK for every file
	// kind. The descriptor is still verified immediately after opening.
	return os.Open(path)
}
