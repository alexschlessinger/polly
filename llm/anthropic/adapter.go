package anthropic

import (
	"strings"

	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
)

// ThinkingBlocksKey is the message metadata key under which the
// adapter stores the thinking blocks (with signatures) a reply carried, so
// the client can replay them on later requests.
const ThinkingBlocksKey = "anthropic_thinking_blocks"

// Adapter handles Anthropic-specific streaming patterns.
// Anthropic uses event-based streaming with thinking blocks and structured events.
type Adapter struct {
	currentBlockType     string
	currentBlockIndex    int
	currentThinkingBlock map[string]any
	thinkingBlocks       []map[string]any
	thinkingBuilder      strings.Builder
	arguments            streaming.ToolArgumentBuffers
}

// NewAdapter creates a new Anthropic streaming adapter
func NewAdapter() *Adapter {
	return &Adapter{
		thinkingBlocks: make([]map[string]any, 0),
	}
}

// ProcessChunk handles Anthropic streaming events
func (a *Adapter) ProcessChunk(chunk any, state streaming.StreamStateInterface) error {
	event, ok := chunk.(*StreamEvent)
	if !ok {
		return nil
	}

	switch event.Type {
	case EventMessageStart:
		// Message started - capture input tokens
		if event.Message != nil && event.Message.Usage != nil {
			applyInputUsage(event.Message.Usage, state)
		}

	case EventContentBlockStart:
		a.handleContentBlockStart(event, state)

	case EventContentBlockDelta:
		a.handleContentBlockDelta(event, state)

	case EventContentBlockStop:
		a.handleContentBlockStop(state)

	case EventMessageDelta:
		// Message delta contains stop_reason and usage stats
		if event.Delta != nil {
			state.SetStopReason(MapStopReason(event.Delta.StopReason))
		}
		if event.Usage != nil {
			state.SetTokenUsage(state.GetInputTokens(), int(event.Usage.OutputTokens))
			streaming.ApplyPromptCacheUsage(state, event.Usage)
		}

	case EventMessageStop:
		// Message complete - nothing to do here
	}

	return nil
}

func applyInputUsage(usage *Usage, state streaming.StreamStateInterface) {
	state.SetTokenUsage(int(usage.TotalInputTokens()), state.GetOutputTokens())
	streaming.ApplyPromptCacheUsage(state, usage)
}

// handleContentBlockStart processes content block start events
func (a *Adapter) handleContentBlockStart(event *StreamEvent, state streaming.StreamStateInterface) {
	if event.ContentBlock == nil {
		return
	}
	a.currentBlockType = event.ContentBlock.Type

	switch event.ContentBlock.Type {
	case "thinking":
		// Start capturing a thinking block
		a.thinkingBuilder.Reset()
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
func (a *Adapter) handleContentBlockDelta(event *StreamEvent, state streaming.StreamStateInterface) {
	if event.Delta == nil {
		return
	}

	// Check for thinking delta
	if thinking := event.Delta.Thinking; thinking != "" {
		// Add to current thinking block if we're capturing one
		if a.currentThinkingBlock != nil {
			a.thinkingBuilder.WriteString(thinking)
			a.currentThinkingBlock["thinking"] = a.thinkingBuilder.String()
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
		// The block-start event already established this index.
		if a.currentBlockIndex >= 0 {
			state.UpdateToolCallAtIndex(a.currentBlockIndex, func(tc *messages.ChatMessageToolCall) {
				tc.Arguments = a.arguments.Append(a.currentBlockIndex, tc.Arguments, event.Delta.PartialJSON)
			})
		}
	}
}

// handleContentBlockStop processes content block stop events
func (a *Adapter) handleContentBlockStop(state streaming.StreamStateInterface) {
	delete(a.arguments, a.currentBlockIndex)
	if a.currentBlockType == "thinking" && a.currentThinkingBlock != nil {
		// Save completed thinking block
		a.thinkingBlocks = append(a.thinkingBlocks, a.currentThinkingBlock)
		a.currentThinkingBlock = nil
	}
	a.currentBlockType = ""
	a.currentBlockIndex = -1
}

// EnrichFinalMessage adds Anthropic-specific metadata to the final message
func (a *Adapter) EnrichFinalMessage(msg *messages.ChatMessage, state streaming.StreamStateInterface) {
	// Add thinking blocks to metadata
	if len(a.thinkingBlocks) > 0 {
		if msg.Metadata == nil {
			msg.Metadata = make(map[string]any)
		}
		msg.Metadata[ThinkingBlocksKey] = a.thinkingBlocks
	}
}

// AddThinkingBlock adds a thinking block for non-streaming responses
func (a *Adapter) AddThinkingBlock(thinking, signature string) {
	a.thinkingBlocks = append(a.thinkingBlocks, map[string]any{
		"type":      "thinking",
		"thinking":  thinking,
		"signature": signature,
	})
}

// AddRedactedThinkingBlock preserves a redacted thinking block so it can be
// replayed unchanged
func (a *Adapter) AddRedactedThinkingBlock(data string) {
	a.thinkingBlocks = append(a.thinkingBlocks, map[string]any{
		"type": "redacted_thinking",
		"data": data,
	})
}

// MapStopReason converts Anthropic's stop reason to our normalized type
func MapStopReason(sr StopReason) messages.StopReason {
	switch sr {
	case StopReasonToolUse:
		return messages.StopReasonToolUse
	case StopReasonMaxTokens:
		return messages.StopReasonMaxTokens
	case StopReasonRefusal:
		return messages.StopReasonContentFilter
	default: // end_turn, stop_sequence, and anything unknown
		return messages.StopReasonEndTurn
	}
}
