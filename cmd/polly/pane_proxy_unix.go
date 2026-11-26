//go:build unix

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

const paneProxySupported = true

// runPaneProxy greets the polly listening on socket and does what it is told
// this pane is for: relay the terminal, or become a command.
func runPaneProxy(socket, token string) error {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return errors.New("polly pane needs a terminal")
	}
	conn, err := net.DialTimeout("unix", socket, paneHelloTimeout)
	if err != nil {
		return fmt.Errorf("reach the polly that opened this pane: %w", err)
	}
	defer conn.Close()
	if uconn, ok := conn.(*net.UnixConn); ok {
		if err := verifyPeerUID(uconn); err != nil {
			return fmt.Errorf("verify the polly that opened this pane: %w", err)
		}
	}
	_ = conn.SetDeadline(time.Now().Add(paneHelloTimeout))
	hello := append(terminalPaneSize(fd).encode(), token...)
	hello = append(hello, 0)
	hello = append(hello, os.Getenv("TERM")...)
	if err := writePaneFrame(conn, paneFrameHello, hello); err != nil {
		return fmt.Errorf("greet the polly that opened this pane: %w", err)
	}
	var mode [1]byte
	if _, err := io.ReadFull(conn, mode[:]); err != nil {
		return fmt.Errorf("the polly that opened this pane did not answer: %w", err)
	}
	switch mode[0] {
	case paneModeRelay:
		_ = conn.SetDeadline(time.Time{})
		return relayPane(conn, fd)
	case paneModeExec:
		return execInPane(conn)
	}
	return fmt.Errorf("the polly that opened this pane answered %q", mode[0])
}

// execInPane replaces this process with the command the owner sent. The
// terminal is untouched until then, so the host can tell from the command's
// own behavior whether it wanted one.
func execInPane(conn net.Conn) error {
	_ = conn.SetReadDeadline(time.Now().Add(paneHelloTimeout))
	data, err := io.ReadAll(io.LimitReader(conn, paneExecLimit+1))
	if err != nil {
		return fmt.Errorf("read the command for this pane: %w", err)
	}
	if len(data) > paneExecLimit {
		return fmt.Errorf("command payload exceeds %d bytes", paneExecLimit)
	}
	var x paneExec
	if err := json.Unmarshal(data, &x); err != nil {
		return fmt.Errorf("decode the command for this pane: %w", err)
	}
	if len(x.Argv) == 0 || !filepath.IsAbs(x.Argv[0]) {
		return errors.New("the polly that opened this pane sent no command")
	}
	_ = conn.Close()
	if x.Dir != "" {
		// A directory that has since gone leaves the command where it is.
		_ = os.Chdir(x.Dir)
	}
	if err := syscall.Exec(x.Argv[0], x.Argv, paneExecEnv(x.Env, os.Environ())); err != nil {
		return fmt.Errorf("run %s: %w", x.Argv[0], err)
	}
	return nil
}

// relayPane relays this terminal to the owner until the owner closes the
// connection or the terminal goes away. It holds no state: the owner's screen
// puts the terminal into and out of its modes with the bytes it sends, and
// only raw mode is this process's to restore.
func relayPane(conn net.Conn, fd int) error {
	state, err := term.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("set raw mode: %w", err)
	}
	defer func() { _ = term.Restore(fd, state) }()

	done := make(chan struct{})
	defer close(done)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	var sigMu sync.Mutex
	var gotSig os.Signal
	go func() {
		select {
		case sig := <-sigs:
			sigMu.Lock()
			gotSig = sig
			sigMu.Unlock()
			_ = conn.Close()
		case <-done:
		}
	}()

	var mu sync.Mutex
	send := func(kind byte, payload []byte) error {
		mu.Lock()
		defer mu.Unlock()
		return writePaneFrame(conn, kind, payload)
	}
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		for {
			select {
			case <-winch:
				if send(paneFrameResize, terminalPaneSize(fd).encode()) != nil {
					_ = conn.Close()
					return
				}
			case <-done:
				return
			}
		}
	}()
	// A resize between hello and here would otherwise go unreported.
	select {
	case winch <- syscall.SIGWINCH:
	default:
	}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 && send(paneFrameInput, buf[:n]) != nil {
				_ = conn.Close()
				return
			}
			if err != nil {
				// The terminal is gone; closing ends the copy below.
				_ = conn.Close()
				return
			}
		}
	}()
	// The owner closing its end is the ordinary way a pane ends.
	_, copyErr := io.Copy(os.Stdout, conn)
	sigMu.Lock()
	sig := gotSig
	sigMu.Unlock()
	if sig != nil {
		return &shutdownSignal{signal: sig}
	}
	if copyErr != nil && !errors.Is(copyErr, net.ErrClosed) {
		return copyErr
	}
	return nil
}

func terminalPaneSize(fd int) paneSize {
	ws, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
	if err != nil {
		return paneSize{}
	}
	return paneSize{cols: ws.Col, rows: ws.Row, pixelW: ws.Xpixel, pixelH: ws.Ypixel}
}

// ownTerminal reports whether the terminal device at path is the one polly's
// own input is on. With no terminal on its input there is nothing to tell
// by, and the answer is yes.
func ownTerminal(path string) bool {
	return ownTerminalForFD(int(os.Stdin.Fd()), path)
}

func ownTerminalForFD(fd int, path string) bool {
	if !term.IsTerminal(fd) {
		return true
	}
	var own, other unix.Stat_t
	if unix.Fstat(fd, &own) != nil || own.Mode&unix.S_IFMT != unix.S_IFCHR {
		return true
	}
	return unix.Stat(path, &other) == nil &&
		other.Mode&unix.S_IFMT == unix.S_IFCHR &&
		other.Rdev == own.Rdev
}
