package streaming

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/alexschlessinger/pollytool/messages"
	"go.uber.org/zap"
)

// StreamingCore provides common streaming functionality for all providers.
// It manages state accumulation, channel lifecycle, and coordinates with
// provider-specific adapters for custom handling.
type StreamingCore struct {
	state          *StreamState
	adapter        ProviderAdapter
	messageChannel chan messages.ChatMessage
	ctx            context.Context
}

// ProviderAdapter allows provider-specific handling while using common state.
// Each provider implements this to handle their unique streaming patterns.
// The adapter implementations are in the llm/adapters package.
type ProviderAdapter interface {
	// ProcessChunk handles provider-specific chunk processing
	// The chunk parameter type depends on the provider (e.g., OpenAI delta, Anthropic event)
	ProcessChunk(chunk any, state StreamStateInterface) error

	// EnrichFinalMessage allows provider to add custom metadata before sending final message
	// Used for things like Anthropic thinking blocks, Gemini signatures, etc.
	EnrichFinalMessage(msg *messages.ChatMessage, state StreamStateInterface)

	// HandleToolCall provides custom tool call handling if needed
	// Can be nil for providers that use standard handling
	HandleToolCall(toolData any, state StreamStateInterface) error
}

// NewStreamingCore creates a new streaming coordinator
func NewStreamingCore(
	ctx context.Context,
	messageChannel chan messages.ChatMessage,
	adapter ProviderAdapter,
) *StreamingCore {
	return &StreamingCore{
		state:          NewStreamState(),
		adapter:        adapter,
		messageChannel: messageChannel,
		ctx:            ctx,
	}
}

// GetState returns the current streaming state (for provider access)
func (sc *StreamingCore) GetState() *StreamState {
	return sc.state
}

// EmitContent sends a content chunk through the message channel
func (sc *StreamingCore) EmitContent(content string) {
	if content == "" {
		return
	}

	select {
	case <-sc.ctx.Done():
		return
	case sc.messageChannel <- messages.ChatMessage{
		Role:    messages.MessageRoleAssistant,
		Content: content,
	}:
		// Also accumulate in state
		sc.state.AppendContent(content)
	}
}

// EmitReasoning sends a reasoning/thinking chunk through the message channel
func (sc *StreamingCore) EmitReasoning(reasoning string) {
	if reasoning == "" {
		return
	}

	select {
	case <-sc.ctx.Done():
		return
	case sc.messageChannel <- messages.ChatMessage{
		Role:      messages.MessageRoleAssistant,
		Reasoning: reasoning,
	}:
		// Also accumulate in state
		sc.state.AppendReasoning(reasoning)
	}
}

// EmitError sends an error message through the channel
func (sc *StreamingCore) EmitError(err error) {
	select {
	case <-sc.ctx.Done():
		return
	case sc.messageChannel <- messages.ChatMessage{
		Role:    messages.MessageRoleAssistant,
		Content: fmt.Sprintf("Error: %v", err),
	}:
		zap.S().Debugw("streaming_error", "error", err)
	}
}

// ProcessChunk delegates chunk processing to the adapter
func (sc *StreamingCore) ProcessChunk(chunk any) error {
	if sc.adapter == nil {
		return fmt.Errorf("no adapter configured")
	}
	return sc.adapter.ProcessChunk(chunk, sc.state)
}

// Complete sends the final accumulated message with all metadata
func (sc *StreamingCore) Complete() {
	// Create the final message with accumulated state
	msg := messages.ChatMessage{
		Role:       messages.MessageRoleAssistant,
		Content:    "", // Always empty to avoid duplication (content was streamed)
		ToolCalls:  sc.state.ToolCalls,
		Reasoning:  "", // Already streamed, don't duplicate
		StopReason: sc.state.StopReason,
	}

	// Set token usage
	msg.SetTokenUsage(sc.state.InputTokens, sc.state.OutputTokens)

	// Let the adapter enrich with provider-specific metadata
	if sc.adapter != nil {
		sc.adapter.EnrichFinalMessage(&msg, sc.state)
	}

	// Send the final message
	select {
	case <-sc.ctx.Done():
		return
	case sc.messageChannel <- msg:
		sc.logCompletionDetails()
	}
}

// CompleteWithContent sends final message with explicit content
// Used for structured output responses that weren't streamed
func (sc *StreamingCore) CompleteWithContent(content string) {
	msg := messages.ChatMessage{
		Role:       messages.MessageRoleAssistant,
		Content:    content, // Explicit content for structured responses
		StopReason: sc.state.StopReason,
	}

	// Set token usage
	msg.SetTokenUsage(sc.state.InputTokens, sc.state.OutputTokens)

	// Let the adapter enrich if needed
	if sc.adapter != nil {
		sc.adapter.EnrichFinalMessage(&msg, sc.state)
	}

	select {
	case <-sc.ctx.Done():
		return
	case sc.messageChannel <- msg:
	}
}

// SetTokenUsage updates token counts in the state
func (sc *StreamingCore) SetTokenUsage(input, output int) {
	sc.state.SetTokenUsage(input, output)
}

// SetStopReason updates the stop reason in the state
func (sc *StreamingCore) SetStopReason(reason messages.StopReason) {
	sc.state.SetStopReason(reason)
}

// HandleStructuredOutput processes structured output tool calls (e.g., for JSON schemas)
func (sc *StreamingCore) HandleStructuredOutput(toolName string) bool {
	for _, tc := range sc.state.ToolCalls {
		if tc.Name == toolName {
			// Parse the arguments to extract the structured data
			var args map[string]any
			if err := parseJSON(tc.Arguments, &args); err == nil {
				if data, ok := args["data"]; ok {
					// Return just the structured data as content
					if dataJSON, err := marshalJSON(data); err == nil {
						sc.CompleteWithContent(string(dataJSON))
						return true
					}
				}
			}
		}
	}
	return false
}

// logCompletionDetails logs streaming completion information for debugging
func (sc *StreamingCore) logCompletionDetails() {
	state := sc.state.Clone()

	contentPreview := state.ResponseContent
	if len(contentPreview) > 200 {
		contentPreview = contentPreview[:200] + "..."
	}

	fields := []any{
		"content_preview", contentPreview,
		"content_length", len(state.ResponseContent),
	}

	if len(state.ToolCalls) > 0 {
		toolInfo := make([]string, len(state.ToolCalls))
		for i, tc := range state.ToolCalls {
			toolInfo[i] = tc.Name
		}
		fields = append(fields,
			"tool_call_count", len(state.ToolCalls),
			"tool_names", toolInfo,
		)
	}

	if state.ReasoningContent != "" {
		fields = append(fields, "reasoning_length", len(state.ReasoningContent))
	}

	if state.InputTokens > 0 || state.OutputTokens > 0 {
		fields = append(fields,
			"input_tokens", state.InputTokens,
			"output_tokens", state.OutputTokens,
		)
	}

	zap.S().Debugw("streaming_completed", fields...)
}

// Helper functions for JSON operations

func parseJSON(data string, v any) error {
	return json.Unmarshal([]byte(data), v)
}

func marshalJSON(v any) ([]byte, error) {
	return json.Marshal(v)
}
