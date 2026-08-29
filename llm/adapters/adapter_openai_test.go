package adapters

import (
	"testing"

	"github.com/alexschlessinger/pollytool/llm/openai"
	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestOpenAIResponsesAdapterAccumulatesFunctionCallState(t *testing.T) {
	adapter := NewOpenAIResponsesAdapter("gpt-5")
	state := streaming.NewStreamState()

	events := []*openai.ResponseStreamEvent{
		{
			Type:        "response.function_call_arguments.delta",
			OutputIndex: 0,
			Delta:       `{"city":"San`,
		},
		{
			Type:        "response.output_item.done",
			OutputIndex: 0,
			Item: &openai.ResponseOutputItem{
				Type:   "function_call",
				CallID: "call_weather",
				Name:   "lookup_weather",
			},
		},
		{
			Type:        "response.function_call_arguments.done",
			OutputIndex: 0,
			Name:        "lookup_weather",
			Arguments:   `{"city":"San Francisco"}`,
		},
		{
			Type: "response.completed",
			Response: &openai.Response{
				Status: openai.ResponseStatusCompleted,
				Usage: &openai.ResponseUsage{
					InputTokens:  12,
					OutputTokens: 7,
					TotalTokens:  19,
				},
			},
		},
	}

	for _, event := range events {
		if err := adapter.ProcessChunk(event, state); err != nil {
			t.Fatalf("ProcessChunk returned error: %v", err)
		}
	}

	toolCalls := state.GetToolCalls()
	if len(toolCalls) != 1 {
		t.Fatalf("tool call count = %d, want 1", len(toolCalls))
	}
	if got := toolCalls[0].ID; got != "call_weather" {
		t.Fatalf("tool call ID = %q, want %q", got, "call_weather")
	}
	if got := toolCalls[0].Name; got != "lookup_weather" {
		t.Fatalf("tool call name = %q, want %q", got, "lookup_weather")
	}
	if got := toolCalls[0].Arguments; got != `{"city":"San Francisco"}` {
		t.Fatalf("tool call arguments = %q, want %q", got, `{"city":"San Francisco"}`)
	}
	if got := state.StopReason; got != messages.StopReasonToolUse {
		t.Fatalf("stop reason = %q, want %q", got, messages.StopReasonToolUse)
	}
	if got := state.GetInputTokens(); got != 12 {
		t.Fatalf("input tokens = %d, want 12", got)
	}
	if got := state.GetOutputTokens(); got != 7 {
		t.Fatalf("output tokens = %d, want 7", got)
	}
}

func TestOpenAIResponsesAdapterCompactsSparseOutputIndices(t *testing.T) {
	adapter := NewOpenAIResponsesAdapter("gpt-5")
	state := streaming.NewStreamState()

	events := []*openai.ResponseStreamEvent{
		{
			Type:        "response.function_call_arguments.delta",
			OutputIndex: 1,
			Delta:       `{"command":"docker ps"`,
		},
		{
			Type:        "response.output_item.done",
			OutputIndex: 1,
			Item: &openai.ResponseOutputItem{
				Type:   "function_call",
				CallID: "call_bash_1",
				Name:   "bash",
			},
		},
		{
			Type:        "response.function_call_arguments.done",
			OutputIndex: 1,
			Name:        "bash",
			Arguments:   `{"command":"docker ps"}`,
		},
		{
			Type:        "response.output_item.done",
			OutputIndex: 3,
			Item: &openai.ResponseOutputItem{
				Type:   "function_call",
				CallID: "call_bash_2",
				Name:   "bash",
			},
		},
		{
			Type:        "response.function_call_arguments.done",
			OutputIndex: 3,
			Name:        "bash",
			Arguments:   `{"command":"docker inspect nginx"}`,
		},
		{
			Type: "response.completed",
			Response: &openai.Response{
				Status: openai.ResponseStatusCompleted,
			},
		},
	}

	for _, event := range events {
		if err := adapter.ProcessChunk(event, state); err != nil {
			t.Fatalf("ProcessChunk returned error: %v", err)
		}
	}

	toolCalls := state.GetToolCalls()
	if len(toolCalls) != 2 {
		t.Fatalf("tool call count = %d, want 2", len(toolCalls))
	}
	if got := toolCalls[0].Name; got != "bash" {
		t.Fatalf("first tool call name = %q, want %q", got, "bash")
	}
	if got := toolCalls[0].Arguments; got != `{"command":"docker ps"}` {
		t.Fatalf("first tool call arguments = %q, want %q", got, `{"command":"docker ps"}`)
	}
	if got := toolCalls[1].ID; got != "call_bash_2" {
		t.Fatalf("second tool call ID = %q, want %q", got, "call_bash_2")
	}
	if got := toolCalls[1].Arguments; got != `{"command":"docker inspect nginx"}` {
		t.Fatalf("second tool call arguments = %q, want %q", got, `{"command":"docker inspect nginx"}`)
	}
}

func TestOpenAIResponsesAdapterMapsIncompleteAndErrorStates(t *testing.T) {
	tests := []struct {
		name             string
		event            *openai.ResponseStreamEvent
		want             messages.StopReason
		wantErrorMessage string
	}{
		{
			name: "max_output_tokens",
			event: &openai.ResponseStreamEvent{
				Type: "response.incomplete",
				Response: &openai.Response{
					Status: openai.ResponseStatusIncomplete,
					IncompleteDetails: &openai.IncompleteDetails{
						Reason: "max_output_tokens",
					},
				},
			},
			want: messages.StopReasonMaxTokens,
		},
		{
			name: "content_filter",
			event: &openai.ResponseStreamEvent{
				Type: "response.incomplete",
				Response: &openai.Response{
					Status: openai.ResponseStatusIncomplete,
					IncompleteDetails: &openai.IncompleteDetails{
						Reason: "content_filter",
					},
				},
			},
			want: messages.StopReasonContentFilter,
		},
		{
			name: "error_event",
			event: &openai.ResponseStreamEvent{
				Type:    "error",
				Code:    "server_error",
				Message: "boom",
			},
			want:             messages.StopReasonError,
			wantErrorMessage: "server_error: boom",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			adapter := NewOpenAIResponsesAdapter("gpt-5")
			state := streaming.NewStreamState()

			if err := adapter.ProcessChunk(tt.event, state); err != nil {
				t.Fatalf("ProcessChunk returned error: %v", err)
			}
			if got := state.StopReason; got != tt.want {
				t.Fatalf("stop reason = %q, want %q", got, tt.want)
			}

			msg := &messages.ChatMessage{}
			adapter.EnrichFinalMessage(msg, state)
			if tt.wantErrorMessage == "" {
				if msg.IsError() {
					t.Fatalf("expected final message to have no error metadata")
				}
				return
			}
			if !msg.IsError() {
				t.Fatalf("expected final message to be marked as error")
			}
			if got := msg.GetError().Error(); got != tt.wantErrorMessage {
				t.Fatalf("error message = %q, want %q", got, tt.wantErrorMessage)
			}
		})
	}
}

