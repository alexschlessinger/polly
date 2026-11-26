package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
)

func TestPaneFrameRoundTripAndErrors(t *testing.T) {
	var buf bytes.Buffer
	if err := writePaneFrame(&buf, paneFrameInput, []byte("hello")); err != nil {
		t.Fatalf("write input frame: %v", err)
	}
	if err := writePaneFrame(&buf, paneFrameResize, nil); err != nil {
		t.Fatalf("write empty frame: %v", err)
	}

	kind, payload, err := readPaneFrame(&buf)
	if err != nil || kind != paneFrameInput || string(payload) != "hello" {
		t.Fatalf("first frame = (%q, %q, %v), want (%q, %q, nil)", kind, payload, err, paneFrameInput, "hello")
	}
	kind, payload, err = readPaneFrame(&buf)
	if err != nil || kind != paneFrameResize || len(payload) != 0 {
		t.Fatalf("empty frame = (%q, %q, %v), want (%q, empty, nil)", kind, payload, err, paneFrameResize)
	}
	if _, _, err := readPaneFrame(&buf); !errors.Is(err, io.EOF) {
		t.Fatalf("EOF at boundary = %v, want io.EOF", err)
	}

	// Truncated header.
	if _, _, err := readPaneFrame(bytes.NewReader([]byte{'i', 0, 0})); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated header = %v, want io.ErrUnexpectedEOF", err)
	}

	// Truncated 0-byte payload when header announces n > 0 must not look like clean EOF.
	var head [5]byte
	head[0] = paneFrameInput
	binary.BigEndian.PutUint32(head[1:], 4)
	if _, _, err := readPaneFrame(bytes.NewReader(head[:])); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated 0-byte payload = %v, want io.ErrUnexpectedEOF", err)
	}

	// Truncated partial payload.
	partial := append(head[:], 'a', 'b')
	if _, _, err := readPaneFrame(bytes.NewReader(partial)); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated partial payload = %v, want io.ErrUnexpectedEOF", err)
	}

	// Oversized frame rejected on write and read.
	huge := make([]byte, paneFrameLimit+1)
	if err := writePaneFrame(&buf, paneFrameInput, huge); err == nil {
		t.Fatal("writePaneFrame accepted oversized payload")
	}
	binary.BigEndian.PutUint32(head[1:], paneFrameLimit+1)
	if _, _, err := readPaneFrame(bytes.NewReader(head[:])); err == nil {
		t.Fatal("readPaneFrame accepted oversized length")
	}
}

func TestPaneSizeEncodeDecode(t *testing.T) {
	want := paneSize{cols: 132, rows: 43, pixelW: 1920, pixelH: 1080}
	got, ok := decodePaneSize(want.encode())
	if !ok || got != want {
		t.Fatalf("decodePaneSize = (%+v, %v), want (%+v, true)", got, ok, want)
	}
	if _, ok := decodePaneSize([]byte{1, 2, 3}); ok {
		t.Fatal("decodePaneSize accepted truncated buffer")
	}
}

func TestPaneExecEnvForwardsTerminalVars(t *testing.T) {
	owner := []string{
		"HOME=/owner",
		"POLLYTOOL_MODEL=openai/gpt-5",
		"TERM=xterm-256color",
		"COLORTERM=truecolor",
		"TERM_PROGRAM=old",
		"TMUX=/tmp/old,1,0",
		"TMUX_PANE=%0",
		"REX_BLOCK=owner-block",
		"ZELLIJ_PANE_ID=1",
		"KITTY_WINDOW_ID=1",
		"WEZTERM_PANE=1",
		"GHOSTTY_RESOURCES_DIR=/old",
		"ITERM_SESSION_ID=old",
		"TERM_SESSION_ID=old",
		"LC_TERMINAL=iTerm2",
		"LC_TERMINAL_VERSION=3.5",
	}
	pane := []string{
		"HOME=/pane",
		"TERM=xterm-ghostty",
		"TMUX=/tmp/new,2,0",
		"TMUX_PANE=%9",
		"REX_BLOCK=pane-block",
		"ZELLIJ_PANE_ID=7",
		"KITTY_WINDOW_ID=42",
		"WEZTERM_PANE=99",
		"GHOSTTY_RESOURCES_DIR=/pane/ghostty",
		"ITERM_SESSION_ID=pane-iterm",
		"TERM_SESSION_ID=pane-term",
		"LC_TERMINAL=WezTerm",
		"LC_TERMINAL_VERSION=2025",
	}
	got := paneExecEnv(owner, pane)
	want := []string{
		"HOME=/owner",
		"POLLYTOOL_MODEL=openai/gpt-5",
		"TERM=xterm-ghostty",
		"TMUX=/tmp/new,2,0",
		"TMUX_PANE=%9",
		"REX_BLOCK=pane-block",
		"ZELLIJ_PANE_ID=7",
		"KITTY_WINDOW_ID=42",
		"WEZTERM_PANE=99",
		"GHOSTTY_RESOURCES_DIR=/pane/ghostty",
		"ITERM_SESSION_ID=pane-iterm",
		"TERM_SESSION_ID=pane-term",
		"LC_TERMINAL=WezTerm",
		"LC_TERMINAL_VERSION=2025",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("paneExecEnv =\n%q\nwant\n%q", got, want)
	}
}

func TestPaneCommandSkipsFlagParsing(t *testing.T) {
	cmd := paneCommand()
	if !cmd.SkipFlagParsing {
		t.Fatal("paneCommand should set SkipFlagParsing")
	}
	err := cmd.Run(t.Context(), []string{"pane", "/tmp/sock", "-hyphen-token"})
	if err == nil {
		t.Fatal("expected error without a terminal")
	}
	if strings.Contains(err.Error(), "flag") {
		t.Fatalf("hyphen token was parsed as a flag: %v", err)
	}
}
