package tools

import (
	"context"
	"fmt"
	"reflect"
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
// owns the per-tool timeout, execution gate, and rich-output dispatch. After
// any wait on the gate it confirms the handle is still the registered,
// allowed tool for its name and never substitutes another; a replaced,
// foreign, or newly disallowed handle is not invoked. The returned error is
// the tool's original error (or an error acquiring the gate or checking the
// handle); ContextErr separately reports cancellation or timeout, including
// when a tool returns successfully after its context ends. Panics propagate
// after releasing the gate and timeout resources.
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
	if err := r.checkHandle(tool); err != nil {
		return result, err
	}
	result.Invoked = true
	if rich, ok := tool.(OutputTool); ok {
		result.Output, err = rich.ExecuteOutput(ctx, args)
	} else {
		result.Output.Text, err = tool.Execute(ctx, args)
	}
	return result, err
}

// checkHandle confirms that tool is the handle this registry currently
// serves under its name: registered, allowed by the active policy, and the
// same object the caller resolved. A tool type that cannot be compared is
// matched by type instead.
func (r *ToolRegistry) checkHandle(tool Tool) error {
	name := registeredName(tool)
	current, exists, allowed := r.GetIfAllowed(name)
	switch {
	case !exists:
		return fmt.Errorf("tool %q is no longer registered", name)
	case !allowed:
		return fmt.Errorf("tool %q is no longer allowed by the active policy", name)
	case !sameTool(current, tool):
		return fmt.Errorf("tool %q was replaced before execution", name)
	}
	return nil
}

func sameTool(current, handle Tool) bool {
	currentType, handleType := reflect.TypeOf(current), reflect.TypeOf(handle)
	if currentType != handleType {
		return false
	}
	if currentType.Comparable() {
		return current == handle
	}
	return true
}