// TestOpenAIResponsesAdapterCapturesReasoningItems covers the capture half of
// the reasoning replay: output_item.added arrives before the model has produced
// any encrypted state, so the later .done event for the same id must win.
func TestOpenAIResponsesAdapterCapturesReasoningItems(t *testing.T) {
	adapter := NewOpenAIResponsesAdapter("gpt-5")
	state := streaming.NewStreamState()

	events := []*openai.ResponseStreamEvent{
		{
			Type:        "response.output_item.added",
			OutputIndex: 0,
			Item:        &openai.ResponseOutputItem{Type: "reasoning", ID: "rs_1"},
		},
		{
			Type:        "response.output_item.done",
			OutputIndex: 0,
			Item: &openai.ResponseOutputItem{
				Type:             "reasoning",
				ID:               "rs_1",
				EncryptedContent: "gAAAAA-payload",
				Summary: []openai.ResponseReasoningSummary{
					{Type: "summary_text", Text: "checking the weather"},
				},
			},
		},
	}
	for _, event := range events {
		if err := adapter.ProcessChunk(event, state); err != nil {
			t.Fatalf("ProcessChunk(%s): %v", event.Type, err)
		}
	}

	msg := &messages.ChatMessage{Role: messages.MessageRoleAssistant}
	adapter.EnrichFinalMessage(msg, state)

	items, ok := msg.Metadata[ResponsesReasoningItemsKey].([]map[string]any)
	if !ok {
		t.Fatalf("reasoning items metadata = %#v, want []map[string]any", msg.Metadata[ResponsesReasoningItemsKey])
	}
	if len(items) != 1 {
		t.Fatalf("item count = %d, want 1 (added and done share an id)", len(items))
	}
	if got := items[0]["encrypted_content"]; got != "gAAAAA-payload" {
		t.Fatalf("encrypted_content = %v, want the payload from the .done event", got)
	}
	if got := msg.Metadata[ResponsesReasoningModelKey]; got != "gpt-5" {
		t.Fatalf("reasoning model = %v, want gpt-5", got)
	}
}

// TestOpenAIResponsesAdapterHarvestsReasoningFromTerminalResponse covers models
// that only attach encrypted_content to the final response object.
func TestOpenAIResponsesAdapterHarvestsReasoningFromTerminalResponse(t *testing.T) {
	adapter := NewOpenAIResponsesAdapter("gpt-5")
	state := streaming.NewStreamState()

	event := &openai.ResponseStreamEvent{
		Type: "response.completed",
		Response: &openai.Response{
			Status: openai.ResponseStatusCompleted,
			Output: []openai.ResponseOutputItem{
				{Type: "reasoning", ID: "rs_1", EncryptedContent: "gAAAAA-payload"},
				{Type: "message", ID: "msg_1"},
			},
		},
	}
	if err := adapter.ProcessChunk(event, state); err != nil {
		t.Fatalf("ProcessChunk: %v", err)
	}

	msg := &messages.ChatMessage{Role: messages.MessageRoleAssistant}
	adapter.EnrichFinalMessage(msg, state)

	items, _ := msg.Metadata[ResponsesReasoningItemsKey].([]map[string]any)
	if len(items) != 1 || items[0]["encrypted_content"] != "gAAAAA-payload" {
		t.Fatalf("reasoning items = %#v, want one entry carrying the encrypted payload", items)
	}
}
