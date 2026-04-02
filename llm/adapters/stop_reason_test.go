package adapters

import (
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/anthropics/anthropic-sdk-go"
	ai "github.com/sashabaranov/go-openai"
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
		input ai.FinishReason
		want  messages.StopReason
	}{
		{ai.FinishReasonStop, messages.StopReasonEndTurn},
		{ai.FinishReasonToolCalls, messages.StopReasonToolUse},
		{ai.FinishReasonFunctionCall, messages.StopReasonToolUse},
		{ai.FinishReasonLength, messages.StopReasonMaxTokens},
		{ai.FinishReasonContentFilter, messages.StopReasonContentFilter},
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
