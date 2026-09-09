package tools

import (
	"context"
	"time"
)

// ToolExecution retains the raw output and execution context's outcome so
// callers can report failures in their own format without losing media or data.
type ToolExecution struct {
	Output     ToolOutput
	Invoked    bool // false when cancellation or the execution gate prevents invocation
	ContextErr error
}

// ExecuteTool invokes a tool already resolved and approved by the caller. It
// owns the per-tool timeout, execution gate, and rich-output dispatch. The
// returned error is the tool's original error (or an error acquiring the gate);
// ContextErr separately reports cancellation or timeout, including when a tool
// returns successfully after its context ends. Panics propagate after releasing
// the gate and timeout resources.
func (r *ToolRegistry) ExecuteTool(ctx context.Context, tool Tool, args map[string]any, timeout time.Duration) (result ToolExecution, err error) {
	if untimed, ok := tool.(UntimedTool); timeout > 0 && !(ok && untimed.Untimed()) {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	// Observe the context before our own deferred cancellation.
	defer func() { result.ContextErr = ctx.Err() }()
	release, err := r.GuardExecution(ctx, tool)
	if err != nil {
		return result, err
	}
	defer release()
	result.Invoked = true
	if rich, ok := tool.(OutputTool); ok {
		result.Output, err = rich.ExecuteOutput(ctx, args)
	} else {
		result.Output.Text, err = tool.Execute(ctx, args)
	}
	return result, err
}
