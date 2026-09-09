package tools

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

type executionOutputTool struct {
	*Func
	run func(context.Context) (ToolOutput, error)
}

func (t executionOutputTool) ExecuteOutput(ctx context.Context, _ map[string]any) (ToolOutput, error) {
	return t.run(ctx)
}

func TestExecuteToolRetainsRawRichOutcome(t *testing.T) {
	r := NewToolRegistry(nil, WithUnsafeNoSandbox())
	defer r.Close()
	want := ToolOutput{Text: "partial output", Data: map[string]any{"count": 2}, Media: []ToolMedia{{Name: "result", MIMEType: "application/octet-stream", Data: []byte{0, 1, 2}}}}
	failure := NewToolError("failed with evidence", "test_failure")
	tool := executionOutputTool{Func: &Func{Name: "rich", Run: func(context.Context, Args) (string, error) {
		t.Fatal("rich output dispatched through the text interface")
		return "", nil
	}}, run: func(context.Context) (ToolOutput, error) { return want, failure }}
	result, err := r.ExecuteTool(context.Background(), &NamespacedTool{Tool: tool, namespacedName: "test__rich"}, nil, time.Second)
	if err != failure || !result.Invoked || result.ContextErr != nil || !reflect.DeepEqual(result.Output, want) {
		t.Fatalf("raw outcome changed: %+v %v", result, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tool.run = func(context.Context) (ToolOutput, error) { cancel(); return want, nil }
	result, err = r.ExecuteTool(ctx, tool, nil, time.Second)
	if err != nil || !result.Invoked || result.ContextErr != context.Canceled || !reflect.DeepEqual(result.Output, want) {
		t.Fatalf("lost completed output or cancellation: %+v %v", result, err)
	}
}

func TestExecuteToolTimeoutAndUntimed(t *testing.T) {
	r := NewToolRegistry(nil, WithUnsafeNoSandbox())
	defer r.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	tool := &Func{Name: "wait", Run: func(ctx context.Context, _ Args) (string, error) {
		<-ctx.Done()
		return "partial", ctx.Err()
	}}
	result, err := r.ExecuteTool(ctx, tool, nil, 10*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) || result.ContextErr != context.DeadlineExceeded || result.Output.Text != "partial" {
		t.Fatalf("timeout lost: %+v %v", result, err)
	}
	tool.LongRunning = true
	tool.Run = func(callCtx context.Context, _ Args) (string, error) {
		got, _ := callCtx.Deadline()
		want, _ := ctx.Deadline()
		if !got.Equal(want) {
			t.Fatalf("untimed tool inherited per-tool deadline: %v != %v", got, want)
		}
		return "complete", nil
	}
	result, err = r.ExecuteTool(ctx, &NamespacedTool{Tool: tool, namespacedName: "test__wait"}, nil, 10*time.Millisecond)
	if err != nil || result.ContextErr != nil || result.Output.Text != "complete" {
		t.Fatalf("untimed outcome: %+v %v", result, err)
	}
}

func TestExecuteToolCanceledGateDoesNotInvoke(t *testing.T) {
	r := NewToolRegistry(nil, WithUnsafeNoSandbox())
	defer r.Close()
	gate := NewExecutionGate()
	r.SetExecutionGate(gate)
	release, err := gate.Exclusive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	called := false
	tool := &Func{Name: "writer", LongRunning: true, Run: func(context.Context, Args) (string, error) { called = true; return "written", nil }}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	result, err := r.ExecuteTool(ctx, tool, nil, time.Second)
	release()
	if called || result.Invoked || !errors.Is(err, context.DeadlineExceeded) || result.ContextErr != context.DeadlineExceeded {
		t.Fatalf("canceled writer invoked: %+v %v", result, err)
	}
	retryCtx, retryCancel := context.WithTimeout(context.Background(), time.Second)
	defer retryCancel()
	result, err = r.ExecuteTool(retryCtx, tool, nil, time.Second)
	if err != nil || !called || !result.Invoked || result.ContextErr != nil || result.Output.Text != "written" {
		t.Fatalf("gate unavailable after canceled wait: %+v %v", result, err)
	}
}
