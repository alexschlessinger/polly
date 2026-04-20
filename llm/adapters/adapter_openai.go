package adapters

import (
	"errors"
	"fmt"

	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
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

	if response.JSON.Usage.Valid() {
		state.SetTokenUsage(int(response.Usage.PromptTokens), int(response.Usage.CompletionTokens))
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

func (a *OpenAIAdapter) handleIndexedToolCall(index int, tc openai.ChatCompletionChunkChoiceDeltaToolCall, state streaming.StreamStateInterface) {
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
type OpenAIResponsesAdapter struct{}

func NewOpenAIResponsesAdapter() *OpenAIResponsesAdapter {
	return &OpenAIResponsesAdapter{}
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

func (a *OpenAIResponsesAdapter) handleFunctionCallDelta(event responses.ResponseStreamEventUnion, state streaming.StreamStateInterface) {
	if event.Delta == "" {
		return
	}
	state.UpdateToolCallAtIndex(int(event.OutputIndex), func(toolCall *messages.ChatMessageToolCall) {
		if toolCall.Arguments == "{}" {
			toolCall.Arguments = event.Delta
			return
		}
		toolCall.Arguments += event.Delta
	})
}

func (a *OpenAIResponsesAdapter) handleFunctionCallDone(event responses.ResponseStreamEventUnion, state streaming.StreamStateInterface) {
	state.UpdateToolCallAtIndex(int(event.OutputIndex), func(toolCall *messages.ChatMessageToolCall) {
		if event.Name != "" {
			toolCall.Name = event.Name
		}
		if event.Arguments != "" {
			toolCall.Arguments = event.Arguments
		}
	})
}

func (a *OpenAIResponsesAdapter) handleOutputItem(item responses.ResponseOutputItemUnion, index int, state streaming.StreamStateInterface) {
	if item.Type != "function_call" {
		return
	}
	state.UpdateToolCallAtIndex(index, func(toolCall *messages.ChatMessageToolCall) {
		if item.CallID != "" {
			toolCall.ID = item.CallID
		} else if item.ID != "" {
			toolCall.ID = item.ID
		}
		if item.Name != "" {
			toolCall.Name = item.Name
		}
		if args := responseArgumentsString(item.Arguments); args != "" {
			toolCall.Arguments = args
		}
	})
}

func (a *OpenAIResponsesAdapter) applyResponse(resp responses.Response, state streaming.StreamStateInterface) {
	if resp.Usage.JSON.TotalTokens.Valid() {
		state.SetTokenUsage(int(resp.Usage.InputTokens), int(resp.Usage.OutputTokens))
	}
	state.SetStopReason(MapResponsesStopReason(resp.Status, resp.IncompleteDetails.Reason, len(state.GetToolCalls()) > 0))
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
func MapResponsesStopReason(status responses.ResponseStatus, incompleteReason string, hasToolCalls bool) messages.StopReason {
	switch status {
	case responses.ResponseStatusCompleted:
		if hasToolCalls {
			return messages.StopReasonToolUse
		}
		return messages.StopReasonEndTurn
	case responses.ResponseStatusIncomplete:
		switch incompleteReason {
		case "max_output_tokens":
			return messages.StopReasonMaxTokens
		case "content_filter":
			return messages.StopReasonContentFilter
		default:
			return messages.StopReasonError
		}
	case responses.ResponseStatusFailed, responses.ResponseStatusCancelled:
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

func asResponsesEvent(chunk any) (responses.ResponseStreamEventUnion, bool) {
	switch value := chunk.(type) {
	case responses.ResponseStreamEventUnion:
		return value, true
	case *responses.ResponseStreamEventUnion:
		if value == nil {
			return responses.ResponseStreamEventUnion{}, false
		}
		return *value, true
	default:
		return responses.ResponseStreamEventUnion{}, false
	}
}

func responseArgumentsString(args responses.ResponseOutputItemUnionArguments) string {
	if args.JSON.OfString.Valid() {
		return args.OfString
	}
	return ""
}
