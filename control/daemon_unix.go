//go:build darwin || linux

package control

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

// maxSocketPath is the longest path a socket's address holds, without its
// terminating NUL.
var maxSocketPath = func() int {
	if runtime.GOOS == "linux" {
		return 107
	}
	return 103
}()

// logCap is the size past which a daemon's log is emptied when the next
// daemon is started on it.
const logCap = 16 << 20

var (
	darwinTempOnce sync.Once
	darwinTemp     string
	darwinTempErr  error
)

// UserSocketDir is the user's own directory for a daemon's sockets and
// locks: on macOS under the temporary directory the system gives the user
// (confstr's _CS_DARWIN_USER_TEMP_DIR, which $TMPDIR does not move), on
// Linux under $XDG_RUNTIME_DIR, else /tmp/polly-zig-<uid>.
func UserSocketDir() (string, error) {
	if runtime.GOOS == "darwin" {
		darwinTempOnce.Do(func() {
			out, err := exec.Command("/usr/bin/getconf", "DARWIN_USER_TEMP_DIR").Output()
			if err != nil {
				darwinTempErr = fmt.Errorf("the system gives this user no temporary directory: %w", err)
				return
			}
			darwinTemp = strings.TrimRight(strings.TrimSpace(string(out)), "/")
			if darwinTemp == "" || !filepath.IsAbs(darwinTemp) {
				darwinTempErr = errors.New("the system gives this user no temporary directory")
			}
		})
		if darwinTempErr != nil {
			return "", darwinTempErr
		}
		return darwinTemp + "/" + DaemonDirName, nil
	}
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" && filepath.IsAbs(dir) {
		return strings.TrimRight(dir, "/") + "/" + DaemonDirName, nil
	}
	return fmt.Sprintf("/tmp/%s-%d", DaemonDirName, os.Getuid()), nil
}

