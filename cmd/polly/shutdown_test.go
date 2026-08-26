package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestSetupSignalHandlingUnwindsInsteadOfExiting(t *testing.T) {
	const helperEnv = "POLLY_SIGNAL_UNWIND_HELPER"
	const markerEnv = "POLLY_SIGNAL_UNWIND_MARKER"
	if os.Getenv(helperEnv) == "1" {
		ctx, stop := setupSignalHandling(context.Background())
		defer stop()
		marker := os.Getenv(markerEnv)
		defer func() {
			if err := os.WriteFile(marker, []byte("deferred cleanup ran"), 0o600); err != nil {
				t.Errorf("write unwind marker: %v", err)
			}
		}()
		process, err := os.FindProcess(os.Getpid())
		if err != nil {
			t.Fatal(err)
		}
		if err := process.Signal(os.Interrupt); err != nil {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
			t.Fatal("signal did not cancel the run context")
		}
		return
	}

	if runtime.GOOS == "windows" {
		t.Skip("os.Interrupt cannot be sent to a Windows process")
	}
	marker := filepath.Join(t.TempDir(), "unwound")
	command := exec.Command(os.Args[0], "-test.run=^TestSetupSignalHandlingUnwindsInsteadOfExiting$")
	command.Env = append(os.Environ(), helperEnv+"=1", markerEnv+"="+marker)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("signal helper exited before unwinding: %v\n%s", err, output)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "deferred cleanup ran" {
		t.Fatalf("deferred cleanup marker = %q, %v", data, err)
	}
}
