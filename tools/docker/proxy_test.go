package docker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/docker/protocol"
)

// TestDecodeKeepsContextOutcomes: the helper reports a timeout or a
// cancellation through its marker and the proxy rebuilds the sentinel,
// whether or not the helper reached the tool; once this side's context has
// ended, its own error is the outcome.
func TestDecodeKeepsContextOutcomes(t *testing.T) {
	plain := func(message string) *protocol.ToolError {
		return &protocol.ToolError{Kind: protocol.ErrorKindPlain, Message: message}
	}
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	cases := []struct {
		name   string
		ctx    context.Context
		result protocol.Result
		want   error
	}{
		{"deadline before the tool started", context.Background(), protocol.Result{Error: plain("context deadline exceeded"), ContextErr: protocol.ContextDeadline}, context.DeadlineExceeded},
		{"cancel before the tool started", context.Background(), protocol.Result{Error: plain("context canceled"), ContextErr: protocol.ContextCanceled}, context.Canceled},
		{"deadline inside the tool", context.Background(), protocol.Result{Invoked: true, Error: plain("context deadline exceeded"), ContextErr: protocol.ContextDeadline}, context.DeadlineExceeded},
		{"deadline the tool swallowed", context.Background(), protocol.Result{Invoked: true, Text: "partial", ContextErr: protocol.ContextDeadline}, context.DeadlineExceeded},
		{"this side ended too", ended, protocol.Result{Invoked: true, Error: plain("context deadline exceeded"), ContextErr: protocol.ContextDeadline}, context.Canceled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			output, err := newProxy(&mirror{}, protocol.ToolInfo{Name: "bash"}).decode(tc.ctx, tc.result)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v (%T), want %v", err, err, tc.want)
			}
			if output.Text != tc.result.Text {
				t.Fatalf("output %q lost alongside the context error", tc.result.Text)
			}
		})
	}
}

// TestDecodeKeepsOtherFailures: a refusal before invocation keeps its text,
// and structured errors keep their kind even when the context ended with
// them.
func TestDecodeKeepsOtherFailures(t *testing.T) {
	proxy := newProxy(&mirror{}, protocol.ToolInfo{Name: "bash"})
	if _, err := proxy.decode(context.Background(), protocol.Result{Error: &protocol.ToolError{Kind: protocol.ErrorKindPlain, Message: "gate refused"}}); err == nil || err.Error() != "gate refused" {
		t.Fatalf("refused before invocation: %v", err)
	}
	if _, err := proxy.decode(context.Background(), protocol.Result{}); err == nil || err.Error() != "tool was not invoked" {
		t.Fatalf("not invoked without a message: %v", err)
	}
	var toolErr *tools.ToolError
	_, err := proxy.decode(context.Background(), protocol.Result{Invoked: true, Error: &protocol.ToolError{Kind: protocol.ErrorKindTool, Message: "no such file", Code: "not_found"}, ContextErr: protocol.ContextDeadline})
	if !errors.As(err, &toolErr) || toolErr.Code != "not_found" {
		t.Fatalf("structured error: %v", err)
	}
	var commandErr *tools.CommandError
	_, err = proxy.decode(context.Background(), protocol.Result{Invoked: true, Error: &protocol.ToolError{Kind: protocol.ErrorKindCommand, Message: "command failed: exit status 3", ExitCode: 3}})
	if !errors.As(err, &commandErr) || commandErr.ExitCode != 3 {
		t.Fatalf("command error: %v", err)
	}
	if output, err := proxy.decode(context.Background(), protocol.Result{Invoked: true, Text: "ok"}); err != nil || output.Text != "ok" {
		t.Fatalf("success: %q %v", output.Text, err)
	}
}

// TestTimeoutMillisRoundsUp: the forwarded deadline never precedes this
// side's, so the host's context ends first and its cancel reaches the helper.
func TestTimeoutMillisRoundsUp(t *testing.T) {
	cases := []struct {
		remaining time.Duration
		want      int64
	}{
		{-time.Second, 1},
		{0, 1},
		{time.Nanosecond, 1},
		{time.Millisecond, 1},
		{time.Millisecond + time.Nanosecond, 2},
		{300*time.Millisecond - time.Nanosecond, 300},
		{300 * time.Millisecond, 300},
		{300*time.Millisecond + time.Nanosecond, 301},
	}
	for _, tc := range cases {
		if got := timeoutMillis(tc.remaining); got != tc.want {
			t.Errorf("timeoutMillis(%v) = %d, want %d", tc.remaining, got, tc.want)
		}
	}
}
