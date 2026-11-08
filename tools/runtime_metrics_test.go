package tools

import (
	"context"
	"errors"
	"testing"

	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
)

func TestRuntimeStorageScaleToolObservations(t *testing.T) {
	t.Parallel()
	registry := NewToolRegistry(nil)
	t.Cleanup(func() { registry.Close() })
	r := runtimemetrics.New()
	ctx := runtimemetrics.WithRecorder(t.Context(), r)
	failure := errors.New("tool failed")
	calls := 0
	tool := &Func{Name: "observed", Run: func(context.Context, Args) (string, error) { calls++; return "partial", failure }}
	registry.Register(tool)
	result, err := registry.ExecuteTool(ctx, tool, nil, 0)
	if !errors.Is(err, failure) || !result.Invoked || result.Output.Text != "partial" {
		t.Fatalf("tool outcome: %+v %v", result, err)
	}
	s := r.Snapshot()
	if s.Count(runtimemetrics.ToolsStarted) != 1 || s.Count(runtimemetrics.ToolsCompleted) != 1 || s.Timing(runtimemetrics.ToolQueue).Count != 1 || s.Timing(runtimemetrics.ToolExecution).Count != 1 {
		t.Fatalf("dispatch/return observations: %+v", s)
	}
	if _, err := registry.ExecuteTool(runtimemetrics.WithRecorder(ctx, nil), tool, nil, 0); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if r.Snapshot() != s || calls != 2 {
		t.Fatal("disabled instrumentation changed call or counters")
	}

	gate := NewExecutionGate()
	registry.SetExecutionGate(gate)
	release, err := gate.Exclusive(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	result, err = registry.ExecuteTool(canceled, tool, nil, 0)
	if !errors.Is(err, context.Canceled) || result.Invoked || result.ContextErr != context.Canceled || calls != 2 {
		t.Fatalf("canceled gate invoked tool: %+v %v", result, err)
	}
	after := r.Snapshot()
	if after.Timing(runtimemetrics.ToolQueue).Count != 2 || after.Timing(runtimemetrics.ToolExecution).Count != 1 || after.Count(runtimemetrics.ToolsStarted) != 1 || after.Count(runtimemetrics.ToolsCompleted) != 1 {
		t.Fatal("canceled gate counted a dispatched call")
	}
	t.Log("tool dispatches/normal returns include errors; they do not count successful effects or useful completed work")
}

func TestRuntimeStorageScaleToolPanicObservations(t *testing.T) {
	t.Parallel()
	registry := NewToolRegistry(nil)
	t.Cleanup(func() { registry.Close() })
	r := runtimemetrics.New()
	ctx := runtimemetrics.WithRecorder(t.Context(), r)
	tool := &Func{Name: "panic", Run: func(context.Context, Args) (string, error) { panic("sentinel") }}
	registry.Register(tool)
	func() {
		defer func() {
			if got := recover(); got != "sentinel" {
				t.Fatalf("panic changed: %v", got)
			}
		}()
		_, _ = registry.ExecuteTool(ctx, tool, nil, 0)
		t.Fatal("tool panic suppressed")
	}()
	s := r.Snapshot()
	if s.Count(runtimemetrics.ToolsStarted) != 1 || s.Count(runtimemetrics.ToolsCompleted) != 0 || s.Timing(runtimemetrics.ToolExecution).Count != 1 {
		t.Fatalf("panic counted a completed return or lost timing: %+v", s)
	}
	// The existing gate cleanup still executes when dispatch panics.
	release, err := registry.BeginEnvironmentMaintenance()
	if err != nil {
		t.Fatal(err)
	}
	release()
}
