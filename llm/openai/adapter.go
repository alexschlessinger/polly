package openai

import (
	"errors"
	"fmt"

	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
)

// responsesReasoningItemsStateKey accumulates reasoning items on the stream
// state. The streaming adapter and the non-streaming response walk both write
// here so EnrichFinalMessage has a single place to read from.
const responsesReasoningItemsStateKey = "openai_responses_reasoning_items"

// ResponsesReasoningItemsKey and ResponsesReasoningModelKey are where a
// completed assistant message carries the reasoning items to replay on the next
// request, and the model that produced them. Encrypted reasoning is decryptable
// only by its own model, so the replay is dropped after a model switch.
const (
	ResponsesReasoningItemsKey = "openai_reasoning_items"
	ResponsesReasoningModelKey = "openai_reasoning_model"
)

// ChatAdapter handles Chat Completions streaming patterns.
// Chat Completions sends tool calls incrementally with index-based updates.
type ChatAdapter struct {
	arguments streaming.ToolArgumentBuffers
}

func NewChatAdapter() *ChatAdapter {
	return &ChatAdapter{}
}

func (a *ChatAdapter) ProcessChunk(chunk any, state streaming.StreamStateInterface) error {
	response, ok := chunk.(*ChatCompletionChunk)
	if !ok {
		return nil
	}

	if response.Usage != nil {
		state.SetTokenUsage(int(response.Usage.PromptTokens), int(response.Usage.CompletionTokens))
		streaming.ApplyPromptCacheUsage(state, response.Usage)
	}

	if len(response.Choices) == 0 {
		return nil
	}

	choice := response.Choices[0]
	if choice.FinishReason != "" {
		state.SetStopReason(MapChatFinishReason(choice.FinishReason))
	}

	for _, tc := range choice.Delta.ToolCalls {
		a.handleIndexedToolCall(int(tc.Index), tc, state)
	}

	return nil
}

func (a *ChatAdapter) handleIndexedToolCall(index int, tc ChatToolCallDelta, state streaming.StreamStateInterface) {
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
		toolCall.Arguments = a.arguments.Append(index, toolCall.Arguments, tc.Function.Arguments)
	})
}

func (a *ChatAdapter) EnrichFinalMessage(_ *messages.ChatMessage, _ streaming.StreamStateInterface) {
}

// ResponsesAdapter handles Responses API streaming events.
type ResponsesAdapter struct {
	// OutputIndex is shared across reasoning/text/function_call items, so it
	// can be sparse — map it to a dense tool-call index.
	toolCallIndexByOutput map[int]int
	// model stamps the reasoning items so a later turn can tell whether they
	// are still replayable.
	model     string
	arguments streaming.ToolArgumentBuffers
	// errMsg is the text of a model-side error event, marked on the final
	// message.
	errMsg string
}

func NewResponsesAdapter(model string) *ResponsesAdapter {
	return &ResponsesAdapter{
		toolCallIndexByOutput: make(map[int]int),
		model:                 model,
	}
}

func (a *ResponsesAdapter) ProcessChunk(chunk any, state streaming.StreamStateInterface) error {
	event, ok := chunk.(*ResponseStreamEvent)
	if !ok || event == nil {
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
		a.errMsg = event.Message
		if event.Code != "" {
			a.errMsg = fmt.Sprintf("%s: %s", event.Code, event.Message)
		}
		state.SetStopReason(messages.StopReasonError)
	}

	return nil
}

func (a *ResponsesAdapter) handleFunctionCallDelta(event *ResponseStreamEvent, state streaming.StreamStateInterface) {
	if event.Delta == "" {
		return
	}
	a.updateToolCallAtOutputIndex(int(event.OutputIndex), state, func(toolCall *messages.ChatMessageToolCall) {
		toolCall.Arguments = a.arguments.Append(int(event.OutputIndex), toolCall.Arguments, string(event.Delta))
	})
}

func (a *ResponsesAdapter) handleFunctionCallDone(event *ResponseStreamEvent, state streaming.StreamStateInterface) {
	a.updateToolCallAtOutputIndex(int(event.OutputIndex), state, func(toolCall *messages.ChatMessageToolCall) {
		if event.Name != "" {
			toolCall.Name = event.Name
		}
		if event.Arguments != "" {
			toolCall.Arguments = event.Arguments
			delete(a.arguments, int(event.OutputIndex))
		}
	})
}

func (a *ResponsesAdapter) handleOutputItem(item *ResponseOutputItem, index int, state streaming.StreamStateInterface) {
	if item == nil {
		return
	}
	if item.Type == "reasoning" {
		AppendResponsesReasoningItem(state, item)
		return
	}
	if item.Type != "function_call" {
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
			delete(a.arguments, index)
		}
	})
}

