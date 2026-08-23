package llm

import (
	"testing"

	"github.com/alexschlessinger/pollytool/llm/anthropic"
	"github.com/alexschlessinger/pollytool/messages"
)

// TestAnthropicBuildRequestParams_ModelFamilyBehavior verifies that buildRequestParams
// branches correctly on Opus 4.7 (no temperature), 4.6+ family (adaptive thinking +
// effort), and legacy models (enabled/budget_tokens).
func TestAnthropicBuildRequestParams_ModelFamilyBehavior(t *testing.T) {
	tests := []struct {
		name         string
		model        string
		effort       ThinkingEffort
		maxTokens    int // 0 -> defaults to 1024
		wantTemp     bool
		wantAdaptive bool
		wantEnabled  bool
		wantBudget   int64
		wantEffort   anthropic.Effort
	}{
		{
			name:         "opus_4_7_no_thinking",
			model:        "claude-opus-4-7",
			effort:       EffortOff(),
			wantTemp:     false,
			wantAdaptive: false,
			wantEnabled:  false,
		},
		{
			name:         "opus_4_7_low",
			model:        "claude-opus-4-7",
			effort:       EffortLevel(LevelLow),
			wantTemp:     false,
			wantAdaptive: true,
			wantEffort:   anthropic.EffortLow,
		},
		{
			name:         "opus_4_7_high",
			model:        "claude-opus-4-7",
			effort:       EffortLevel(LevelHigh),
			wantTemp:     false,
			wantAdaptive: true,
			wantEffort:   anthropic.EffortHigh,
		},
		{
			// minimal has no Anthropic equivalent and clamps up to low.
			name:         "opus_4_7_minimal_clamps_to_low",
			model:        "claude-opus-4-7",
			effort:       EffortLevel(LevelMinimal),
			wantTemp:     false,
			wantAdaptive: true,
			wantEffort:   anthropic.EffortLow,
		},
		{
			name:         "opus_4_7_xhigh",
			model:        "claude-opus-4-7",
			effort:       EffortLevel(LevelXHigh),
			wantTemp:     false,
			wantAdaptive: true,
			wantEffort:   anthropic.EffortXHigh,
		},
		{
			name:         "opus_4_7_max",
			model:        "claude-opus-4-7",
			effort:       EffortLevel(LevelMax),
			wantTemp:     false,
			wantAdaptive: true,
			wantEffort:   anthropic.EffortMax,
		},
		{
			// Dynamic -> adaptive thinking with NO explicit effort (model decides).
			name:         "opus_4_7_dynamic_has_no_effort",
			model:        "claude-opus-4-7",
			effort:       EffortDynamic(),
			wantTemp:     false,
			wantAdaptive: true,
			wantEffort:   "", // OutputConfig.Effort left unset
		},
		{
			// A raw budget on an adaptive model reduces to its nearest level.
			name:         "opus_4_7_budget_maps_to_level",
			model:        "claude-opus-4-7",
			effort:       EffortBudget(70000), // above the max threshold (65536) -> max
			wantTemp:     false,
			wantAdaptive: true,
			wantEffort:   anthropic.EffortMax,
		},
		{
			name:         "opus_4_7_dated_variant",
			model:        "claude-opus-4-7-20260101",
			effort:       EffortLevel(LevelMedium),
			wantTemp:     false,
			wantAdaptive: true,
			wantEffort:   anthropic.EffortMedium,
		},
		{
			// Regression: opus-4-8 must use adaptive thinking, not legacy
			// enabled/budget_tokens, which 400s ("hi" reproduced this).
			name:         "opus_4_8_high",
			model:        "claude-opus-4-8",
			effort:       EffortLevel(LevelHigh),
			wantTemp:     false,
			wantAdaptive: true,
			wantEffort:   anthropic.EffortHigh,
		},
		{
			name:         "opus_4_8_dated_variant",
			model:        "claude-opus-4-8-20260601",
			effort:       EffortLevel(LevelMedium),
			wantTemp:     false,
			wantAdaptive: true,
			wantEffort:   anthropic.EffortMedium,
		},
		{
			name:         "sonnet_4_6_medium",
			model:        "claude-sonnet-4-6",
			effort:       EffortLevel(LevelMedium),
			wantTemp:     true,
			wantAdaptive: true,
			wantEffort:   anthropic.EffortMedium,
		},
		{
			name:         "opus_4_6_low",
			model:        "claude-opus-4-6",
			effort:       EffortLevel(LevelLow),
			wantTemp:     true,
			wantAdaptive: true,
			wantEffort:   anthropic.EffortLow,
		},
		{
			name:        "sonnet_4_5_legacy_low",
			model:       "claude-sonnet-4-5-20250929",
			effort:      EffortLevel(LevelLow),
			maxTokens:   16000,
			wantTemp:    true,
			wantEnabled: true,
			wantBudget:  int64(levelBudgets[LevelLow]),
		},
		{
			// A raw budget passes through on legacy models...
			name:        "sonnet_4_5_legacy_raw_budget",
			model:       "claude-sonnet-4-5-20250929",
			effort:      EffortBudget(6000),
			maxTokens:   16000,
			wantTemp:    true,
			wantEnabled: true,
			wantBudget:  6000,
		},
		{
			// ...but is clamped to strictly less than max_tokens (API 400s otherwise).
			name:        "sonnet_4_5_legacy_budget_clamped_to_maxtokens",
			model:       "claude-sonnet-4-5-20250929",
			effort:      EffortBudget(50000),
			maxTokens:   16000,
			wantTemp:    true,
			wantEnabled: true,
			wantBudget:  15999,
		},
		{
			// Dynamic has no legacy equivalent: fall back to the medium budget.
			name:        "sonnet_4_5_legacy_dynamic_falls_back_to_medium",
			model:       "claude-sonnet-4-5-20250929",
			effort:      EffortDynamic(),
			maxTokens:   16000,
			wantTemp:    true,
			wantEnabled: true,
			wantBudget:  int64(levelBudgets[LevelMedium]),
		},
		{
			name:         "sonnet_4_5_legacy_no_thinking",
			model:        "claude-sonnet-4-5-20250929",
			effort:       EffortOff(),
			wantTemp:     true,
			wantAdaptive: false,
			wantEnabled:  false,
		},
	}

	client := NewAnthropicClient("")
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			maxTokens := tc.maxTokens
			if maxTokens == 0 {
				maxTokens = 1024
			}
			params := client.buildRequestParams(&CompletionRequest{
				Model:          tc.model,
				MaxTokens:      maxTokens,
				Temperature:    Float32Ptr(1.0),
				ThinkingEffort: tc.effort,
				Messages: []messages.ChatMessage{
					{Role: messages.MessageRoleUser, Content: "hi"},
				},
			})

			if got := params.Temperature != nil; got != tc.wantTemp {
				t.Errorf("Temperature set = %v, want %v", got, tc.wantTemp)
			}

			gotAdaptive := params.Thinking != nil && params.Thinking.Type == anthropic.ThinkingTypeAdaptive
			if gotAdaptive != tc.wantAdaptive {
				t.Errorf("thinking type adaptive = %v, want %v", gotAdaptive, tc.wantAdaptive)
			}
			if gotAdaptive {
				if got := params.Thinking.Display; got != anthropic.DisplaySummarized {
					t.Errorf("adaptive Display = %q, want %q", got, anthropic.DisplaySummarized)
				}
			}

			gotEnabled := params.Thinking != nil && params.Thinking.Type == anthropic.ThinkingTypeEnabled
			if gotEnabled != tc.wantEnabled {
				t.Errorf("thinking type enabled = %v, want %v", gotEnabled, tc.wantEnabled)
			}
			if gotEnabled {
				if got := params.Thinking.BudgetTokens; got != tc.wantBudget {
					t.Errorf("budget_tokens = %d, want %d", got, tc.wantBudget)
				}
			}

			var gotEffort anthropic.Effort
			if params.OutputConfig != nil {
				gotEffort = params.OutputConfig.Effort
			}
			if gotEffort != tc.wantEffort {
				t.Errorf("output_config effort = %q, want %q", gotEffort, tc.wantEffort)
			}
		})
	}
}

