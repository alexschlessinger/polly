package main

import (
	"os"

	"github.com/alexschlessinger/pollytool/images"
)

func openBoundedRegularFile(path string, maxBytes int64) (*os.File, error) {
	return images.OpenBoundedFile(path, maxBytes)
}

func readBoundedRegularFile(path string, maxBytes int64) ([]byte, error) {
	return images.ReadBoundedFile(path, maxBytes)
}
