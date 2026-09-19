package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

// The report pipe is descriptor 3, ahead of the startup acknowledgment, and
// the explicit environment reaches the command.
func TestFiniteCommandReportAndEnvironment(t *testing.T) {
	skipIfWindows(t)
	output, report := newBoundedBuffer(1024), newBoundedBuffer(1024)
	_, err := runFiniteCommand(context.Background(), nil, finiteCommand{
		name: "bash", args: []string{"-c", `printf '%s' "$TRIAL_VALUE" >&3; echo out`},
		env:    map[string]string{"TRIAL_VALUE": "reported"},
		stdout: output, stderr: output, report: report, acknowledge: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(report.Bytes()); got != "reported" {
		t.Fatalf("report = %q, want the explicit variable", got)
	}
	if got := output.String(); got != "out\n" {
		t.Fatalf("output = %q", got)
	}
}

// Where the sandbox gives no way to observe denials, a trial still runs its
// command and says why nothing was seen.
func TestRunTrialWithoutObservableDenials(t *testing.T) {
	skipIfWindows(t)
	t.Setenv("HOME", t.TempDir())
	registry := stubSandboxRegistry(t, sandbox.Config{})
	defer registry.Close()
	result, err := registry.RunTrial(context.Background(), "echo out; echo err >&2; exit 5", sandbox.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 5 || result.Output != "out\nerr" {
		t.Fatalf("trial = exit %d, output %q; want exit 5 with both streams in order", result.ExitCode, result.Output)
	}
	if !strings.Contains(result.Observation.Incomplete, "cannot be observed") || len(result.Observation.Denials) != 0 {
		t.Fatalf("observation = %+v; want it to say denials cannot be observed", result.Observation)
	}
}

func TestRunTrialRefusals(t *testing.T) {
	ctx := context.Background()
	registry := stubSandboxRegistry(t, sandbox.Config{})
	defer registry.Close()
	if _, err := registry.RunTrial(ctx, "  ", sandbox.Config{}); err == nil {
		t.Fatal("a trial ran an empty command")
	}
	bound, _, err := registry.BindExecutionContext(ExecutionContext{Root: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer bound.Close()
	if _, err := bound.RunTrial(ctx, "true", sandbox.Config{}); err == nil || !strings.Contains(err.Error(), "execution context") {
		t.Fatalf("a bound registry ran a trial: %v", err)
	}
	unsandboxed := NewToolRegistry(nil, WithUnsafeNoSandbox())
	defer unsandboxed.Close()
	if _, err := unsandboxed.RunTrial(ctx, "true", sandbox.Config{}); err == nil || !strings.Contains(err.Error(), "need the sandbox") {
		t.Fatalf("an unsandboxed registry ran a trial: %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := registry.RunTrial(canceled, "true", sandbox.Config{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("a canceled trial = %v, want context.Canceled", err)
	}
}
