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
	currentBlockType  string
	currentBlockIndex int
	// thinkingBuilder and thinkingSignature collect the open thinking block;
	// the block is recorded whole when it stops.
	thinkingBuilder   strings.Builder
	thinkingSignature string
	thinkingBlocks    []map[string]any
	arguments         streaming.ToolArgumentBuffers
}

// NewAdapter creates a new Anthropic streaming adapter
func NewAdapter() *Adapter {
	return &Adapter{}
}

// ProcessChunk translates one provider payload into stream state: a
// *StreamEvent from a streaming response, or a whole *Message from a
// non-streaming one. Text and reasoning emission stays with the caller.
func (a *Adapter) ProcessChunk(chunk any, state streaming.StreamStateInterface) error {
	switch v := chunk.(type) {
	case *StreamEvent:
		a.processEvent(v, state)
	case *Message:
		a.processMessage(v, state)
	}
	return nil
}

// processMessage records a complete (non-streaming) response: thinking
// blocks for replay, tool calls, the stop reason, and usage.
func (a *Adapter) processMessage(msg *Message, state streaming.StreamStateInterface) {
	for _, block := range msg.Content {
		switch block.Type {
		case "thinking":
			a.addThinkingBlock(block.Thinking, block.Signature)
		case "redacted_thinking":
			// Preserve verbatim; must be replayed unchanged in tool loops
			a.addRedactedThinkingBlock(block.Data)
		case "tool_use":
			state.AddToolCall(messages.ChatMessageToolCall{
				ID:        block.ID,
				Name:      block.Name,
				Arguments: string(block.Input),
			})
		}
	}
	state.SetStopReason(mapStopReason(msg.StopReason))
	if msg.Usage != nil {
		state.SetTokenUsage(int(msg.Usage.TotalInputTokens()), int(msg.Usage.OutputTokens))
		streaming.ApplyPromptCacheUsage(state, msg.Usage)
	}
}

// processEvent handles one streaming event.
func (a *Adapter) processEvent(event *StreamEvent, state streaming.StreamStateInterface) {
	switch event.Type {
	case EventMessageStart:
		// Message started - capture input tokens
		if event.Message != nil && event.Message.Usage != nil {
			state.SetTokenUsage(int(event.Message.Usage.TotalInputTokens()), state.GetOutputTokens())
			streaming.ApplyPromptCacheUsage(state, event.Message.Usage)
		}

	case EventContentBlockStart:
		a.handleContentBlockStart(event, state)

	case EventContentBlockDelta:
		a.handleContentBlockDelta(event, state)

	case EventContentBlockStop:
		a.handleContentBlockStop()

	case EventMessageDelta:
		// Message delta contains stop_reason and usage stats
		if event.Delta != nil {
			state.SetStopReason(mapStopReason(event.Delta.StopReason))
		}
		if event.Usage != nil {
			state.SetTokenUsage(state.GetInputTokens(), int(event.Usage.OutputTokens))
			streaming.ApplyPromptCacheUsage(state, event.Usage)
		}
	}
}

// handleContentBlockStart processes content block start events
func (a *Adapter) handleContentBlockStart(event *StreamEvent, state streaming.StreamStateInterface) {
	if event.ContentBlock == nil {
		return
	}
	a.currentBlockType = event.ContentBlock.Type

	switch event.ContentBlock.Type {
	case "thinking":
		// Start capturing a thinking block; deltas fill it in.
		a.thinkingBuilder.Reset()
		a.thinkingSignature = ""

	case "redacted_thinking":
		// Redacted thinking arrives complete in the start event (no
		// deltas); preserve it verbatim — it must be replayed unchanged
		// during tool loops.
		if event.ContentBlock.Data != "" {
			a.addRedactedThinkingBlock(event.ContentBlock.Data)
		}

	case "tool_use":
		// Initialize a new tool call
		a.currentBlockIndex = state.ToolCallCount()
		state.AddToolCall(messages.ChatMessageToolCall{
			ID:        event.ContentBlock.ID,
			Name:      event.ContentBlock.Name,
			Arguments: "{}", // Default to empty JSON object
		})
	}
}

// handleContentBlockDelta processes content block delta events. Text and
// reasoning emission is handled by the main streaming loop.
func (a *Adapter) handleContentBlockDelta(event *StreamEvent, state streaming.StreamStateInterface) {
	if event.Delta == nil {
		return
	}

	switch a.currentBlockType {
	case "thinking":
		a.thinkingBuilder.WriteString(event.Delta.Thinking)
		// The signature delta comes after the thinking content.
		if event.Delta.Signature != "" {
			a.thinkingSignature = event.Delta.Signature
		}

	case "tool_use":
		if event.Delta.PartialJSON != "" {
			state.UpdateToolCallAtIndex(a.currentBlockIndex, func(tc *messages.ChatMessageToolCall) {
				tc.Arguments = a.arguments.Append(a.currentBlockIndex, tc.Arguments, event.Delta.PartialJSON)
			})
		}
	}
}

// handleContentBlockStop processes content block stop events
func (a *Adapter) handleContentBlockStop() {
	switch a.currentBlockType {
	case "thinking":
		a.addThinkingBlock(a.thinkingBuilder.String(), a.thinkingSignature)
	case "tool_use":
		delete(a.arguments, a.currentBlockIndex)
	}
	a.currentBlockType = ""
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

// addThinkingBlock records a completed thinking block, streamed or from a
// non-streaming response, for replay.
func (a *Adapter) addThinkingBlock(thinking, signature string) {
	a.thinkingBlocks = append(a.thinkingBlocks, map[string]any{
		"type":      "thinking",
		"thinking":  thinking,
		"signature": signature,
	})
}

// addRedactedThinkingBlock preserves a redacted thinking block so it can be
// replayed unchanged
func (a *Adapter) addRedactedThinkingBlock(data string) {
	a.thinkingBlocks = append(a.thinkingBlocks, map[string]any{
		"type": "redacted_thinking",
		"data": data,
	})
}

// mapStopReason converts Anthropic's stop reason to our normalized type
func mapStopReason(sr StopReason) messages.StopReason {
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
