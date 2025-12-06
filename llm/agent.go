package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
	"golang.org/x/sync/errgroup"
)

// Agent handles the agentic loop without owning session state.
// It executes completions with automatic tool call handling.
type Agent struct {
	client LLM
	tools  *tools.ToolRegistry
	config AgentConfig
}

// AgentConfig configures agent behavior
type AgentConfig struct {
	MaxIterations    int           // Maximum LLM calls before giving up (default: 10)
	ToolTimeout      time.Duration // Per-tool execution timeout (0 = no timeout)
	MaxParallelTools int           // Maximum parallel tool executions (0 = unlimited)
}

// AgentCallbacks provides hooks for observing and customizing agent execution
type AgentCallbacks struct {
	// OnReasoning is called when reasoning/thinking content is streamed
	OnReasoning func(content string)

	// OnContent is called when regular content is streamed
	OnContent func(content string)

	// BeforeToolExecute is called before each tool executes.
	// Returns a (possibly modified) context to pass to the tool.
	// Use this to inject context values that tools need (e.g., IRC context).
	// If nil, context passes through unchanged.
	BeforeToolExecute func(ctx context.Context, call messages.ChatMessageToolCall, args map[string]any) context.Context

	// OnToolStart is called before each tool executes (after BeforeToolExecute)
	OnToolStart func(call messages.ChatMessageToolCall)

	// OnToolEnd is called after each tool executes
	OnToolEnd func(call messages.ChatMessageToolCall, result string, duration time.Duration, err error)

	// OnComplete is called when the final response is ready (no more tool calls)
	OnComplete func(response *messages.ChatMessage)

	// OnError is called when an error occurs
	OnError func(err error)
}

// AgentResponse contains the results after Run completes
type AgentResponse struct {
	Message        *messages.ChatMessage  // Final assistant message (no tool calls)
	AllMessages    []messages.ChatMessage // All messages generated (assistant + tool results)
	IterationCount int                    // Number of LLM calls made
}

// NewAgent creates a stateless agent that handles the agentic loop.
// The agent does not own session state - callers provide messages and
// receive back all generated messages to add to their own session.
func NewAgent(client LLM, registry *tools.ToolRegistry, config AgentConfig) *Agent {
	if config.MaxIterations <= 0 {
		config.MaxIterations = 10
	}
	return &Agent{
		client: client,
		tools:  registry,
		config: config,
	}
}

// Run executes a completion with automatic tool call handling.
// It loops until the LLM returns a response with no tool calls,
// or until MaxIterations is reached.
//
// The caller provides messages in req.Messages and receives back
// all generated messages (assistant responses + tool results) in
// AgentResponse.AllMessages. The caller is responsible for adding
// these to their session.
func (a *Agent) Run(ctx context.Context, req *CompletionRequest, cb *AgentCallbacks) (*AgentResponse, error) {
	// Work with a copy of messages - don't mutate input
	msgs := make([]messages.ChatMessage, len(req.Messages))
	copy(msgs, req.Messages)

	var allGenerated []messages.ChatMessage

	for iteration := 0; iteration < a.config.MaxIterations; iteration++ {
		// Check for context cancellation
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		// Build request with accumulated messages
		iterReq := *req
		iterReq.Messages = msgs
		if a.tools != nil {
			iterReq.Tools = a.tools.All()
		}

		// Stream completion
		processor := messages.NewStreamProcessor()
		events := a.client.ChatCompletionStream(ctx, &iterReq, processor)

		// Process events
		response, err := a.processEvents(ctx, events, cb)
		if err != nil {
			return nil, err
		}

		// Track the assistant response
		msgs = append(msgs, *response)
		allGenerated = append(allGenerated, *response)

		// Check stop reason to determine next action
		switch response.StopReason {
		case messages.StopReasonEndTurn:
			// Normal completion
			if cb != nil && cb.OnComplete != nil {
				cb.OnComplete(response)
			}
			return &AgentResponse{
				Message:        response,
				AllMessages:    allGenerated,
				IterationCount: iteration + 1,
			}, nil

		case messages.StopReasonMaxTokens:
			// Response truncated - warn and return
			log.Printf("agent: response truncated due to max_tokens")
			if cb != nil && cb.OnComplete != nil {
				cb.OnComplete(response)
			}
			return &AgentResponse{
				Message:        response,
				AllMessages:    allGenerated,
				IterationCount: iteration + 1,
			}, nil

		case messages.StopReasonContentFilter:
			// Response blocked by safety/policy
			err := errors.New("response blocked by content filter")
			if cb != nil && cb.OnError != nil {
				cb.OnError(err)
			}
			return nil, err

		case messages.StopReasonError:
			// Model produced malformed output
			err := errors.New("model produced malformed output")
			if cb != nil && cb.OnError != nil {
				cb.OnError(err)
			}
			return nil, err

		case messages.StopReasonToolUse:
			// Continue to execute tool calls below

		default:
			// Unknown stop reason with no tool calls = treat as completion
			if len(response.ToolCalls) == 0 {
				if cb != nil && cb.OnComplete != nil {
					cb.OnComplete(response)
				}
				return &AgentResponse{
					Message:        response,
					AllMessages:    allGenerated,
					IterationCount: iteration + 1,
				}, nil
			}
			// Has tool calls, continue to execute them
		}

		// Execute tool calls in parallel
		toolMsgs, err := a.executeToolsParallel(ctx, response.ToolCalls, cb)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, toolMsgs...)
		allGenerated = append(allGenerated, toolMsgs...)
	}

	err := errors.New("max iterations exceeded")
	if cb != nil && cb.OnError != nil {
		cb.OnError(err)
	}
	// Return the partial response so the caller can save the history
	return &AgentResponse{
		Message:        &msgs[len(msgs)-1], // Last message
		AllMessages:    allGenerated,
		IterationCount: a.config.MaxIterations,
	}, err
}