// ownDir makes dir with mode 0700 where it is not there, and holds it to
// being the user's own: a directory and no link, owned by this user, closed
// to group and others.
func ownDir(dir string) error {
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("cannot make the socket directory %s: %w", dir, err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("cannot look at the socket directory %s: %w", dir, err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || !ok || int(st.Uid) != os.Getuid() || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("the socket directory %s is not this user's own and closed to others", dir)
	}
	return nil
}

func socketDir(b Bootstrap) (string, error) {
	dir := b.SocketDir
	if dir == "" {
		var err error
		if dir, err = UserSocketDir(); err != nil {
			return "", err
		}
	}
	if err := ownDir(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// SocketPath is the socket of build key's daemon in dir.
func SocketPath(dir, buildKey string) string {
	return fmt.Sprintf("%s/%d-%s.sock", dir, Protocol, buildKey)
}

func lockPath(dir, buildKey string) string {
	return fmt.Sprintf("%s/%d-%s.lock", dir, Protocol, buildKey)
}

// dialUnix connects to a socket; a daemon nobody listens for is
// errNobodyListens.
func dialUnix(ctx context.Context, path string) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	if err == nil {
		return conn, nil
	}
	if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EAGAIN) {
		return nil, fmt.Errorf("%w: %v", errNobodyListens, err)
	}
	return nil, fmt.Errorf("cannot connect to %s: %w", path, err)
}

var errNobodyListens = errors.New("nobody listens")

// dialExisting connects to the daemon that is there, starting nothing.
func dialExisting(ctx context.Context, b Bootstrap) (*Client, error) {
	path, err := targetSocket(b)
	if err != nil {
		return nil, err
	}
	var conn net.Conn
	for tries := 0; ; tries++ {
		conn, err = dialUnix(ctx, path)
		if err == nil {
			break
		}
		// A backlog that is full is tried again for a moment.
		if !errors.Is(err, errNobodyListens) || tries >= 50 || !strings.Contains(err.Error(), "resource temporarily unavailable") {
			return nil, fmt.Errorf("no backend daemon at %s: %w", path, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return NewClient(conn), nil
}

func targetSocket(b Bootstrap) (string, error) {
	if b.Target.Scheme == SchemeUnix {
		return b.Target.Socket, nil
	}
	dir, err := socketDir(b)
	if err != nil {
		return "", err
	}
	return SocketPath(dir, b.BuildKey), nil
}

// connectOrStart connects to the daemon, or, for an exec:// target where
// none lives, starts it and returns its end of the start pair.
func connectOrStart(ctx context.Context, b Bootstrap) (*Client, bool, error) {
	if b.Target.Scheme == SchemeUnix {
		conn, err := dialUnix(ctx, b.Target.Socket)
		if err != nil {
			return nil, false, fmt.Errorf("no backend daemon at %s: %w", b.Target.Socket, err)
		}
		return NewClient(conn), false, nil
	}
	if b.BuildKey == "" {
		return nil, false, errors.New("an exec:// backend needs its build key")
	}
	dir, err := socketDir(b)
	if err != nil {
		return nil, false, err
	}
	sock := SocketPath(dir, b.BuildKey)
	if len(sock) > maxSocketPath {
		return nil, false, fmt.Errorf("the socket's name is longer than an address holds: %s", sock)
	}
	lock := lockPath(dir, b.BuildKey)
	began := time.Now()
	pause := time.Millisecond
	for {
		conn, err := dialUnix(ctx, sock)
		if err == nil {
			return NewClient(conn), false, nil
		}
		if !errors.Is(err, errNobodyListens) {
			return nil, false, err
		}
		held, err := takeLock(lock)
		if err != nil {
			return nil, false, err
		}
		if held != nil {
			client, err := startDaemon(b, sock, held)
			if err != nil {
				return nil, false, err
			}
			return client, true, nil
		}
		if time.Since(began) >= b.Limit {
			return nil, false, fmt.Errorf("no backend daemon answered within %s, its lock held", b.Limit)
		}
		select {
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case <-time.After(pause):
		}
		pause = min(pause*2, 50*time.Millisecond)
	}
}

// takeLock takes the daemon's lock without waiting: its file, or nil where
// another holds it. It is held only as the file its path names now.
func takeLock(path string) (*os.File, error) {
	for range 8 {
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			return nil, fmt.Errorf("cannot open the lock %s: %w", path, err)
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			f.Close()
			if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
				return nil, nil
			}
			return nil, fmt.Errorf("cannot take the lock %s: %w", path, err)
		}
		held, err1 := f.Stat()
		named, err2 := os.Lstat(path)
		if err1 == nil && err2 == nil && os.SameFile(held, named) {
			return f, nil
		}
		f.Close()
	}
	return nil, fmt.Errorf("the lock %s was another file each time it was taken", path)
}

// startDaemon binds and listens on the socket, and starts the daemon with
// the listening socket, the lock and its end of a socket pair as its
// descriptors 3, 4 and 5. It returns this launch's end of the pair, on
// which the start line goes.
func startDaemon(b Bootstrap, sock string, lock *os.File) (_ *Client, retErr error) {
	defer lock.Close()
	_ = os.Remove(sock)
	listenFD, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, fmt.Errorf("cannot make a socket: %w", err)
	}
	syscall.CloseOnExec(listenFD)
	listen := os.NewFile(uintptr(listenFD), "listen")
	defer listen.Close()
	if err := syscall.Bind(listenFD, &syscall.SockaddrUnix{Name: sock}); err != nil {
		return nil, fmt.Errorf("cannot bind %s: %w", sock, err)
	}
	defer func() {
		if retErr != nil {
			_ = os.Remove(sock)
		}
	}()
	if err := os.Chmod(sock, 0o600); err != nil {
		return nil, fmt.Errorf("cannot set the mode of %s: %w", sock, err)
	}
	if err := syscall.Listen(listenFD, 1024); err != nil {
		return nil, fmt.Errorf("cannot listen on %s: %w", sock, err)
	}
	pair, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, fmt.Errorf("cannot make a socket pair: %w", err)
	}
	syscall.CloseOnExec(pair[0])
	syscall.CloseOnExec(pair[1])
	daemonEnd := os.NewFile(uintptr(pair[0]), "start")
	defer daemonEnd.Close()
	ours := os.NewFile(uintptr(pair[1]), "start")

	logPath := SetupLog(b.Start.Runtime, b.BuildKey)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		ours.Close()
		return nil, fmt.Errorf("cannot make the log directory: %w", err)
	}
	flags := os.O_WRONLY | os.O_APPEND | os.O_CREATE
	if info, err := os.Stat(logPath); err == nil && info.Size() > logCap {
		flags |= os.O_TRUNC
	}
	log, err := os.OpenFile(logPath, flags, 0o600)
	if err != nil {
		ours.Close()
		return nil, fmt.Errorf("cannot open the daemon's log %s: %w", logPath, err)
	}
	defer log.Close()

	env := withoutProviderKeys(b.Environ)
	env = slicesDeleteVar(env, WorkspaceVar)
	if b.Start.Setup.Workspace != "" {
		env = append(env, WorkspaceVar+"="+b.Start.Setup.Workspace)
	}
	cmd := &exec.Cmd{
		Path:        b.Program.Path,
		Args:        []string{b.Program.Path, "serve"},
		Dir:         "/",
		Env:         env,
		Stdout:      log,
		Stderr:      log,
		ExtraFiles:  []*os.File{listen, lock, daemonEnd},
		SysProcAttr: &syscall.SysProcAttr{Setsid: true},
	}
	if err := cmd.Start(); err != nil {
		ours.Close()
		return nil, fmt.Errorf("cannot start the backend daemon %s: %w", b.Program.Path, err)
	}
	// The daemon lives on its own; this launch reaps it only if it ends
	// while the launch is still here.
	go func() { _ = cmd.Wait() }()
	conn, err := net.FileConn(ours)
	ours.Close()
	if err != nil {
		return nil, fmt.Errorf("cannot read the daemon's start pair: %w", err)
	}
	return NewClient(conn), nil
}

func slicesDeleteVar(environ []string, name string) []string {
	out := environ[:0:0]
	for _, kv := range environ {
		if strings.HasPrefix(kv, name+"=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}
