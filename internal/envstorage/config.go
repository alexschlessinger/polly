package envstorage

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// Configuration is copied without following links or overwriting a context's
// existing edits. Bound both the walk and copied bytes; oversized configuration
// is a setup error, not a reason to silently omit files.
func CopyConfig(roots Roots, allocation Allocation, target Roots) error {
	src, err := roots.Open(allocation)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := target.Open(allocation)
	if err != nil {
		return err
	}
	defer dst.Close()
	count, total := 0, int64(0)
	return fs.WalkDir(src.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		count++
		if count > 4096 {
			return fmt.Errorf("managed configuration has too many files")
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("configuration contains a symbolic link: %s", name)
		}
		if d.IsDir() {
			return dst.MkdirAll(name, 0700)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("configuration is not a regular file: %s", name)
		}
		if existing, err := dst.Lstat(name); err == nil {
			if !existing.Mode().IsRegular() {
				return fmt.Errorf("configuration destination is not a regular file: %s", name)
			}
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		if total > 16<<20 {
			return fmt.Errorf("managed configuration exceeds 16 MiB")
		}
		// A replacement symlink inside the opened root cannot escape that root.
		f, err := src.OpenFile(name, os.O_RDONLY|regularReadFlags, 0)
		if err != nil {
			return err
		}
		opened, err := f.Stat()
		if err != nil || !opened.Mode().IsRegular() {
			f.Close()
			return fmt.Errorf("configuration file changed during copy: %s", name)
		}
		data, err := io.ReadAll(io.LimitReader(f, (16<<20)+1))
		f.Close()
		if err != nil {
			return err
		}
		total += int64(len(data)) - info.Size()
		if total > 16<<20 {
			return fmt.Errorf("managed configuration grew during copy")
		}
		if err := dst.MkdirAll(filepath.Dir(name), 0700); err != nil {
			return err
		}
		f, err = dst.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, err = f.Write(data)
		closeErr := f.Close()
		if err != nil {
			return err
		}
		return closeErr
	})
}
