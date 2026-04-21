package llm

import (
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/anthropics/anthropic-sdk-go"
)

// TestAnthropicBuildRequestParams_ModelFamilyBehavior verifies that buildRequestParams
// branches correctly on Opus 4.7 (no temperature), 4.6+ family (adaptive thinking +
// effort), and legacy models (enabled/budget_tokens).
func TestAnthropicBuildRequestParams_ModelFamilyBehavior(t *testing.T) {
	tests := []struct {
		name         string
		model        string
		effort       ThinkingEffort
		wantTemp     bool
		wantAdaptive bool
		wantEnabled  bool
		wantBudget   int64
		wantEffort   anthropic.OutputConfigEffort
	}{
		{
			name:         "opus_4_7_no_thinking",
			model:        "claude-opus-4-7",
			effort:       ThinkingOff,
			wantTemp:     false,
			wantAdaptive: false,
			wantEnabled:  false,
		},
		{
			name:         "opus_4_7_low",
			model:        "claude-opus-4-7",
			effort:       ThinkingLow,
			wantTemp:     false,
			wantAdaptive: true,
			wantEffort:   anthropic.OutputConfigEffortLow,
		},
		{
			name:         "opus_4_7_high",
			model:        "claude-opus-4-7",
			effort:       ThinkingHigh,
			wantTemp:     false,
			wantAdaptive: true,
			wantEffort:   anthropic.OutputConfigEffortHigh,
		},
		{
			name:         "opus_4_7_dated_variant",
			model:        "claude-opus-4-7-20260101",
			effort:       ThinkingMedium,
			wantTemp:     false,
			wantAdaptive: true,
			wantEffort:   anthropic.OutputConfigEffortMedium,
		},
		{
			name:         "sonnet_4_6_medium",
			model:        "claude-sonnet-4-6",
			effort:       ThinkingMedium,
			wantTemp:     true,
			wantAdaptive: true,
			wantEffort:   anthropic.OutputConfigEffortMedium,
		},
		{
			name:         "opus_4_6_low",
			model:        "claude-opus-4-6",
			effort:       ThinkingLow,
			wantTemp:     true,
			wantAdaptive: true,
			wantEffort:   anthropic.OutputConfigEffortLow,
		},
		{
			name:         "sonnet_4_5_legacy_low",
			model:        "claude-sonnet-4-5-20250929",
			effort:       ThinkingLow,
			wantTemp:     true,
			wantEnabled:  true,
			wantBudget:   thinkingBudgetLow,
		},
		{
			name:         "sonnet_4_5_legacy_no_thinking",
			model:        "claude-sonnet-4-5-20250929",
			effort:       ThinkingOff,
			wantTemp:     true,
			wantAdaptive: false,
			wantEnabled:  false,
		},
	}

	client := NewAnthropicClient("")
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			params := client.buildRequestParams(&CompletionRequest{
				Model:          tc.model,
				MaxTokens:      1024,
				Temperature:    Float32Ptr(1.0),
				ThinkingEffort: tc.effort,
				Messages: []messages.ChatMessage{
					{Role: messages.MessageRoleUser, Content: "hi"},
				},
			})

			if got := params.Temperature.Valid(); got != tc.wantTemp {
				t.Errorf("Temperature.Valid() = %v, want %v", got, tc.wantTemp)
			}

			gotAdaptive := params.Thinking.OfAdaptive != nil
			if gotAdaptive != tc.wantAdaptive {
				t.Errorf("Thinking.OfAdaptive set = %v, want %v", gotAdaptive, tc.wantAdaptive)
			}
			if gotAdaptive {
				if got := params.Thinking.OfAdaptive.Display; got != anthropic.ThinkingConfigAdaptiveDisplaySummarized {
					t.Errorf("adaptive Display = %q, want %q", got, anthropic.ThinkingConfigAdaptiveDisplaySummarized)
				}
			}

			gotEnabled := params.Thinking.OfEnabled != nil
			if gotEnabled != tc.wantEnabled {
				t.Errorf("Thinking.OfEnabled set = %v, want %v", gotEnabled, tc.wantEnabled)
			}
			if gotEnabled {
				if got := params.Thinking.OfEnabled.BudgetTokens; got != tc.wantBudget {
					t.Errorf("budget_tokens = %d, want %d", got, tc.wantBudget)
				}
			}

			if got := params.OutputConfig.Effort; got != tc.wantEffort {
				t.Errorf("OutputConfig.Effort = %q, want %q", got, tc.wantEffort)
			}
		})
	}
}

func TestAnthropicCapabilityPredicates(t *testing.T) {
	adaptive := []string{
		"claude-opus-4-6",
		"claude-opus-4-7",
		"claude-opus-4-7-20260101",
		"claude-sonnet-4-6",
	}
	for _, m := range adaptive {
		if !supportsAdaptiveThinking(m) {
			t.Errorf("supportsAdaptiveThinking(%q) = false, want true", m)
		}
	}

	legacy := []string{
		"claude-sonnet-4-5-20250929",
		"claude-opus-4-5",
		"claude-opus-4-1",
		"claude-3-5-sonnet-20240620",
	}
	for _, m := range legacy {
		if supportsAdaptiveThinking(m) {
			t.Errorf("supportsAdaptiveThinking(%q) = true, want false", m)
		}
	}

	if !rejectsSamplingParams("claude-opus-4-7") {
		t.Errorf("rejectsSamplingParams(claude-opus-4-7) = false, want true")
	}
	if !rejectsSamplingParams("claude-opus-4-7-20260101") {
		t.Errorf("rejectsSamplingParams(dated opus-4-7) = false, want true")
	}
	for _, m := range []string{"claude-opus-4-6", "claude-sonnet-4-6", "claude-sonnet-4-5-20250929"} {
		if rejectsSamplingParams(m) {
			t.Errorf("rejectsSamplingParams(%q) = true, want false", m)
		}
	}
}

// TestAnthropicToolChoiceWithThinking verifies that when thinking is enabled,
// buildRequestParams does NOT force tool_choice=any — Anthropic rejects the
// combination with "Thinking may not be enabled when tool_choice forces tool use".
func TestAnthropicToolChoiceWithThinking(t *testing.T) {
	client := NewAnthropicClient("")
	schema := &Schema{
		Raw: map[string]any{
			"type":       "object",
			"properties": map[string]any{"answer": map[string]any{"type": "string"}},
			"required":   []any{"answer"},
		},
	}

	tests := []struct {
		name       string
		effort     ThinkingEffort
		wantForced bool
	}{
		{"no_thinking_forces_tool_choice", ThinkingOff, true},
		{"thinking_low_skips_force", ThinkingLow, false},
		{"thinking_medium_skips_force", ThinkingMedium, false},
		{"thinking_high_skips_force", ThinkingHigh, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			params := client.buildRequestParams(&CompletionRequest{
				Model:          "claude-haiku-4-5",
				MaxTokens:      1024,
				ResponseSchema: schema,
				ThinkingEffort: tc.effort,
				Messages: []messages.ChatMessage{
					{Role: messages.MessageRoleUser, Content: "hi"},
				},
			})
			gotForced := params.ToolChoice.OfAny != nil
			if gotForced != tc.wantForced {
				t.Errorf("tool_choice forced = %v, want %v", gotForced, tc.wantForced)
			}
		})
	}
}
