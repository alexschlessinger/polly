package images

import (
	"bytes"
	"fmt"
	"image"
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
	return ReadBoundedFrom(file, maxBytes)
}

// ReadBoundedFrom reads an already-open file of at most maxBytes bytes,
// verifying the descriptor itself rather than trusting an earlier lookup.
func ReadBoundedFrom(file *os.File, maxBytes int64) ([]byte, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file")
	}
	if info.Size() < 0 || info.Size() > maxBytes {
		return nil, fmt.Errorf("file exceeds the %d MiB limit", maxBytes>>20)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("file exceeds the %d MiB limit", maxBytes>>20)
	}
	return data, nil
}

// DecodeBoundedFile reads an image file within maxBytes, applies Validate,
// and decodes it, reporting the detected format name.
func DecodeBoundedFile(path string, maxBytes int64) (image.Image, string, error) {
	data, err := ReadBoundedFile(path, maxBytes)
	if err != nil {
		return nil, "", err
	}
	if _, _, err := Validate(data); err != nil {
		return nil, "", err
	}
	return image.Decode(bytes.NewReader(data))
}
