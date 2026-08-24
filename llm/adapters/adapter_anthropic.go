package adapters

import (
	"github.com/alexschlessinger/pollytool/llm/anthropic"
	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
)

// AnthropicAdapter handles Anthropic-specific streaming patterns.
// Anthropic uses event-based streaming with thinking blocks and structured events.
type AnthropicAdapter struct {
	currentBlockType     string
	currentBlockIndex    int
	currentThinkingBlock map[string]any
	thinkingBlocks       []map[string]any
}

// NewAnthropicAdapter creates a new Anthropic streaming adapter
func NewAnthropicAdapter() *AnthropicAdapter {
	return &AnthropicAdapter{
		thinkingBlocks: make([]map[string]any, 0),
	}
}

// ProcessChunk handles Anthropic streaming events
func (a *AnthropicAdapter) ProcessChunk(chunk any, state streaming.StreamStateInterface) error {
	event, ok := chunk.(*anthropic.StreamEvent)
	if !ok {
		return nil
	}

	switch event.Type {
	case anthropic.EventMessageStart:
		// Message started - capture input tokens
		if event.Message != nil && event.Message.Usage != nil {
			state.SetTokenUsage(int(event.Message.Usage.InputTokens), state.GetOutputTokens())
		}

	case anthropic.EventContentBlockStart:
		a.handleContentBlockStart(event, state)

	case anthropic.EventContentBlockDelta:
		a.handleContentBlockDelta(event, state)

	case anthropic.EventContentBlockStop:
		a.handleContentBlockStop(state)

	case anthropic.EventMessageDelta:
		// Message delta contains stop_reason and usage stats
		if event.Delta != nil {
			state.SetStopReason(MapAnthropicStopReason(event.Delta.StopReason))
		}
		if event.Usage != nil {
			state.SetTokenUsage(state.GetInputTokens(), int(event.Usage.OutputTokens))
		}

	case anthropic.EventMessageStop:
		// Message complete - nothing to do here
	}

	return nil
}

// handleContentBlockStart processes content block start events
func (a *AnthropicAdapter) handleContentBlockStart(event *anthropic.StreamEvent, state streaming.StreamStateInterface) {
	if event.ContentBlock == nil {
		return
	}
	a.currentBlockType = event.ContentBlock.Type

	switch event.ContentBlock.Type {
	case "thinking":
		// Start capturing a thinking block
		a.currentThinkingBlock = map[string]any{
			"type":     "thinking",
			"thinking": "", // Will be filled by deltas
		}

	case "redacted_thinking":
		// Redacted thinking arrives complete in the start event (no
		// deltas); preserve it verbatim — it must be replayed unchanged
		// during tool loops.
		if event.ContentBlock.Data != "" {
			a.AddRedactedThinkingBlock(event.ContentBlock.Data)
		}

	case "tool_use":
		// Initialize a new tool call
		state.AddToolCall(messages.ChatMessageToolCall{
			ID:        event.ContentBlock.ID,
			Name:      event.ContentBlock.Name,
			Arguments: "{}", // Default to empty JSON object
		})
		toolCalls := state.GetToolCalls()
		a.currentBlockIndex = len(toolCalls) - 1
	}
}

// handleContentBlockDelta processes content block delta events
func (a *AnthropicAdapter) handleContentBlockDelta(event *anthropic.StreamEvent, state streaming.StreamStateInterface) {
	if event.Delta == nil {
		return
	}

	// Check for thinking delta
	if thinking := event.Delta.Thinking; thinking != "" {
		// Add to current thinking block if we're capturing one
		if a.currentThinkingBlock != nil {
			if existingThinking, ok := a.currentThinkingBlock["thinking"].(string); ok {
				a.currentThinkingBlock["thinking"] = existingThinking + thinking
			} else {
				a.currentThinkingBlock["thinking"] = thinking
			}
		}
		// Note: Reasoning emission is handled by the main streaming loop
	}

	// Check for signature delta (comes after thinking content)
	if signature := event.Delta.Signature; signature != "" {
		if a.currentThinkingBlock != nil {
			a.currentThinkingBlock["signature"] = signature
		}
	}

	// Check for text delta (regular content)
	// Note: Content emission is handled by the main streaming loop

	// Check if it's tool use input delta
	if event.Delta.PartialJSON != "" && a.currentBlockType == "tool_use" {
		// Update the last tool call's arguments
		toolCalls := state.GetToolCalls()
		if a.currentBlockIndex >= 0 && a.currentBlockIndex < len(toolCalls) {
			state.UpdateToolCallAtIndex(a.currentBlockIndex, func(tc *messages.ChatMessageToolCall) {
				if tc.Arguments == "{}" {
					// First content, replace the default empty object
					tc.Arguments = event.Delta.PartialJSON
				} else {
					// Append to existing content
					tc.Arguments += event.Delta.PartialJSON
				}
			})
		}
	}
}

// handleContentBlockStop processes content block stop events
func (a *AnthropicAdapter) handleContentBlockStop(state streaming.StreamStateInterface) {
	if a.currentBlockType == "thinking" && a.currentThinkingBlock != nil {
		// Save completed thinking block
		a.thinkingBlocks = append(a.thinkingBlocks, a.currentThinkingBlock)
		a.currentThinkingBlock = nil
	}
	a.currentBlockType = ""
	a.currentBlockIndex = -1
}

// EnrichFinalMessage adds Anthropic-specific metadata to the final message
func (a *AnthropicAdapter) EnrichFinalMessage(msg *messages.ChatMessage, state streaming.StreamStateInterface) {
	// Add thinking blocks to metadata
	if len(a.thinkingBlocks) > 0 {
		if msg.Metadata == nil {
			msg.Metadata = make(map[string]any)
		}
		msg.Metadata["anthropic_thinking_blocks"] = a.thinkingBlocks
	}
}

// HandleToolCall provides Anthropic-specific tool call handling
func (a *AnthropicAdapter) HandleToolCall(toolData any, state streaming.StreamStateInterface) error {
	// Tool calls are handled in ProcessChunk for Anthropic
	return nil
}

// AddThinkingBlock adds a thinking block for non-streaming responses
func (a *AnthropicAdapter) AddThinkingBlock(thinking, signature string) {
	a.thinkingBlocks = append(a.thinkingBlocks, map[string]any{
		"type":      "thinking",
		"thinking":  thinking,
		"signature": signature,
	})
}

// AddRedactedThinkingBlock preserves a redacted thinking block so it can be
// replayed unchanged
func (a *AnthropicAdapter) AddRedactedThinkingBlock(data string) {
	a.thinkingBlocks = append(a.thinkingBlocks, map[string]any{
		"type": "redacted_thinking",
		"data": data,
	})
}

// MapAnthropicStopReason converts Anthropic's stop reason to our normalized type
func MapAnthropicStopReason(sr anthropic.StopReason) messages.StopReason {
	switch sr {
	case "end_turn":
		return messages.StopReasonEndTurn
	case "tool_use":
		return messages.StopReasonToolUse
	case "max_tokens":
		return messages.StopReasonMaxTokens
	case "refusal":
		return messages.StopReasonContentFilter
	case "stop_sequence":
		return messages.StopReasonEndTurn
	default:
		return messages.StopReasonEndTurn
	}
}
