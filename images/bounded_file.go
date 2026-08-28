package images

import (
	"fmt"
	"io"
	"os"
)

// OpenBoundedFile opens path without letting special files block on Unix,
// then verifies the opened descriptor rather than trusting an earlier path
// lookup. The size is checked both before and during reads so concurrent
// growth cannot bypass maxBytes.
func OpenBoundedFile(path string, maxBytes int64) (*os.File, error) {
	file, err := openFileForBoundedRead(path)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, fmt.Errorf("not a regular file")
	}
	if info.Size() < 0 || info.Size() > maxBytes {
		_ = file.Close()
		return nil, fmt.Errorf("file exceeds the %d MiB limit", maxBytes>>20)
	}
	return file, nil
}

// ReadBoundedFile reads a regular file of at most maxBytes bytes.
func ReadBoundedFile(path string, maxBytes int64) ([]byte, error) {
	file, err := OpenBoundedFile(path, maxBytes)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("file exceeds the %d MiB limit", maxBytes>>20)
	}
	return data, nil
}
