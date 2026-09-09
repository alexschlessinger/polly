package tools

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestExecutionGateWritersCancellationAndCoordination(t *testing.T) {
	gate := NewExecutionGate()
	r := NewToolRegistry(nil, WithUnsafeNoSandbox())
	t.Cleanup(func() { r.Close() })
	r.SetExecutionGate(gate)
	ctx := context.Background()
	first, err := gate.Shared(ctx)
	if err != nil {
		t.Fatal(err)
	}
	second, err := gate.Shared(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wait, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, err := gate.Exclusive(wait); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("exclusive passed readers: %v", err)
	}
	first()
	second()
	write, err := gate.Exclusive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range []Tool{
		&Func{Name: "ordinary"}, &Func{Name: "long_writer", LongRunning: true},
		&NamespacedTool{Tool: &Func{Name: "mcp_writer", LongRunning: true}},
	} {
		wait, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
		_, err := r.GuardExecution(wait, tool)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("%s bypassed writer: %v", tool.GetName(), err)
		}
	}
	for _, tool := range []Tool{&Func{Coordinator: true}, &NamespacedTool{Tool: &Func{Coordinator: true}}} {
		wait, cancel := context.WithTimeout(ctx, time.Second)
		release, err := r.GuardExecution(wait, tool)
		if err != nil {
			t.Fatal(err)
		}
		release()
		cancel()
	}
	write()
	last, err := gate.Shared(ctx)
	if err != nil {
		t.Fatal(err)
	}
	last()
}
