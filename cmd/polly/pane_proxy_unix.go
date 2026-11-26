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
	"sync"
	"syscall"

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
	conn, err := net.Dial("unix", socket)
	if err != nil {
		return fmt.Errorf("reach the polly that opened this pane: %w", err)
	}
	defer conn.Close()
	hello := append(terminalPaneSize(fd).encode(), token...)
	hello = append(hello, 0)
	hello = append(hello, os.Getenv("TERM")...)
	if err := writePaneFrame(conn, paneFrameHello, hello); err != nil {
		return fmt.Errorf("greet the polly that opened this pane: %w", err)
	}
	var mode [1]byte
	if _, err := io.ReadFull(conn, mode[:]); err != nil {
		return errors.New("the polly that opened this pane did not answer")
	}
	switch mode[0] {
	case paneModeRelay:
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
	data, err := io.ReadAll(conn)
	if err != nil {
		return fmt.Errorf("read the command for this pane: %w", err)
	}
	var x paneExec
	if err := json.Unmarshal(data, &x); err != nil || len(x.Argv) == 0 {
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

	var mu sync.Mutex
	send := func(kind byte, payload []byte) error {
		mu.Lock()
		defer mu.Unlock()
		return writePaneFrame(conn, kind, payload)
	}
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	// A resize between hello and here would otherwise go unreported.
	winch <- syscall.SIGWINCH
	go func() {
		for range winch {
			if send(paneFrameResize, terminalPaneSize(fd).encode()) != nil {
				return
			}
		}
	}()
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 && send(paneFrameInput, buf[:n]) != nil {
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
	_, _ = io.Copy(os.Stdout, conn)
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
	var own, other unix.Stat_t
	if unix.Fstat(int(os.Stdin.Fd()), &own) != nil || own.Mode&unix.S_IFMT != unix.S_IFCHR {
		return true
	}
	return unix.Stat(path, &other) == nil && other.Rdev == own.Rdev
}