func (a *ResponsesAdapter) updateToolCallAtOutputIndex(outputIndex int, state streaming.StreamStateInterface, updater func(*messages.ChatMessageToolCall)) {
	toolIndex, exists := a.toolCallIndexByOutput[outputIndex]
	if !exists {
		toolIndex = len(a.toolCallIndexByOutput)
		a.toolCallIndexByOutput[outputIndex] = toolIndex
	}
	state.UpdateToolCallAtIndex(toolIndex, updater)
}

func (a *ResponsesAdapter) applyResponse(resp *Response, state streaming.StreamStateInterface) {
	if resp == nil {
		return
	}
	if resp.Usage != nil {
		state.SetTokenUsage(int(resp.Usage.InputTokens), int(resp.Usage.OutputTokens))
		streaming.ApplyPromptCacheUsage(state, resp.Usage)
	}
	// The terminal event carries the finished output items, so harvest
	// reasoning again here: whether encrypted_content rides on
	// output_item.done or only on the final response varies by model.
	for i := range resp.Output {
		if resp.Output[i].Type == "reasoning" {
			AppendResponsesReasoningItem(state, &resp.Output[i])
		}
	}
	incompleteReason := ""
	if resp.IncompleteDetails != nil {
		incompleteReason = resp.IncompleteDetails.Reason
	}
	state.SetStopReason(MapResponsesStopReason(resp.Status, incompleteReason, state.ToolCallCount() > 0))
}

// AppendResponsesReasoningItem records a reasoning item so the next request can
// replay it. The API treats encrypted_content as the reasoning state itself, so
// the item is kept verbatim rather than reduced to its summary text. Items
// arrive more than once — output_item.added, then .done, then the terminal
// response — so a repeated id replaces the earlier entry.
func AppendResponsesReasoningItem(state streaming.StreamStateInterface, item *ResponseOutputItem) {
	if item == nil || item.ID == "" {
		return
	}
	summary := make([]any, 0, len(item.Summary))
	for _, part := range item.Summary {
		summary = append(summary, map[string]any{"type": part.Type, "text": part.Text})
	}
	entry := map[string]any{
		"id":                item.ID,
		"summary":           summary,
		"encrypted_content": item.EncryptedContent,
	}

	existing, _ := state.GetMetadata(responsesReasoningItemsStateKey)
	items, _ := existing.([]map[string]any)
	for i, prior := range items {
		if prior["id"] == item.ID {
			// Only overwrite once the encrypted state has arrived; the early
			// output_item.added carries an empty payload.
			if item.EncryptedContent != "" {
				items[i] = entry
				state.SetMetadata(responsesReasoningItemsStateKey, items)
			}
			return
		}
	}
	state.SetMetadata(responsesReasoningItemsStateKey, append(items, entry))
}

func (a *ResponsesAdapter) EnrichFinalMessage(msg *messages.ChatMessage, state streaming.StreamStateInterface) {
	if items, ok := state.GetMetadata(responsesReasoningItemsStateKey); ok {
		if msg.Metadata == nil {
			msg.Metadata = make(map[string]any)
		}
		msg.Metadata[ResponsesReasoningItemsKey] = items
		msg.Metadata[ResponsesReasoningModelKey] = a.model
	}
	if a.errMsg != "" {
		msg.SetError(errors.New(a.errMsg))
	}
}

// MapChatFinishReason converts Chat Completions finish reasons to Polly's normalized type.
func MapChatFinishReason(fr string) messages.StopReason {
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

// MapResponsesStopReason converts Responses terminal state to Polly's
// normalized type. A completed response with tool calls maps to an ordinary
// finish here; the streaming core promotes it to a tool turn at completion.
// hasToolCalls only decides how an unknown terminal status is read.
func MapResponsesStopReason(status ResponseStatus, incompleteReason string, hasToolCalls bool) messages.StopReason {
	switch status {
	case ResponseStatusCompleted:
		return messages.StopReasonEndTurn
	case ResponseStatusIncomplete:
		switch incompleteReason {
		case "max_output_tokens":
			return messages.StopReasonMaxTokens
		case "content_filter":
			return messages.StopReasonContentFilter
		default:
			return messages.StopReasonError
		}
	case ResponseStatusFailed, ResponseStatusCancelled:
		return messages.StopReasonError
	default:
		if hasToolCalls {
			return messages.StopReasonToolUse
		}
		return messages.StopReasonError
	}
}