func TestAnthropicCapabilityPredicates(t *testing.T) {
	adaptive := []string{
		"claude-opus-4-6",
		"claude-opus-4-7",
		"claude-opus-4-7-20260101",
		"claude-opus-4-8",
		"claude-opus-4-8-20260601",
		"claude-sonnet-4-6",
		"claude-opus-5",
		"claude-sonnet-5",
		"claude-fable-5",
		"claude-mythos-5",
		"claude-opus-6", // unknown future models default to adaptive
	}
	for _, m := range adaptive {
		if !supportsAdaptiveThinking(m) {
			t.Errorf("supportsAdaptiveThinking(%q) = false, want true", m)
		}
	}

	legacy := []string{
		"claude-sonnet-4-5-20250929",
		"claude-sonnet-4-20250514",
		"claude-opus-4-5",
		"claude-opus-4-1",
		"claude-opus-4-20250514",
		"claude-haiku-4-5",
		"claude-haiku-4-5-20251001",
		"claude-3-5-sonnet-20240620",
		"claude-mythos-preview",
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
	if !rejectsSamplingParams("claude-opus-4-8") {
		t.Errorf("rejectsSamplingParams(claude-opus-4-8) = false, want true")
	}
	if !rejectsSamplingParams("claude-opus-4-8-20260601") {
		t.Errorf("rejectsSamplingParams(dated opus-4-8) = false, want true")
	}
	for _, m := range []string{"claude-opus-5", "claude-sonnet-5", "claude-fable-5", "claude-mythos-5", "claude-opus-6"} {
		if !rejectsSamplingParams(m) {
			t.Errorf("rejectsSamplingParams(%q) = false, want true", m)
		}
	}
	for _, m := range []string{"claude-opus-4-6", "claude-sonnet-4-6", "claude-sonnet-4-5-20250929", "claude-haiku-4-5"} {
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
		{"no_thinking_forces_tool_choice", EffortOff(), true},
		{"thinking_low_skips_force", EffortLevel(LevelLow), false},
		{"thinking_medium_skips_force", EffortLevel(LevelMedium), false},
		{"thinking_high_skips_force", EffortLevel(LevelHigh), false},
		{"thinking_dynamic_skips_force", EffortDynamic(), false},
		{"thinking_budget_skips_force", EffortBudget(12000), false},
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
			gotForced := params.ToolChoice != nil && params.ToolChoice.Type == "any"
			if gotForced != tc.wantForced {
				t.Errorf("tool_choice forced = %v, want %v", gotForced, tc.wantForced)
			}
		})
	}
}
