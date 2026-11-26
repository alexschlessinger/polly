//go:build unix

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestVerifyPeerUIDMatchesSelf(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "polly-peercred")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		if c, err := ln.Accept(); err == nil {
			_ = c.Close()
		}
	}()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := verifyPeerUID(conn.(*net.UnixConn)); err != nil {
		t.Fatalf("verifyPeerUID failed for same-user peer: %v", err)
	}
	<-done
}

func TestExecInPaneValidation(t *testing.T) {
	runWith := func(payload []byte) error {
		server, client := net.Pipe()
		go func() {
			_, _ = server.Write(payload)
			_ = server.Close()
		}()
		defer client.Close()
		return execInPane(client)
	}

	// Malformed JSON.
	if err := runWith([]byte("{bad json")); err == nil {
		t.Fatal("expected error for malformed JSON")
	} else {
		var syn *json.SyntaxError
		if !errors.As(err, &syn) {
			t.Fatalf("malformed JSON error did not wrap syntax error: %v", err)
		}
	}

	// Empty argv.
	emptyPayload, _ := json.Marshal(paneExec{})
	if err := runWith(emptyPayload); err == nil {
		t.Fatal("expected error for empty argv")
	}

	// Relative argv[0].
	relPayload, _ := json.Marshal(paneExec{Argv: []string{"sh", "-c", "true"}})
	if err := runWith(relPayload); err == nil {
		t.Fatal("expected error for relative argv[0]")
	}

	// Oversized payload rejected before JSON unmarshal.
	oversized := bytes.Repeat([]byte("a"), paneExecLimit+16)
	if err := runWith(oversized); err == nil {
		t.Fatal("expected error for oversized payload")
	}

	// Missing absolute executable returns wrapped ENOENT.
	missingPayload, _ := json.Marshal(paneExec{Argv: []string{"/nonexistent/polly-test-binary"}})
	if err := runWith(missingPayload); !errors.Is(err, syscall.ENOENT) {
		t.Fatalf("missing binary error = %v, want wrapped ENOENT", err)
	}
}

func TestOwnTerminalHandlesNonTerminals(t *testing.T) {
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()
	// /dev/null is S_IFCHR but not a terminal: ownTerminal must report true.
	if !ownTerminalForFD(int(devNull.Fd()), "/dev/nonexistent-tty") {
		t.Fatal("ownTerminalForFD(/dev/null) = false, want true")
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if !ownTerminalForFD(int(r.Fd()), "/dev/nonexistent-tty") {
		t.Fatal("ownTerminalForFD(pipe) = false, want true")
	}
}

func TestSplitSignalErrorHandlesSIGHUP(t *testing.T) {
	code, rem, ok := splitSignalError(&shutdownSignal{signal: syscall.SIGHUP})
	if !ok || code != 129 || rem != nil {
		t.Fatalf("splitSignalError(SIGHUP) = (%d, %v, %v), want (129, nil, true)", code, rem, ok)
	}
}
