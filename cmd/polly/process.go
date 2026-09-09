package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/alexschlessinger/pollytool/llm"
)

// beforeExit is invoked synchronously by cleanupAndExit before os.Exit. Signal
// handling itself only cancels the run context, so ordinary shutdown unwinds
// through defers; this hook remains a final guard for explicit process exits.
var (
	beforeExitMu sync.Mutex
	beforeExit   func()
)

func setBeforeExit(fn func()) {
	beforeExitMu.Lock()
	beforeExit = fn
	beforeExitMu.Unlock()
}

// cleanupAndExit performs cleanup and exits with the given code
func cleanupAndExit(code int) {
	beforeExitMu.Lock()
	fn := beforeExit
	beforeExitMu.Unlock()
	if fn != nil {
		fn()
	}
	os.Exit(code)
}

// readFromStdin reads all of stdin as one prompt: CRLF line endings are
// normalized and the trailing newline dropped. The whole input is read rather
// than scanned line by line, so a single long line — minified JSON, a source
// map, a document without breaks — is not rejected at bufio.Scanner's default
// 64 KiB line limit.
func readFromStdin() (string, error) {
	return readAllInput(os.Stdin)
}

func readAllInput(r io.Reader) (string, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("error reading stdin: %w", err)
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	return strings.TrimSuffix(text, "\n"), nil
}

// hasStdinData checks if stdin has data available
func hasStdinData() bool {
	stat, _ := os.Stdin.Stat()
	return (stat.Mode() & os.ModeCharDevice) == 0
}

// shutdownSignal is carried as the cancellation cause so main can distinguish
// an expected process signal from an ordinary context cancellation after all
// command defers have unwound.
type shutdownSignal struct {
	signal os.Signal
}

func (e *shutdownSignal) Error() string {
	return e.signal.String()
}

// splitSignalError finds the process signal while retaining independent
// cleanup failures joined during deferred unwinding. The expected signal branch
// stays silent, but losing the session/store/tool cleanup error would hide
// durable-state failures at exactly the point they matter most.
func splitSignalError(err error) (int, error, bool) {
	var shutdown *shutdownSignal
	if !errors.As(err, &shutdown) {
		return 0, nil, false
	}
	code := 1
	switch shutdown.signal {
	case os.Interrupt:
		code = 130
	case syscall.SIGTERM:
		code = 143
	}
	return code, stripShutdownSignal(err), true
}

// stripShutdownSignal drops every branch of a joined error tree that carries
// the shutdown signal, keeping the independent failures joined beside it.
func stripShutdownSignal(err error) error {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var remaining []error
		for _, child := range joined.Unwrap() {
			if child = stripShutdownSignal(child); child != nil {
				remaining = append(remaining, child)
			}
		}
		return errors.Join(remaining...)
	}
	var shutdown *shutdownSignal
	if errors.As(err, &shutdown) {
		return nil
	}
	return err
}

// setupSignalHandling sets up signal handling for graceful shutdown. The
// returned stop function unregisters the process handlers and cancels the
// context when the caller finishes normally.
func setupSignalHandling(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(ctx)
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case received := <-signals:
			cancel(&shutdownSignal{signal: received})
		case <-ctx.Done():
		}
	}()
	return ctx, func() {
		signal.Stop(signals)
		cancel(context.Canceled)
	}
}

// outputStructured validates and prints a structured response. Only output
// that parses as JSON and satisfies the schema reaches stdout; anything else
// is an error, with the raw reply on stderr for inspection, so a pipeline
// reading --schema output never mistakes a malformed or off-schema reply for
// success.
func outputStructured(content string, schema *llm.Schema) error {
	return writeStructured(os.Stdout, os.Stderr, content, schema)
}

func writeStructured(stdout, stderr io.Writer, content string, schema *llm.Schema) error {
	// Empty content means no structured output was produced — e.g. the model
	// emitted a tool call that was denied and the turn short-circuited. Report
	// it instead of printing a silent blank line that looks like success.
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("no structured output produced")
	}
	var data any
	if err := json.Unmarshal([]byte(content), &data); err != nil {
		fmt.Fprintln(stderr, content)
		return fmt.Errorf("structured output is not valid JSON: %w", err)
	}
	if err := schema.Validate(content); err != nil {
		fmt.Fprintln(stderr, content)
		return fmt.Errorf("structured output does not match the schema: %w", err)
	}
	jsonBytes, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("format structured output: %w", err)
	}
	fmt.Fprintln(stdout, string(jsonBytes))
	return nil
}
