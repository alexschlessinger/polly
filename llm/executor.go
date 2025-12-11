package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
)

// ExecutionHooks provides callbacks for customizing tool execution
type ExecutionHooks struct {
	// BeforeExecute is called before each tool executes.
	// Returns a (possibly modified) context to pass to the tool.
	// If nil, context passes through unchanged.
	BeforeExecute func(ctx context.Context, toolCall messages.ChatMessageToolCall, args map[string]any) context.Context

	// AfterExecute is called after each tool executes with timing info.
	// Receives the result string and any error that occurred.
	AfterExecute func(toolCall messages.ChatMessageToolCall, result string, duration time.Duration, err error)

	// OnParseError is called when tool arguments fail to parse.
	// Returns the error message to use. If nil, uses default message.
	OnParseError func(toolCall messages.ChatMessageToolCall, err error) string

	// OnToolNotFound is called when a tool isn't in the registry.
	// Returns the error message to use. If nil, uses default message.
	OnToolNotFound func(toolCall messages.ChatMessageToolCall) string
}

// ToolExecutor handles tool execution with customizable hooks
type ToolExecutor struct {
	Registry *tools.ToolRegistry
	Hooks    *ExecutionHooks
	Timeout  time.Duration // Default timeout for tool execution
}

// NewToolExecutor creates a new executor with the given registry
func NewToolExecutor(registry *tools.ToolRegistry) *ToolExecutor {
	return &ToolExecutor{Registry: registry}
}

// WithHooks sets execution hooks and returns the executor for chaining
func (e *ToolExecutor) WithHooks(hooks *ExecutionHooks) *ToolExecutor {
	e.Hooks = hooks
	return e
}

// WithTimeout sets default timeout and returns the executor for chaining
func (e *ToolExecutor) WithTimeout(timeout time.Duration) *ToolExecutor {
	e.Timeout = timeout
	return e
}

// ExecuteToolCall executes a single tool call and adds result to session.
// Returns true if successful, false on error.
func (e *ToolExecutor) ExecuteToolCall(
	ctx context.Context,
	toolCall messages.ChatMessageToolCall,
	session sessions.Session,
) bool {
	// Parse arguments from JSON string
	var args map[string]any
	if err := json.Unmarshal([]byte(toolCall.Arguments), &args); err != nil {
		errMsg := fmt.Sprintf("Error parsing arguments: %v", err)
		if e.Hooks != nil && e.Hooks.OnParseError != nil {
			errMsg = e.Hooks.OnParseError(toolCall, err)
		}
		session.AddMessage(messages.ChatMessage{
			Role:       messages.MessageRoleTool,
			Content:    errMsg,
			ToolCallID: toolCall.ID,
			ToolName:   toolCall.Name,
		})
		return false
	}

	// Get tool from registry
	tool, exists := e.Registry.Get(toolCall.Name)
	if !exists {
		errMsg := fmt.Sprintf("Tool not found: %s", toolCall.Name)
		if e.Hooks != nil && e.Hooks.OnToolNotFound != nil {
			errMsg = e.Hooks.OnToolNotFound(toolCall)
		}
		session.AddMessage(messages.ChatMessage{
			Role:       messages.MessageRoleTool,
			Content:    errMsg,
			ToolCallID: toolCall.ID,
			ToolName:   toolCall.Name,
		})
		return false
	}

	// Pre-execution hook - allows modifying context
	execCtx := ctx
	if e.Hooks != nil && e.Hooks.BeforeExecute != nil {
		execCtx = e.Hooks.BeforeExecute(ctx, toolCall, args)
	}

	// Apply timeout if set
	if e.Timeout > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(execCtx, e.Timeout)
		defer cancel()
	}

	// Execute tool with timing
	startTime := time.Now()
	result, err := tool.Execute(execCtx, args)
	duration := time.Since(startTime)

	success := err == nil
	if err != nil {
		if execCtx.Err() == context.DeadlineExceeded {
			result = fmt.Sprintf("Error: tool execution timed out after %v", e.Timeout)
		} else {
			result = fmt.Sprintf("Error: %v", err)
		}
	}

	// Post-execution hook
	if e.Hooks != nil && e.Hooks.AfterExecute != nil {
		e.Hooks.AfterExecute(toolCall, result, duration, err)
	}

	// Add tool result to session
	session.AddMessage(messages.ChatMessage{
		Role:       messages.MessageRoleTool,
		Content:    result,
		ToolCallID: toolCall.ID,
		ToolName:   toolCall.Name,
	})

	return success
}

// ExecuteAll executes all tool calls and adds results to session
func (e *ToolExecutor) ExecuteAll(
	ctx context.Context,
	toolCalls []messages.ChatMessageToolCall,
	session sessions.Session,
) {
	for _, tc := range toolCalls {
		e.ExecuteToolCall(ctx, tc, session)
	}
}
