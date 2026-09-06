package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
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
		if code, ok := signalExitCode(context.Cause(ctx)); !ok || code != 130 {
			t.Fatalf("signal cause maps to (%d, %v), want (130, true)", code, ok)
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

func TestCLISignalExitStatusAndStderr(t *testing.T) {
	const helperEnv = "POLLY_CLI_SIGNAL_HELPER"
	const baseURLEnv = "POLLY_CLI_SIGNAL_BASE_URL"
	if os.Getenv(helperEnv) == "1" {
		os.Args = []string{
			"polly",
			"--nosandbox",
			"--model", "openai/signal-test",
			"--baseurl", os.Getenv(baseURLEnv),
			"--prompt", "wait for cancellation",
		}
		main()
		panic("main returned without the signal exit path")
	}

	if runtime.GOOS == "windows" {
		t.Skip("process signals cannot be sent reliably on Windows")
	}

	tests := []struct {
		name     string
		signal   os.Signal
		wantCode int
	}{
		{name: "interrupt", signal: os.Interrupt, wantCode: 130},
		{name: "term", signal: syscall.SIGTERM, wantCode: 143},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			requestStarted := make(chan struct{})
			releaseRequest := make(chan struct{})
			var startedOnce sync.Once
			server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
				startedOnce.Do(func() { close(requestStarted) })
				select {
				case <-request.Context().Done():
				case <-releaseRequest:
				}
			}))
			defer func() {
				close(releaseRequest)
				server.Close()
			}()

			command := exec.Command(os.Args[0], "-test.run=^TestCLISignalExitStatusAndStderr$/"+tc.name+"$")
			command.Env = append(filteredSignalTestEnv("HOME", "OPENAI_API_KEY", helperEnv, baseURLEnv),
				"HOME="+t.TempDir(),
				"OPENAI_API_KEY=signal-test-key",
				helperEnv+"=1",
				baseURLEnv+"="+server.URL+"/v1",
			)
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			command.Stdout = &stdout
			command.Stderr = &stderr
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}

			select {
			case <-requestStarted:
			case <-time.After(10 * time.Second):
				_ = command.Process.Kill()
				_ = command.Wait()
				t.Fatalf("CLI did not reach the blocking model request; stderr=%q", stderr.String())
			}
			if err := command.Process.Signal(tc.signal); err != nil {
				_ = command.Process.Kill()
				_ = command.Wait()
				t.Fatal(err)
			}

			waitDone := make(chan error, 1)
			go func() { waitDone <- command.Wait() }()
			var err error
			select {
			case err = <-waitDone:
			case <-time.After(10 * time.Second):
				_ = command.Process.Kill()
				err = <-waitDone
				t.Fatalf("CLI did not exit after %s: %v; stderr=%q", tc.name, err, stderr.String())
			}
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != tc.wantCode {
				t.Fatalf("CLI exit error = %v (code %d), want code %d; stderr=%q", err, command.ProcessState.ExitCode(), tc.wantCode, stderr.String())
			}
			if got := strings.TrimSpace(stderr.String()); !strings.HasPrefix(got, "Working\nStopped · ") || strings.ContainsAny(got, "\x1b\r") || strings.Contains(got, "Error:") {
				t.Fatalf("CLI should settle interrupted activity without an error or cursor controls: %q", got)
			}
		})
	}
}

func TestSplitSignalErrorPreservesCleanupFailures(t *testing.T) {
	cleanupErr := errors.New("close session failed")
	code, remaining, ok := splitSignalError(errors.Join(
		&exitError{code: 1, err: &shutdownSignal{signal: os.Interrupt}},
		fmt.Errorf("cleanup: %w", cleanupErr),
	))
	if !ok || code != 130 {
		t.Fatalf("split signal = code %d, found %v", code, ok)
	}
	if !errors.Is(remaining, cleanupErr) {
		t.Fatalf("remaining error = %v, want cleanup failure", remaining)
	}
	var shutdown *shutdownSignal
	if errors.As(remaining, &shutdown) {
		t.Fatalf("remaining error still contains shutdown signal: %v", remaining)
	}
}

func TestTurnPersistencePreservesShutdownSignalCause(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	session := testAcquireSession(t, store, "signal-persistence")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(&shutdownSignal{signal: os.Interrupt})

	code, err := executeTurnWithUserMessage(ctx, &Config{}, &conversationState{session: session},
		messages.ChatMessage{Role: messages.MessageRoleUser, Content: "hello"}, nil, nil, nil, false)
	if code != 1 {
		t.Fatalf("turn exit code = %d, want hard-error code 1 before process mapping", code)
	}
	if signalCode, ok := signalExitCode(err); !ok || signalCode != 130 {
		t.Fatalf("turn error = %v; signal mapping = (%d, %v), want SIGINT/130", err, signalCode, ok)
	}
}

func filteredSignalTestEnv(drop ...string) []string {
	dropped := make(map[string]struct{}, len(drop))
	for _, key := range drop {
		dropped[key] = struct{}{}
	}
	result := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "POLLYTOOL_") {
			continue
		}
		if _, skip := dropped[key]; !skip {
			result = append(result, entry)
		}
	}
	return result
}
