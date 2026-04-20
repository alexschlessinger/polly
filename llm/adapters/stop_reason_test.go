package adapters

import (
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/openai/openai-go/v3/responses"
	"google.golang.org/genai"
)

func TestMapAnthropicStopReason(t *testing.T) {
	tests := []struct {
		input anthropic.StopReason
		want  messages.StopReason
	}{
		{"end_turn", messages.StopReasonEndTurn},
		{"tool_use", messages.StopReasonToolUse},
		{"max_tokens", messages.StopReasonMaxTokens},
		{"refusal", messages.StopReasonContentFilter},
		{"stop_sequence", messages.StopReasonEndTurn},
		{"unknown_value", messages.StopReasonEndTurn},
	}

	for _, tt := range tests {
		t.Run(string(tt.input), func(t *testing.T) {
			got := MapAnthropicStopReason(tt.input)
			if got != tt.want {
				t.Errorf("MapAnthropicStopReason(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestMapOpenAIFinishReason(t *testing.T) {
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
			got := MapOpenAIFinishReason(tt.input)
			if got != tt.want {
				t.Errorf("MapOpenAIFinishReason(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestMapResponsesStopReason(t *testing.T) {
	tests := []struct {
		name             string
		status           responses.ResponseStatus
		incompleteReason string
		hasToolCalls     bool
		want             messages.StopReason
	}{
		{
			name:         "completed_end_turn",
			status:       responses.ResponseStatusCompleted,
			hasToolCalls: false,
			want:         messages.StopReasonEndTurn,
		},
		{
			name:         "completed_tool_use",
			status:       responses.ResponseStatusCompleted,
			hasToolCalls: true,
			want:         messages.StopReasonToolUse,
		},
		{
			name:             "incomplete_max_output_tokens",
			status:           responses.ResponseStatusIncomplete,
			incompleteReason: "max_output_tokens",
			want:             messages.StopReasonMaxTokens,
		},
		{
			name:             "incomplete_content_filter",
			status:           responses.ResponseStatusIncomplete,
			incompleteReason: "content_filter",
			want:             messages.StopReasonContentFilter,
		},
		{
			name:   "failed",
			status: responses.ResponseStatusFailed,
			want:   messages.StopReasonError,
		},
		{
			name:   "cancelled",
			status: responses.ResponseStatusCancelled,
			want:   messages.StopReasonError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MapResponsesStopReason(tt.status, tt.incompleteReason, tt.hasToolCalls)
			if got != tt.want {
				t.Errorf("MapResponsesStopReason(%q, %q, %t) = %q, want %q", tt.status, tt.incompleteReason, tt.hasToolCalls, got, tt.want)
			}
		})
	}
}

func TestMapGeminiFinishReason(t *testing.T) {
	tests := []struct {
		input genai.FinishReason
		want  messages.StopReason
	}{
		{genai.FinishReasonStop, messages.StopReasonEndTurn},
		{genai.FinishReasonMaxTokens, messages.StopReasonMaxTokens},
		{genai.FinishReasonSafety, messages.StopReasonContentFilter},
		{genai.FinishReasonRecitation, messages.StopReasonContentFilter},
		{genai.FinishReasonBlocklist, messages.StopReasonContentFilter},
		{genai.FinishReasonProhibitedContent, messages.StopReasonContentFilter},
		{genai.FinishReasonSPII, messages.StopReasonContentFilter},
		{genai.FinishReasonImageSafety, messages.StopReasonContentFilter},
		{genai.FinishReasonImageProhibitedContent, messages.StopReasonContentFilter},
		{genai.FinishReasonMalformedFunctionCall, messages.StopReasonError},
		{"unknown", messages.StopReasonEndTurn},
	}

	for _, tt := range tests {
		t.Run(string(tt.input), func(t *testing.T) {
			got := mapGeminiFinishReason(tt.input)
			if got != tt.want {
				t.Errorf("mapGeminiFinishReason(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
