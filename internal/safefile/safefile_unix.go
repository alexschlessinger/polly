//go:build unix

package safefile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// openRegular walks path from the root one component at a time, opening each
// directory with O_NOFOLLOW so a symlink anywhere in the route fails the open
// instead of redirecting it. The final component is opened non-blocking so a
// FIFO or device cannot stall the caller before it is rejected; the descriptor
// is switched back to blocking mode once it is known to be a regular file.
func openRegular(path string, flag int, perm os.FileMode) (*os.File, error) {
	if !filepath.IsAbs(path) {
		return nil, &os.PathError{Op: "open", Path: path, Err: errors.New("path is not absolute")}
	}
	path = filepath.Clean(path)
	sep := string(os.PathSeparator)
	components := strings.Split(strings.TrimPrefix(path, sep), sep)
	if len(components) == 1 && components[0] == "" {
		return nil, &NotRegularError{Path: path, Mode: os.ModeDir | 0o755}
	}

	dirfd, err := openat(unix.AT_FDCWD, sep, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: sep, Err: err}
	}
	for i, component := range components[:len(components)-1] {
		fd, err := openat(dirfd, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(dirfd)
		if err != nil {
			return nil, &os.PathError{Op: "open", Path: sep + strings.Join(components[:i+1], sep), Err: symlinkError(err)}
		}
		dirfd = fd
	}

	fd, err := openat(dirfd, components[len(components)-1], flag|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, uint32(perm.Perm()))
	unix.Close(dirfd)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: symlinkError(err)}
	}
	var st unix.Stat_t
	if err := ignoringEINTR(func() error { return unix.Fstat(fd, &st) }); err != nil {
		unix.Close(fd)
		return nil, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		unix.Close(fd)
		return nil, &NotRegularError{Path: path, Mode: fileMode(uint32(st.Mode))}
	}
	if err := unix.SetNonblock(fd, false); err != nil {
		unix.Close(fd)
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}

func openat(dirfd int, name string, flag int, mode uint32) (int, error) {
	for {
		fd, err := unix.Openat(dirfd, name, flag, mode)
		if err == syscall.EINTR {
			continue
		}
		return fd, err
	}
}

func ignoringEINTR(fn func() error) error {
	for {
		err := fn()
		if err != syscall.EINTR {
			return err
		}
	}
}

// symlinkError makes the refusal of a symlinked component recognizable: the
// kernel reports ELOOP (Linux) or EMLINK (macOS) when O_NOFOLLOW meets a link.
func symlinkError(err error) error {
	switch err {
	case syscall.ELOOP, syscall.EMLINK:
		return fmt.Errorf("%w: refusing to follow a symbolic link", err)
	}
	return err
}

func fileMode(mode uint32) os.FileMode {
	out := os.FileMode(mode & 0o777)
	switch mode & unix.S_IFMT {
	case unix.S_IFDIR:
		out |= os.ModeDir
	case unix.S_IFIFO:
		out |= os.ModeNamedPipe
	case unix.S_IFSOCK:
		out |= os.ModeSocket
	case unix.S_IFLNK:
		out |= os.ModeSymlink
	case unix.S_IFBLK:
		out |= os.ModeDevice
	case unix.S_IFCHR:
		out |= os.ModeDevice | os.ModeCharDevice
	}
	return out
}
