package adapters

import (
	"errors"
	"fmt"

	"github.com/alexschlessinger/pollytool/llm/openai"
	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
)

const responsesErrorMetadataKey = "openai_responses_error"

// OpenAIAdapter handles Chat Completions streaming patterns.
// Chat Completions sends tool calls incrementally with index-based updates.
type OpenAIAdapter struct{}

func NewOpenAIAdapter() *OpenAIAdapter {
	return &OpenAIAdapter{}
}

func (a *OpenAIAdapter) ProcessChunk(chunk any, state streaming.StreamStateInterface) error {
	response, ok := asChatCompletionChunk(chunk)
	if !ok {
		return nil
	}

	if response.Usage != nil {
		state.SetTokenUsage(int(response.Usage.PromptTokens), int(response.Usage.CompletionTokens))
		applyOpenAIPromptCacheUsage(response.Usage, state)
	}

	if len(response.Choices) == 0 {
		return nil
	}

	choice := response.Choices[0]
	if choice.FinishReason != "" {
		state.SetStopReason(MapOpenAIFinishReason(choice.FinishReason))
	}

	for _, tc := range choice.Delta.ToolCalls {
		a.handleIndexedToolCall(int(tc.Index), tc, state)
	}

	return nil
}

func (a *OpenAIAdapter) handleIndexedToolCall(index int, tc openai.ChatToolCallDelta, state streaming.StreamStateInterface) {
	state.UpdateToolCallAtIndex(index, func(toolCall *messages.ChatMessageToolCall) {
		if tc.ID != "" {
			toolCall.ID = tc.ID
		}
		if tc.Function.Name != "" {
			toolCall.Name = tc.Function.Name
		}
		if tc.Function.Arguments == "" {
			return
		}
		if toolCall.Arguments == "{}" {
			toolCall.Arguments = tc.Function.Arguments
			return
		}
		toolCall.Arguments += tc.Function.Arguments
	})
}

func (a *OpenAIAdapter) EnrichFinalMessage(_ *messages.ChatMessage, _ streaming.StreamStateInterface) {
}

func (a *OpenAIAdapter) HandleToolCall(_ any, _ streaming.StreamStateInterface) error {
	return nil
}

// OpenAIResponsesAdapter handles Responses API streaming events.
type OpenAIResponsesAdapter struct {
	// OutputIndex is shared across reasoning/text/function_call items, so it
	// can be sparse — map it to a dense tool-call index.
	toolCallIndexByOutput map[int]int
}

func NewOpenAIResponsesAdapter() *OpenAIResponsesAdapter {
	return &OpenAIResponsesAdapter{
		toolCallIndexByOutput: make(map[int]int),
	}
}

func (a *OpenAIResponsesAdapter) ProcessChunk(chunk any, state streaming.StreamStateInterface) error {
	event, ok := asResponsesEvent(chunk)
	if !ok {
		return nil
	}

	switch event.Type {
	case "response.function_call_arguments.delta":
		a.handleFunctionCallDelta(event, state)
	case "response.function_call_arguments.done":
		a.handleFunctionCallDone(event, state)
	case "response.output_item.added", "response.output_item.done":
		a.handleOutputItem(event.Item, int(event.OutputIndex), state)
	case "response.completed", "response.incomplete", "response.failed":
		a.applyResponse(event.Response, state)
	case "error":
		msg := event.Message
		if event.Code != "" {
			msg = fmt.Sprintf("%s: %s", event.Code, event.Message)
		}
		if msg != "" {
			state.SetMetadata(responsesErrorMetadataKey, msg)
		}
		state.SetStopReason(messages.StopReasonError)
	}

	return nil
}

func (a *OpenAIResponsesAdapter) handleFunctionCallDelta(event *openai.ResponseStreamEvent, state streaming.StreamStateInterface) {
	if event.Delta == "" {
		return
	}
	a.updateToolCallAtOutputIndex(int(event.OutputIndex), state, func(toolCall *messages.ChatMessageToolCall) {
		if toolCall.Arguments == "{}" {
			toolCall.Arguments = string(event.Delta)
			return
		}
		toolCall.Arguments += string(event.Delta)
	})
}

func (a *OpenAIResponsesAdapter) handleFunctionCallDone(event *openai.ResponseStreamEvent, state streaming.StreamStateInterface) {
	a.updateToolCallAtOutputIndex(int(event.OutputIndex), state, func(toolCall *messages.ChatMessageToolCall) {
		if event.Name != "" {
			toolCall.Name = event.Name
		}
		if event.Arguments != "" {
			toolCall.Arguments = event.Arguments
		}
	})
}