// processEvents processes the event stream and returns the final message
func (a *Agent) processEvents(ctx context.Context, events <-chan *messages.StreamEvent, cb *AgentCallbacks) (*messages.ChatMessage, error) {
	var response *messages.ChatMessage

	for event := range events {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		switch event.Type {
		case messages.EventTypeReasoning:
			if cb != nil && cb.OnReasoning != nil {
				cb.OnReasoning(event.Content)
			}
		case messages.EventTypeContent:
			if cb != nil && cb.OnContent != nil {
				cb.OnContent(event.Content)
			}
		case messages.EventTypeComplete:
			response = event.Message
		case messages.EventTypeError:
			if cb != nil && cb.OnError != nil {
				cb.OnError(event.Error)
			}
			return nil, event.Error
		}
	}

	if response == nil {
		return nil, errors.New("no response received from LLM")
	}

	return response, nil
}

// executeTool executes a single tool call and returns the result message
func (a *Agent) executeTool(ctx context.Context, tc messages.ChatMessageToolCall, cb *AgentCallbacks) messages.ChatMessage {
	// Parse args early so we can pass them to BeforeToolExecute
	var args map[string]any
	if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
		args = nil // Will be handled in executeToolCall
	}

	// Allow callback to modify context (e.g., inject IRC context)
	execCtx := ctx
	if cb != nil && cb.BeforeToolExecute != nil {
		execCtx = cb.BeforeToolExecute(ctx, tc, args)
	}

	if cb != nil && cb.OnToolStart != nil {
		cb.OnToolStart(tc)
	}

	start := time.Now()
	result, err := a.executeToolCall(execCtx, tc, args)
	duration := time.Since(start)

	if cb != nil && cb.OnToolEnd != nil {
		cb.OnToolEnd(tc, result, duration, err)
	}

	return messages.ChatMessage{
		Role:       messages.MessageRoleTool,
		Content:    result,
		ToolCallID: tc.ID,
	}
}

// executeToolCall performs the actual tool execution
func (a *Agent) executeToolCall(ctx context.Context, tc messages.ChatMessageToolCall, args map[string]any) (string, error) {
	// Apply timeout
	if a.config.ToolTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, a.config.ToolTimeout)
		defer cancel()
	}

	// Parse args if not already parsed
	if args == nil {
		if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
			errMsg := fmt.Sprintf("Error parsing arguments: %v", err)
			return errMsg, err
		}
	}

	// Get tool from registry
	if a.tools == nil {
		errMsg := fmt.Sprintf("Tool not found: %s (no registry)", tc.Name)
		return errMsg, errors.New("no tool registry")
	}

	tool, exists := a.tools.Get(tc.Name)
	if !exists {
		errMsg := fmt.Sprintf("Tool not found: %s", tc.Name)
		return errMsg, errors.New("tool not found: " + tc.Name)
	}

	// Execute
	result, err := tool.Execute(ctx, args)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Sprintf("Error: tool execution timed out after %v", a.config.ToolTimeout), err
		}
		return fmt.Sprintf("Error: %v", err), err
	}

	return result, nil
}

// executeToolsParallel executes multiple tool calls concurrently and returns results in order.
// If context is cancelled, all running tools are notified via their context.
func (a *Agent) executeToolsParallel(ctx context.Context, toolCalls []messages.ChatMessageToolCall, cb *AgentCallbacks) ([]messages.ChatMessage, error) {
	results := make([]messages.ChatMessage, len(toolCalls))

	g, ctx := errgroup.WithContext(ctx)

	// Semaphore for concurrency limiting
	sem := make(chan struct{}, a.effectiveParallelism(len(toolCalls)))

	for i, tc := range toolCalls {
		i, tc := i, tc // capture loop vars
		g.Go(func() error {
			// Acquire semaphore (respects context cancellation)
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return ctx.Err()
			}

			results[i] = a.executeTool(ctx, tc, cb)
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return results, err // Return partial results + error
	}
	return results, nil
}

// effectiveParallelism returns the concurrency limit based on config and number of tools.
func (a *Agent) effectiveParallelism(n int) int {
	if a.config.MaxParallelTools <= 0 || a.config.MaxParallelTools > n {
		return n
	}
	return a.config.MaxParallelTools
}
