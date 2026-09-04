//go:build !unix

package safefile

import "os"

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
