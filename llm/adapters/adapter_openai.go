package adapters

import (
	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
	ai "github.com/sashabaranov/go-openai"
)

// OpenAIAdapter handles OpenAI-specific streaming patterns.
// OpenAI sends tool calls incrementally with index-based updates.
type OpenAIAdapter struct{}

// NewOpenAIAdapter creates a new OpenAI streaming adapter
func NewOpenAIAdapter() *OpenAIAdapter {
	return &OpenAIAdapter{}
}

// ProcessChunk handles OpenAI streaming chunks
func (a *OpenAIAdapter) ProcessChunk(chunk any, state streaming.StreamStateInterface) error {
	response, ok := chunk.(*ai.ChatCompletionStreamResponse)
	if !ok {
		return nil
	}

	// Capture usage from final chunk (sent when StreamOptions.IncludeUsage is true)
	if response.Usage != nil {
		state.SetTokenUsage(response.Usage.PromptTokens, response.Usage.CompletionTokens)
	}

	if len(response.Choices) > 0 {
		choice := response.Choices[0]
		delta := choice.Delta

		// Capture finish reason when it's set
		if choice.FinishReason != "" {
			state.SetStopReason(mapOpenAIFinishReason(choice.FinishReason))
		}

		// Handle tool calls - OpenAI's special index-based accumulation
		if len(delta.ToolCalls) > 0 {
			for _, tc := range delta.ToolCalls {
				if tc.Index != nil {
					a.handleIndexedToolCall(*tc.Index, tc, state)
				}
			}
		}
	}

	return nil
}

// handleIndexedToolCall manages OpenAI's index-based tool call accumulation
func (a *OpenAIAdapter) handleIndexedToolCall(index int, tc ai.ToolCall, state streaming.StreamStateInterface) {
	state.UpdateToolCallAtIndex(index, func(toolCall *messages.ChatMessageToolCall) {
		// Update ID if provided
		if tc.ID != "" {
			toolCall.ID = tc.ID
		}

		// Update name if provided
		if tc.Function.Name != "" {
			toolCall.Name = tc.Function.Name
		}

		// Accumulate arguments
		if tc.Function.Arguments != "" {
			if toolCall.Arguments == "{}" {
				// First content, replace the default empty object
				toolCall.Arguments = tc.Function.Arguments
			} else {
				// Append to existing content
				toolCall.Arguments += tc.Function.Arguments
			}
		}
	})
}

// EnrichFinalMessage adds any OpenAI-specific metadata to the final message
func (a *OpenAIAdapter) EnrichFinalMessage(msg *messages.ChatMessage, state streaming.StreamStateInterface) {
	// OpenAI doesn't require special metadata enrichment
	// Token usage is already set by StreamingCore
}

// HandleToolCall provides OpenAI-specific tool call handling
func (a *OpenAIAdapter) HandleToolCall(toolData any, state streaming.StreamStateInterface) error {
	// Tool calls are handled in ProcessChunk for OpenAI
	return nil
}

// mapOpenAIFinishReason converts OpenAI's finish reason to our normalized type
func mapOpenAIFinishReason(fr ai.FinishReason) messages.StopReason {
	switch fr {
	case ai.FinishReasonStop:
		return messages.StopReasonEndTurn
	case ai.FinishReasonToolCalls, ai.FinishReasonFunctionCall:
		return messages.StopReasonToolUse
	case ai.FinishReasonLength:
		return messages.StopReasonMaxTokens
	case ai.FinishReasonContentFilter:
		return messages.StopReasonContentFilter
	default:
		return messages.StopReasonEndTurn
	}
}
