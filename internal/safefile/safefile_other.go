//go:build !unix

package safefile

import (
	"errors"
	"os"
)

// openRegular falls back to os.OpenFile where per-component O_NOFOLLOW is
// unavailable, still verifying the opened descriptor is a regular file.
func openRegular(path string, flag int, perm os.FileMode) (*os.File, error) {
	f, err := os.OpenFile(path, flag, perm)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, &NotRegularError{Path: path, Mode: info.Mode()}
	}
	return f, nil
}

// openRegularFollow is openRegular's read-only form: os.OpenFile already
// follows symlinks here.
func openRegularFollow(path string) (*os.File, error) {
	return openRegular(path, os.O_RDONLY, 0)
}

// openDirectory falls back to os.Open, verifying the opened object is a
// directory.
func openDirectory(path string) (*os.File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !info.IsDir() {
		_ = f.Close()
		return nil, &os.PathError{Op: "open", Path: path, Err: errors.New("not a directory")}
	}
	return f, nil
}
