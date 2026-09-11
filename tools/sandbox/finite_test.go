package sandbox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"testing"
)

func TestWrapFiniteCmdRequiresContext(t *testing.T) {
	cmd := exec.Command("unused")
	called := false
	cleanup, err := WrapFiniteCmdManaged(stubSandbox{rewrite: func(*exec.Cmd) { called = true }}, cmd)
	if err == nil || called {
		t.Fatalf("invalid command reached wrapping: %v / %v", err, called)
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
}

func TestWrapFiniteCmdDescriptorOwnership(t *testing.T) {
	for _, managed := range []bool{false, true} {
		for _, fail := range []bool{false, true} {
			borrowed, err := os.CreateTemp(t.TempDir(), "caller")
			if err != nil {
				t.Fatal(err)
			}
			defer borrowed.Close()
			owned, err := os.CreateTemp(t.TempDir(), "sandbox")
			if err != nil {
				t.Fatal(err)
			}
			defer owned.Close()
			var wrapErr error
			if fail {
				wrapErr = errors.New("wrapper failed")
			}
			var sb Sandbox = extraFileSandbox{file: owned, err: wrapErr}
			if managed {
				sb = managedExtraFileSandbox{extraFiles: []*os.File{owned}, err: wrapErr}
			}
			cmd := exec.CommandContext(context.Background(), "unused")
			cmd.ExtraFiles = []*os.File{borrowed}
			cleanup, err := WrapFiniteCmdManaged(sb, cmd)
			if !errors.Is(err, wrapErr) {
				t.Fatalf("wrap error: %v, want %v", err, wrapErr)
			}
			if err := cleanup(); err != nil {
				t.Fatal(err)
			}
			if err := cleanup(); err != nil {
				t.Fatalf("cleanup not idempotent: %v", err)
			}
			if _, err := borrowed.Stat(); err != nil {
				t.Fatalf("caller descriptor closed: %v", err)
			}
			if _, err := owned.Stat(); (err != nil) != managed {
				t.Fatalf("managed=%v fail=%v: wrong descriptor ownership: %v", managed, fail, err)
			}
		}
	}
}

func TestWrapFiniteCmdAlreadyExited(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX shell")
	}
	cmd := exec.CommandContext(context.Background(), "sh", "-c", "exit 0")
	cleanup, err := WrapFiniteCmdManaged(nil, cmd)
	if err != nil {
		t.Fatal(err)
	}
	err = cmd.Start()
	if closeErr := cleanup(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Cancel(); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("late cancellation: %v", err)
	}
}
