package tools

import (
	"context"
	"errors"
	"reflect"
	"strings"
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
	r := NewToolRegistry(nil, WithNativeTools(), WithUnsafeNoSandbox())
	defer r.Close()
	want := ToolOutput{Text: "partial output", Data: map[string]any{"count": 2}, Media: []ToolMedia{{Name: "result", MIMEType: "application/octet-stream", Data: []byte{0, 1, 2}}}}
	failure := NewToolError("failed with evidence", "test_failure")
	tool := executionOutputTool{Func: &Func{Name: "rich", Run: func(context.Context, Args) (string, error) {
		t.Fatal("rich output dispatched through the text interface")
		return "", nil
	}}, run: func(context.Context) (ToolOutput, error) { return want, failure }}
	namespaced := &NamespacedTool{Tool: tool, namespacedName: "test__rich"}
	r.Register(namespaced)
	result, err := r.ExecuteTool(context.Background(), namespaced, nil, time.Second)
	if err != failure || !result.Invoked || result.ContextErr != nil || !reflect.DeepEqual(result.Output, want) {
		t.Fatalf("raw outcome changed: %+v %v", result, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tool.run = func(context.Context) (ToolOutput, error) { cancel(); return want, nil }
	r.Register(tool)
	result, err = r.ExecuteTool(ctx, tool, nil, time.Second)
	if err != nil || !result.Invoked || result.ContextErr != context.Canceled || !reflect.DeepEqual(result.Output, want) {
		t.Fatalf("lost completed output or cancellation: %+v %v", result, err)
	}
}

func TestExecuteToolTimeoutAndUntimed(t *testing.T) {
	r := NewToolRegistry(nil, WithNativeTools(), WithUnsafeNoSandbox())
	defer r.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	tool := &Func{Name: "wait", Run: func(ctx context.Context, _ Args) (string, error) {
		<-ctx.Done()
		return "partial", ctx.Err()
	}}
	r.Register(tool)
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
	namespaced := &NamespacedTool{Tool: tool, namespacedName: "test__wait"}
	r.Register(namespaced)
	result, err = r.ExecuteTool(ctx, namespaced, nil, 10*time.Millisecond)
	if err != nil || result.ContextErr != nil || result.Output.Text != "complete" {
		t.Fatalf("untimed outcome: %+v %v", result, err)
	}
}

func TestExecuteToolCanceledGateDoesNotInvoke(t *testing.T) {
	r := NewToolRegistry(nil, WithNativeTools(), WithUnsafeNoSandbox())
	defer r.Close()
	gate := NewExecutionGate()
	r.SetExecutionGate(gate)
	release, err := gate.Exclusive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	called := false
	tool := &Func{Name: "writer", LongRunning: true, Run: func(context.Context, Args) (string, error) { called = true; return "written", nil }}
	r.Register(tool)
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

func TestExecuteToolRejectsForeignAndReplacedHandles(t *testing.T) {
	r := NewToolRegistry(nil)
	defer r.Close()
	other := NewToolRegistry(nil)
	defer other.Close()
	foreign := &Func{Name: "shared", Run: func(context.Context, Args) (string, error) { return "foreign", nil }}
	other.Register(foreign)
	result, err := r.ExecuteTool(context.Background(), foreign, nil, time.Second)
	if err == nil || !strings.Contains(err.Error(), "no longer registered") || result.Invoked {
		t.Fatalf("foreign handle ran: %+v %v", result, err)
	}

	ran := ""
	first := &Func{Name: "shared", Run: func(context.Context, Args) (string, error) { ran = "first"; return "first", nil }}
	r.Register(first)
	handle, ok := r.Get("shared")
	if !ok {
		t.Fatal("resolve")
	}
	second := &Func{Name: "shared", Run: func(context.Context, Args) (string, error) { ran = "second"; return "second", nil }}
	r.Register(second)
	result, err = r.ExecuteTool(context.Background(), handle, nil, time.Second)
	if err == nil || !strings.Contains(err.Error(), "replaced") || result.Invoked || ran != "" {
		t.Fatalf("replaced handle ran %q: %+v %v", ran, result, err)
	}
	result, err = r.ExecuteTool(context.Background(), second, nil, time.Second)
	if err != nil || !result.Invoked || result.Output.Text != "second" || ran != "second" {
		t.Fatalf("current handle: %+v %v", result, err)
	}
}

func TestExecuteToolRechecksAllowanceAfterGate(t *testing.T) {
	r := NewToolRegistry(nil)
	defer r.Close()
	ran := false
	tool := &Func{Name: "gated", Run: func(context.Context, Args) (string, error) { ran = true; return "ran", nil }}
	r.Register(tool)
	gate := NewExecutionGate()
	r.SetExecutionGate(gate)
	release, err := gate.Exclusive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var result ToolExecution
	var execErr error
	go func() {
		defer close(done)
		result, execErr = r.ExecuteTool(context.Background(), tool, nil, time.Second)
	}()
	// The call is waiting on the gate; a skill policy that excludes the tool
	// commits meanwhile.
	time.Sleep(50 * time.Millisecond)
	r.stageSkillAllowance([]string{"something_else"}, nil)
	r.CommitPendingChanges()
	release()
	<-done
	if execErr == nil || !strings.Contains(execErr.Error(), "no longer allowed") || result.Invoked || ran {
		t.Fatalf("disallowed handle ran after the gate: %+v %v", result, execErr)
	}
}
