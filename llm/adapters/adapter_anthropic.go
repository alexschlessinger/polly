package adapters

import (
	"encoding/json"

	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"
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
	event, ok := chunk.(anthropic.MessageStreamEventUnion)
	if !ok {
		return nil
	}

	switch event.Type {
	case string(constant.ValueOf[constant.MessageStart]()):
		// Message started - capture input tokens
		msgStart := event.AsMessageStart()
		state.SetTokenUsage(int(msgStart.Message.Usage.InputTokens), state.GetOutputTokens())

	case string(constant.ValueOf[constant.ContentBlockStart]()):
		a.handleContentBlockStart(event, state)

	case string(constant.ValueOf[constant.ContentBlockDelta]()):
		a.handleContentBlockDelta(event, state)

	case string(constant.ValueOf[constant.ContentBlockStop]()):
		a.handleContentBlockStop(state)

	case string(constant.ValueOf[constant.MessageDelta]()):
		// Message delta contains stop_reason and usage stats
		msgDelta := event.AsMessageDelta()
		state.SetStopReason(mapAnthropicStopReason(msgDelta.Delta.StopReason))
		state.SetTokenUsage(state.GetInputTokens(), int(msgDelta.Usage.OutputTokens))

	case string(constant.ValueOf[constant.MessageStop]()):
		// Message complete - nothing to do here
	}

	return nil
}

// handleContentBlockStart processes content block start events
func (a *AnthropicAdapter) handleContentBlockStart(event anthropic.MessageStreamEventUnion, state streaming.StreamStateInterface) {
	blockStart := event.AsContentBlockStart()

	// Marshal to JSON to inspect the type
	b, _ := json.Marshal(blockStart.ContentBlock)
	var block map[string]any
	if json.Unmarshal(b, &block) == nil {
		blockType, _ := block["type"].(string)
		a.currentBlockType = blockType

		switch blockType {
		case string(constant.ValueOf[constant.Thinking]()):
			// Start capturing a thinking block
			a.currentThinkingBlock = map[string]any{
				"type":     string(constant.ValueOf[constant.Thinking]()),
				"thinking": "", // Will be filled by deltas
			}

		case string(constant.ValueOf[constant.ToolUse]()):
			// Initialize a new tool call
			id, _ := block["id"].(string)
			name, _ := block["name"].(string)

			// Add tool call to state
			state.AddToolCall(messages.ChatMessageToolCall{
				ID:        id,
				Name:      name,
				Arguments: "{}", // Default to empty JSON object
			})
			toolCalls := state.GetToolCalls()
			a.currentBlockIndex = len(toolCalls) - 1
		}
	}
}

// handleContentBlockDelta processes content block delta events
func (a *AnthropicAdapter) handleContentBlockDelta(event anthropic.MessageStreamEventUnion, state streaming.StreamStateInterface) {
	blockDelta := event.AsContentBlockDelta()

	// Check for thinking delta
	if thinking := blockDelta.Delta.Thinking; thinking != "" {
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
	if signature := blockDelta.Delta.Signature; signature != "" {
		if a.currentThinkingBlock != nil {
			a.currentThinkingBlock["signature"] = signature
		}
	}

	// Check for text delta (regular content)
	// Note: Content emission is handled by the main streaming loop

	// Check if it's tool use input delta
	if blockDelta.Delta.PartialJSON != "" && a.currentBlockType == string(constant.ValueOf[constant.ToolUse]()) {
		// Update the last tool call's arguments
		toolCalls := state.GetToolCalls()
		if a.currentBlockIndex >= 0 && a.currentBlockIndex < len(toolCalls) {
			state.UpdateToolCallAtIndex(a.currentBlockIndex, func(tc *messages.ChatMessageToolCall) {
				if tc.Arguments == "{}" {
					// First content, replace the default empty object
					tc.Arguments = blockDelta.Delta.PartialJSON
				} else {
					// Append to existing content
					tc.Arguments += blockDelta.Delta.PartialJSON
				}
			})
		}
	}
}

// handleContentBlockStop processes content block stop events
func (a *AnthropicAdapter) handleContentBlockStop(state streaming.StreamStateInterface) {
	if a.currentBlockType == string(constant.ValueOf[constant.Thinking]()) && a.currentThinkingBlock != nil {
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

// mapAnthropicStopReason converts Anthropic's stop reason to our normalized type
func mapAnthropicStopReason(sr anthropic.StopReason) messages.StopReason {
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