func (a *OpenAIResponsesAdapter) handleOutputItem(item *openai.ResponseOutputItem, index int, state streaming.StreamStateInterface) {
	if item == nil || item.Type != "function_call" {
		return
	}
	a.updateToolCallAtOutputIndex(index, state, func(toolCall *messages.ChatMessageToolCall) {
		if item.CallID != "" {
			toolCall.ID = item.CallID
		} else if item.ID != "" {
			toolCall.ID = item.ID
		}
		if item.Name != "" {
			toolCall.Name = item.Name
		}
		if args := string(item.Arguments); args != "" {
			toolCall.Arguments = args
		}
	})
}

func (a *OpenAIResponsesAdapter) updateToolCallAtOutputIndex(outputIndex int, state streaming.StreamStateInterface, updater func(*messages.ChatMessageToolCall)) {
	toolIndex, exists := a.toolCallIndexByOutput[outputIndex]
	if !exists {
		toolIndex = len(a.toolCallIndexByOutput)
		a.toolCallIndexByOutput[outputIndex] = toolIndex
	}
	state.UpdateToolCallAtIndex(toolIndex, updater)
}

func (a *OpenAIResponsesAdapter) applyResponse(resp *openai.Response, state streaming.StreamStateInterface) {
	if resp == nil {
		return
	}
	if resp.Usage != nil {
		state.SetTokenUsage(int(resp.Usage.InputTokens), int(resp.Usage.OutputTokens))
		if read, write, reported := resp.Usage.PromptCacheUsage(); reported {
			state.SetPromptCacheUsage(read, write)
		}
	}
	incompleteReason := ""
	if resp.IncompleteDetails != nil {
		incompleteReason = resp.IncompleteDetails.Reason
	}
	state.SetStopReason(MapResponsesStopReason(resp.Status, incompleteReason, len(state.GetToolCalls()) > 0))
}

func applyOpenAIPromptCacheUsage(usage *openai.ChatUsage, state streaming.StreamStateInterface) {
	if read, write, reported := usage.PromptCacheUsage(); reported {
		state.SetPromptCacheUsage(read, write)
	}
}

func (a *OpenAIResponsesAdapter) EnrichFinalMessage(msg *messages.ChatMessage, state streaming.StreamStateInterface) {
	errValue, ok := state.GetMetadata(responsesErrorMetadataKey)
	if !ok {
		return
	}
	errMsg, ok := errValue.(string)
	if !ok || errMsg == "" {
		return
	}
	msg.SetError(errors.New(errMsg))
}

func (a *OpenAIResponsesAdapter) HandleToolCall(_ any, _ streaming.StreamStateInterface) error {
	return nil
}

// MapOpenAIFinishReason converts Chat Completions finish reasons to Polly's normalized type.
func MapOpenAIFinishReason(fr string) messages.StopReason {
	switch fr {
	case "stop":
		return messages.StopReasonEndTurn
	case "tool_calls", "function_call":
		return messages.StopReasonToolUse
	case "length":
		return messages.StopReasonMaxTokens
	case "content_filter":
		return messages.StopReasonContentFilter
	default:
		return messages.StopReasonEndTurn
	}
}

// MapResponsesStopReason converts Responses terminal state to Polly's normalized type.
func MapResponsesStopReason(status openai.ResponseStatus, incompleteReason string, hasToolCalls bool) messages.StopReason {
	switch status {
	case openai.ResponseStatusCompleted:
		if hasToolCalls {
			return messages.StopReasonToolUse
		}
		return messages.StopReasonEndTurn
	case openai.ResponseStatusIncomplete:
		switch incompleteReason {
		case "max_output_tokens":
			return messages.StopReasonMaxTokens
		case "content_filter":
			return messages.StopReasonContentFilter
		default:
			return messages.StopReasonError
		}
	case openai.ResponseStatusFailed, openai.ResponseStatusCancelled:
		return messages.StopReasonError
	default:
		if hasToolCalls {
			return messages.StopReasonToolUse
		}
		return messages.StopReasonError
	}
}

func asChatCompletionChunk(chunk any) (*openai.ChatCompletionChunk, bool) {
	switch value := chunk.(type) {
	case *openai.ChatCompletionChunk:
		return value, true
	case openai.ChatCompletionChunk:
		return &value, true
	default:
		return nil, false
	}
}

func asResponsesEvent(chunk any) (*openai.ResponseStreamEvent, bool) {
	switch value := chunk.(type) {
	case *openai.ResponseStreamEvent:
		if value == nil {
			return nil, false
		}
		return value, true
	case openai.ResponseStreamEvent:
		return &value, true
	default:
		return nil, false
	}
}
