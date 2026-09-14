package openai

import (
	"testing"

	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestMapChatFinishReason(t *testing.T) {
	tests := []struct {
		input string
		want  messages.StopReason
	}{
		{"stop", messages.StopReasonEndTurn},
		{"tool_calls", messages.StopReasonToolUse},
		{"function_call", messages.StopReasonToolUse},
		{"length", messages.StopReasonMaxTokens},
		{"content_filter", messages.StopReasonContentFilter},
		{"unknown", messages.StopReasonEndTurn},
	}

	for _, tt := range tests {
		t.Run(string(tt.input), func(t *testing.T) {
			got := mapChatFinishReason(tt.input)
			if got != tt.want {
				t.Errorf("mapChatFinishReason(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestMapResponsesStopReason(t *testing.T) {
	tests := []struct {
		name             string
		status           ResponseStatus
		incompleteReason string
		hasToolCalls     bool
		want             messages.StopReason
	}{
		{
			name:         "completed_end_turn",
			status:       ResponseStatusCompleted,
			hasToolCalls: false,
			want:         messages.StopReasonEndTurn,
		},
		{
			name:         "completed_with_tool_calls_promoted_by_core",
			status:       ResponseStatusCompleted,
			hasToolCalls: true,
			want:         messages.StopReasonEndTurn,
		},
		{
			name:             "incomplete_max_output_tokens",
			status:           ResponseStatusIncomplete,
			incompleteReason: "max_output_tokens",
			want:             messages.StopReasonMaxTokens,
		},
		{
			name:             "incomplete_content_filter",
			status:           ResponseStatusIncomplete,
			incompleteReason: "content_filter",
			want:             messages.StopReasonContentFilter,
		},
		{
			name:   "failed",
			status: ResponseStatusFailed,
			want:   messages.StopReasonError,
		},
		{
			name:   "cancelled",
			status: ResponseStatusCancelled,
			want:   messages.StopReasonError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mapResponsesStopReason(tt.status, tt.incompleteReason, tt.hasToolCalls)
			if got != tt.want {
				t.Errorf("mapResponsesStopReason(%q, %q, %t) = %q, want %q", tt.status, tt.incompleteReason, tt.hasToolCalls, got, tt.want)
			}
		})
	}
}

// TestAdaptersParsePromptCacheUsage: both APIs report cached and cache-write
// prompt tokens as provider-supplied; omitted details are never inferred.
func TestAdaptersParsePromptCacheUsage(t *testing.T) {
	int64Ptr := func(value int64) *int64 { return &value }
	t.Run("chat completions", func(t *testing.T) {
		state := streaming.NewStreamState()
		adapter := NewChatAdapter()
		err := adapter.ProcessChunk(&ChatCompletionChunk{Usage: &ChatUsage{
			PromptTokens: 20, CompletionTokens: 3,
			PromptTokensDetails: &PromptTokenDetails{CachedTokens: int64Ptr(12), CacheWriteTokens: int64Ptr(4)},
		}}, state)
		if err != nil {
			t.Fatal(err)
		}
		assertCacheUsage(t, state, 20, 3, 12, 4)
	})

	t.Run("responses", func(t *testing.T) {
		state := streaming.NewStreamState()
		adapter := NewResponsesAdapter("gpt-5")
		err := adapter.ProcessChunk(&ResponseStreamEvent{
			Type: "response.completed",
			Response: &Response{Status: ResponseStatusCompleted, Usage: &ResponseUsage{
				InputTokens: 30, OutputTokens: 5,
				InputTokensDetails: &PromptTokenDetails{CachedTokens: int64Ptr(18), CacheWriteTokens: int64Ptr(6)},
			}},
		}, state)
		if err != nil {
			t.Fatal(err)
		}
		assertCacheUsage(t, state, 30, 5, 18, 6)
	})

	t.Run("omitted details are not inferred", func(t *testing.T) {
		state := streaming.NewStreamState()
		adapter := NewChatAdapter()
		if err := adapter.ProcessChunk(&ChatCompletionChunk{Usage: &ChatUsage{
			PromptTokens: 20, PromptTokensDetails: &PromptTokenDetails{},
		}}, state); err != nil {
			t.Fatal(err)
		}
		if state.HasPromptCacheUsage() {
			t.Fatal("adapter inferred cache usage from ordinary prompt tokens")
		}
	})
}

func assertCacheUsage(t *testing.T, state *streaming.StreamState, input, output, read, write int) {
	t.Helper()
	if state.GetInputTokens() != input || state.GetOutputTokens() != output {
		t.Fatalf("token usage = %d/%d, want %d/%d", state.GetInputTokens(), state.GetOutputTokens(), input, output)
	}
	if !state.HasPromptCacheUsage() {
		t.Fatal("cache usage was not marked as provider-reported")
	}
	if state.GetCacheReadInputTokens() != read || state.GetCacheWriteInputTokens() != write {
		t.Fatalf("cache usage = %d/%d, want %d/%d", state.GetCacheReadInputTokens(), state.GetCacheWriteInputTokens(), read, write)
	}
}
