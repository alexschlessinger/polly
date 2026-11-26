package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/urfave/cli/v3"
)

// A pane is another terminal that a running polly put there. The polly that
// owns the session listens on a Unix socket; `polly pane` runs in the other
// terminal, says hello, and is told what the pane is for.
//
// A relayed pane is one the owner paints: its keys and size go to the owner
// in frames, and the owner's screen output comes back as raw bytes. All state
// stays in the owner, so the pane shows live turns and needs no session lease
// of its own.
//
// A pane that runs a command becomes that command, with the owner's
// environment and directory handed over the socket rather than on a command
// line or through a shell's startup files: another polly on another session,
// or the user's editor.

// Frames the proxy sends: one kind byte, a big-endian uint32 length, then the
// payload.
const (
	// paneFrameHello opens the connection: the terminal size, then the pane's
	// token and the proxy's TERM separated by a NUL.
	paneFrameHello byte = 'h'
	// paneFrameInput carries bytes read from the terminal.
	paneFrameInput byte = 'i'
	// paneFrameResize carries the terminal's new size.
	paneFrameResize byte = 'r'
	// paneFrameWho is not a pane's but another polly's, sent in place of
	// hello: it asks whether a session is open here without mutating state
	// (see repl_pane_peers.go).
	paneFrameWho byte = 'w'
	// paneFrameActivate asks a polly that holds a session to show that
	// session's workspace, sent only after host compatibility is confirmed.
	paneFrameActivate byte = 'a'

	paneFrameLimit = 64 << 10
)

// The owner's answer to hello starts with what the pane is for.
const (
	// paneModeRelay: the owner paints this terminal, and its screen follows.
	paneModeRelay byte = 'r'
	// paneModeExec: a paneExec follows as JSON, and then the owner hangs up.
	paneModeExec byte = 'x'
)

// paneExec is a command for a pane to become. Argv[0] is an absolute path.
type paneExec struct {
	Argv []string `json:"argv"`
	Env  []string `json:"env"`
	Dir  string   `json:"dir"`
}

// paneExecEnv is the environment a pane's command runs with: the owner's,
// except for what describes the terminal, which is the pane's own. The owner
// sits in a different terminal, and its REX_BLOCK, TMUX_PANE or
// ZELLIJ_PANE_ID names the wrong pane.
func paneExecEnv(owner, pane []string) []string {
	terminal := func(entry string) bool {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "TERM", "COLORTERM", "TERM_PROGRAM", "TERM_PROGRAM_VERSION", "TMUX", "TMUX_PANE":
			return true
		}
		return strings.HasPrefix(key, "REX_") || strings.HasPrefix(key, "ZELLIJ")
	}
	env := make([]string, 0, len(owner))
	for _, entry := range owner {
		if !terminal(entry) {
			env = append(env, entry)
		}
	}
	for _, entry := range pane {
		if terminal(entry) {
			env = append(env, entry)
		}
	}
	return env
}

// paneSize is a terminal's size in cells and in pixels (zero when unknown).
type paneSize struct{ cols, rows, pixelW, pixelH uint16 }

const paneSizeBytes = 8

func (s paneSize) encode() []byte {
	b := make([]byte, paneSizeBytes)
	binary.BigEndian.PutUint16(b[0:], s.cols)
	binary.BigEndian.PutUint16(b[2:], s.rows)
	binary.BigEndian.PutUint16(b[4:], s.pixelW)
	binary.BigEndian.PutUint16(b[6:], s.pixelH)
	return b
}

func decodePaneSize(b []byte) (paneSize, bool) {
	if len(b) < paneSizeBytes {
		return paneSize{}, false
	}
	return paneSize{
		cols:   binary.BigEndian.Uint16(b[0:]),
		rows:   binary.BigEndian.Uint16(b[2:]),
		pixelW: binary.BigEndian.Uint16(b[4:]),
		pixelH: binary.BigEndian.Uint16(b[6:]),
	}, true
}

// writePaneFrame sends one frame in a single Write, so frames from the input
// and resize goroutines cannot interleave under the caller's lock.
func writePaneFrame(w io.Writer, kind byte, payload []byte) error {
	if len(payload) > paneFrameLimit {
		return fmt.Errorf("pane frame of %d bytes exceeds %d", len(payload), paneFrameLimit)
	}
	frame := make([]byte, 5+len(payload))
	frame[0] = kind
	binary.BigEndian.PutUint32(frame[1:], uint32(len(payload)))
	copy(frame[5:], payload)
	_, err := w.Write(frame)
	return err
}

func readPaneFrame(r io.Reader) (kind byte, payload []byte, err error) {
	var head [5]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(head[1:])
	if n > paneFrameLimit {
		return 0, nil, fmt.Errorf("pane frame of %d bytes exceeds %d", n, paneFrameLimit)
	}
	payload = make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return head[0], payload, nil
}

// paneCommand is the proxy half. The owning polly starts it in the pane it
// opens; it is not something a person runs, so it stays out of the help.
func paneCommand() *cli.Command {
	return &cli.Command{
		Name:      "pane",
		Usage:     "Serve this terminal to the polly that opened it",
		ArgsUsage: "<socket> <token>",
		Hidden:    true,
		Action: func(_ context.Context, cmd *cli.Command) error {
			if cmd.Args().Len() != 2 {
				return errors.New("polly pane is started by polly for a pane it opens")
			}
			return runPaneProxy(cmd.Args().Get(0), cmd.Args().Get(1))
		},
	}
}
