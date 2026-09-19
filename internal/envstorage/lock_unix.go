//go:build unix

package envstorage

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/alexschlessinger/pollytool/internal/safefile"
	"golang.org/x/sys/unix"
)

func owned(info fs.FileInfo) error {
	s, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(s.Uid) != os.Getuid() || info.Mode().Perm()&0022 != 0 {
		return errors.New("managed storage must belong to this user and not be writable by others")
	}
	return nil
}
func identity(info fs.FileInfo) string {
	s := info.Sys().(*syscall.Stat_t)
	return fmt.Sprintf("%d:%d", s.Dev, s.Ino)
}

func lockOpen(path string) (*os.File, error) {
	if err := privateDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	f, err := safefile.OpenRegular(path, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil {
		err = owned(info)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func Lock(path string) (func(), error) {
	f, err := lockOpen(path)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { _ = f.Close() }, nil
}

// Lease is held for the lifetime of a session using an environment. Separate
// file descriptions also make other sessions in this process count as users.
type Lease struct {
	mu sync.Mutex
	f  *os.File
}

func OpenLease(path string) (*Lease, error) {
	guard, err := lockOpen(path + ".admission")
	if err != nil {
		return nil, err
	}
	defer guard.Close()
	if err := unix.Flock(int(guard.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, fmt.Errorf("environment is being reset: %w", err)
	}
	f, err := lockOpen(path)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("environment is being reset: %w", err)
	}
	return &Lease{f: f}, nil
}

func (l *Lease) Exclusive(run func() error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return errors.New("environment lease is closed")
	}
	guard, err := lockOpen(l.f.Name() + ".admission")
	if err != nil {
		return err
	}
	defer guard.Close()
	if err := unix.Flock(int(guard.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return errors.New("environment cleanup is already in progress")
	}
	if err := unix.Flock(int(l.f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if e := unix.Flock(int(l.f.Fd()), unix.LOCK_SH); e != nil {
			return errors.Join(err, e)
		}
		return errors.New("environment is in use by another session; close it before cleanup")
	}
	err = run()
	return errors.Join(err, unix.Flock(int(l.f.Fd()), unix.LOCK_SH))
}
func (l *Lease) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}
